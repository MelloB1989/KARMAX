package mesh

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Transport carries envelopes over HTTP.
//
// HTTP rather than a persistent socket because a connection here is a
// RELATIONSHIP, not a TCP session: two instances are connected when each has
// the other's key and has agreed to talk, which survives restarts, address
// changes and either side being offline for a week. A websocket would make the
// relationship only as durable as the process.
type Transport struct {
	id     *Identity
	log    *zap.Logger
	client *http.Client
}

func NewTransport(id *Identity, log *zap.Logger) *Transport {
	return &Transport{
		id:  id,
		log: log,
		client: &http.Client{
			Timeout: 20 * time.Second,
			// Redirects are not followed. A peer that can redirect us can point
			// this instance's authenticated POST at a third party.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// post delivers one envelope to an endpoint.
func (t *Transport) post(ctx context.Context, endpoint string, e *Envelope) error {
	if err := checkEndpoint(endpoint); err != nil {
		return err
	}
	blob, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(blob))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "karmax-mesh/1")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mesh: could not reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mesh: %s refused the envelope: %s %s",
			endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// checkEndpoint refuses addresses an instance should never be made to call.
//
// A peer supplies its own endpoint, so without this an attacker could point
// this instance at 169.254.169.254 and use its authenticated identity to probe
// the host's cloud metadata — server-side request forgery with a signature
// attached. Loopback stays allowed because two instances on one machine is the
// normal way to test the mesh.
func checkEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("mesh: endpoint is not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("mesh: endpoint scheme %q is not allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("mesh: endpoint has no host")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil // a name; resolution is the resolver's business
	}
	if ip.IsLoopback() {
		return nil
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		// 169.254.0.0/16 — where cloud metadata services live.
		return fmt.Errorf("mesh: refusing to send to the link-local address %s", ip)
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return fmt.Errorf("mesh: refusing to send to %s", ip)
	}
	return nil
}

// Connect asks another instance to accept a connection.
//
// The request is recorded locally as pending too: connecting is mutual, and
// this instance has not decided to trust them either until they accept and the
// operator confirms.
func (n *Node) Connect(ctx context.Context, endpoint, note string) (*store.MeshPeer, error) {
	if err := checkEndpoint(endpoint); err != nil {
		return nil, err
	}
	// Who is there? Fetched before anything is sent so the operator can be
	// shown a fingerprint to verify out of band.
	remote, err := n.tp.hello(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if remote.ID == n.id.ID() {
		return nil, fmt.Errorf("mesh: that endpoint is this instance")
	}

	peer := store.MeshPeer{
		ID: remote.ID, Name: sanitizeName(remote.Name), BoxPub: remote.BoxPub,
		Endpoint: endpoint, State: store.PeerPending,
		Fingerprint: fingerprintOf(remote.ID), Direction: "out",
		Note: truncate(note, 200),
	}
	if err := n.store.UpsertMeshPeer(peer); err != nil {
		return nil, err
	}

	body := peerRequestBody{
		Name: n.id.Name, BoxPub: base64.RawURLEncoding.EncodeToString(n.id.BoxPub[:]),
		Endpoint: n.cfg.Endpoint, Note: truncate(note, 200),
		Scopes: defaultScopes(),
	}
	// A connection request is signed but NOT encrypted: there is no shared
	// secret until each side has the other's box key, and this message is what
	// carries ours. It contains only what is about to be public anyway.
	e, err := n.sealPlain(KindPeerRequest, remote.ID, body)
	if err != nil {
		return nil, err
	}
	if err := n.tp.post(ctx, endpoint, e); err != nil {
		return nil, err
	}
	return &peer, nil
}

// Accept admits a pending peer and tells it so.
//
// The operator's decision, not the protocol's: nothing in an inbound request
// can cause this to be called.
func (n *Node) Accept(ctx context.Context, peerID string, scopes []string) error {
	peer, err := n.store.MeshPeerByID(peerID)
	if err != nil {
		return err
	}
	if len(scopes) == 0 {
		scopes = defaultScopes()
	}
	if err := n.store.SetMeshPeerState(peerID, store.PeerAccepted, scopes); err != nil {
		return err
	}
	if peer.Endpoint == "" {
		// Nothing to notify; they will discover it when they next reach us.
		return nil
	}
	e, err := n.sealPlain(KindPeerAccept, peerID, peerRequestBody{
		Name:     n.id.Name,
		BoxPub:   base64.RawURLEncoding.EncodeToString(n.id.BoxPub[:]),
		Endpoint: n.cfg.Endpoint,
	})
	if err != nil {
		return err
	}
	return n.tp.post(ctx, peer.Endpoint, e)
}

// Block refuses a peer permanently.
func (n *Node) Block(peerID string) error {
	return n.store.SetMeshPeerState(peerID, store.PeerBlocked, nil)
}

// Revoke withdraws a previously accepted connection.
func (n *Node) Revoke(peerID string) error {
	return n.store.SetMeshPeerState(peerID, store.PeerRevoked, nil)
}

// Send delivers to one connected instance on this instance's own behalf, so it
// carries no delegation chain.
func (n *Node) Send(ctx context.Context, peerID string, kind Kind, body MessageBody) error {
	return n.send(ctx, peerID, kind, body, nil)
}

// SendOnBehalf delivers work this instance was asked to do, naming the chain it
// came through. Use it whenever an inbound request is the reason an outbound one
// exists — plain Send there tells the recipient the work originated here.
func (n *Node) SendOnBehalf(ctx context.Context, peerID string, kind Kind, body MessageBody, on Provenance) error {
	chain, err := on.extend(n.id, peerID)
	if err != nil {
		return err
	}
	return n.send(ctx, peerID, kind, body, chain)
}

func (n *Node) send(ctx context.Context, peerID string, kind Kind, body MessageBody, chain []Hop) error {
	peer, err := n.store.MeshPeerByID(peerID)
	if err != nil {
		return err
	}
	if peer.State != store.PeerAccepted {
		return fmt.Errorf("mesh: %s is not a connected instance (state: %s)", peer.Name, peer.State)
	}
	if peer.Endpoint == "" {
		return fmt.Errorf("mesh: no endpoint on record for %s", peer.Name)
	}
	box, err := ParseBoxKey(peer.BoxPub)
	if err != nil {
		return fmt.Errorf("mesh: no usable encryption key for %s: %w", peer.Name, err)
	}
	e, err := Seal(n.id, kind, peer.ID, box, body)
	if err != nil {
		return err
	}
	if len(chain) > 0 {
		// Re-signed so the chain is inside the signature, as SendAsOrg does.
		e.Chain = chain
		e.Sig = base64.RawURLEncoding.EncodeToString(n.id.Sign(e.signingBytes()))
	}
	if err := n.tp.post(ctx, peer.Endpoint, e); err != nil {
		return err
	}
	_ = n.store.RecordMeshMessage(store.MeshMessage{
		ID: e.ID, PeerID: peer.ID, PeerName: peer.Name, Kind: string(kind),
		Direction: "out", Body: truncate(body.Subject+" "+body.Text, 4000),
		Origin: originOf(e),
	})
	return nil
}

// originOf is recorded only when the work traces back to somebody else.
func originOf(e *Envelope) string {
	if len(e.Chain) == 0 {
		return ""
	}
	return e.Origin()
}

// BroadcastResult reports what a fan-out achieved.
type BroadcastResult struct {
	Delivered int
	Failed    map[string]string // peer name -> why
}

// Broadcast sends to every connected instance.
//
// Concurrent but bounded, and partial failure is reported rather than swallowed:
// "sent to the team" when four of nine were unreachable is worse than an error,
// because the operator stops looking.
func (n *Node) Broadcast(ctx context.Context, body MessageBody) (*BroadcastResult, error) {
	peers, err := n.store.ListMeshPeers(store.PeerAccepted)
	if err != nil {
		return nil, err
	}
	res := &BroadcastResult{Failed: map[string]string{}}
	if len(peers) == 0 {
		return res, nil
	}

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		sem = make(chan struct{}, 8)
	)
	for _, p := range peers {
		if !hasScope(p.Scopes, ScopeBroadcast) && !hasScope(p.Scopes, ScopeMessage) {
			continue
		}
		wg.Add(1)
		go func(p store.MeshPeer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			err := n.Send(ctx, p.ID, KindBroadcast, body)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Failed[p.Name] = err.Error()
				return
			}
			res.Delivered++
		}(p)
	}
	wg.Wait()
	return res, nil
}

// SendAsOrg addresses a member instance under this org's authority, with no
// pairwise connection.
//
// This is the org's privilege, and it is bounded: it works only where the
// member's operator configured this org as trusted, only with an unexpired
// certificate naming that member, and only within the scopes that certificate
// grants. An org cannot mint authority over an instance that never opted in.
func (n *Node) SendAsOrg(ctx context.Context, member store.MeshPeer, cert *Certificate, kind Kind, body MessageBody) error {
	if !n.cfg.IsOrg {
		return fmt.Errorf("mesh: this instance is not an org node")
	}
	if cert == nil || cert.Subject != member.ID {
		return fmt.Errorf("mesh: the certificate does not name this member")
	}
	box, err := ParseBoxKey(member.BoxPub)
	if err != nil {
		return fmt.Errorf("mesh: no usable encryption key for %s: %w", member.Name, err)
	}
	e, err := Seal(n.id, kind, member.ID, box, body)
	if err != nil {
		return err
	}
	// Via and Cert are inside the signed bytes, so neither can be attached to
	// somebody else's envelope after the fact.
	e.Via = n.id.ID()
	e.Cert = cert
	e.Sig = base64.RawURLEncoding.EncodeToString(n.id.Sign(e.signingBytes()))

	if member.Endpoint == "" {
		return fmt.Errorf("mesh: no endpoint on record for %s", member.Name)
	}
	return n.tp.post(ctx, member.Endpoint, e)
}

// sealPlain builds a signed envelope whose body is readable — used only for the
// handshake, before a shared secret exists.
func (n *Node) sealPlain(kind Kind, to string, body any) (*Envelope, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := readRandom(nonce); err != nil {
		return nil, err
	}
	e := &Envelope{
		V: 1, ID: newID(), Kind: kind, From: n.id.ID(), To: to,
		TS: time.Now().Unix(), Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Body: base64.RawURLEncoding.EncodeToString(raw),
	}
	e.Sig = base64.RawURLEncoding.EncodeToString(n.id.Sign(e.signingBytes()))
	return e, nil
}

// hello asks an endpoint who it is, so a fingerprint can be shown before any
// trust decision is made.
func (t *Transport) hello(ctx context.Context, endpoint string) (*PublicIdentity, error) {
	u := strings.TrimSuffix(endpoint, "/") + "/hello"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mesh: could not reach %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mesh: %s answered %s", u, resp.Status)
	}
	var pub PublicIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&pub); err != nil {
		return nil, fmt.Errorf("mesh: %s did not return an identity: %w", u, err)
	}
	if _, err := DecodeKey(pub.ID); err != nil {
		return nil, fmt.Errorf("mesh: %s returned an invalid identity: %w", u, err)
	}
	if _, err := ParseBoxKey(pub.BoxPub); err != nil {
		return nil, fmt.Errorf("mesh: %s returned an invalid encryption key: %w", u, err)
	}
	// Recomputed locally rather than trusted: a peer that supplies its own
	// fingerprint could supply one matching a key it does not hold, and the
	// operator would verify the wrong thing.
	pub.Fingerprint = fingerprintOf(pub.ID)
	return &pub, nil
}

func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// readRandom is crypto/rand, named so the intent is obvious at the call site:
// a predictable nonce would make replay defence ineffective.
func readRandom(b []byte) (int, error) { return rand.Read(b) }

// Hello fetches another instance's public identity without connecting to it.
//
// Exported because the org path needs it: an org addresses members it has no
// pairwise connection with, so it has to learn their keys somehow, and asking
// them directly is better than a directory it would have to keep in sync.
func (n *Node) Hello(ctx context.Context, endpoint string) (*PublicIdentity, error) {
	if err := checkEndpoint(endpoint); err != nil {
		return nil, err
	}
	return n.tp.hello(ctx, endpoint)
}

// IssueFor mints a membership certificate for a member instance.
func (n *Node) IssueFor(subject string, scopes []string, ttl time.Duration) (*Certificate, error) {
	if !n.cfg.IsOrg {
		return nil, fmt.Errorf("mesh: this instance is not an org node (set KARMAX_MESH_IS_ORG=true)")
	}
	if _, err := DecodeKey(subject); err != nil {
		return nil, fmt.Errorf("mesh: %w", err)
	}
	name := n.cfg.OrgName
	if name == "" {
		name = n.id.Name
	}
	return IssueCertificate(n.id, name, subject, scopes, ttl), nil
}
