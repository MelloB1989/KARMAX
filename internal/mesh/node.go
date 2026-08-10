package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Node is this instance's presence on the mesh.
//
// It answers exactly one question on every inbound envelope, in a fixed order
// that must not be rearranged:
//
//  1. Is the signature valid?          — who sent this
//  2. Is it fresh and unseen?          — is it a replay
//  3. Is the sender allowed to say it? — authorisation
//
// Authentication before authorisation, and both before anything is acted on.
// Doing (3) first would mean looking up an attacker-supplied identity; doing
// (1) and then acting would mean obeying any stranger with a valid key.

// Scopes an accepted peer may hold. Admitting a peer is not the same as letting
// it read memory, which is why "message" is separate from everything else.
const (
	ScopeMessage   = "message"   // may send direct messages and broadcasts
	ScopeAsk       = "ask"       // may put questions to this instance's agent
	ScopeBroadcast = "broadcast" // may include this instance in a fan-out
)

// Config wires a node.
type Config struct {
	// Endpoint is where other instances reach this one, e.g.
	// https://karmax.example.com/mesh. Advertised in peer requests.
	Endpoint string
	// TrustedOrg is the org signing key this instance accepts certificates
	// from. Empty means it belongs to no org and every org envelope is refused.
	//
	// Set by the operator locally. It is never learned from the network:
	// accepting an org key because a message claimed it would make the whole
	// certificate mechanism decorative.
	TrustedOrg string
	// IsOrg marks this instance as an organisation node, able to issue
	// certificates and address members directly.
	IsOrg bool
	// OrgName labels this org in requests. Display only.
	OrgName string
	// AutoAcceptOrgMembers admits peers that present a valid certificate from
	// the trusted org without asking the operator. Off by default: colleagues
	// are still a decision, and an org compromise should not silently become
	// access to every member's instance.
	AutoAcceptOrgMembers bool
}

// Node is the mesh runtime.
type Node struct {
	id    *Identity
	cfg   Config
	store *store.Store
	log   *zap.Logger
	guard *ReplayGuard
	tp    *Transport

	// handlers are what the runtime does with an authorised message. Held
	// behind a mutex because they are installed after construction.
	//
	// The peer is who spoke; the Provenance is on whose behalf. A handler that
	// delegates onward must pass it to SendOnBehalf or the chain stops here.
	mu        sync.RWMutex
	onMessage func(peer store.MeshPeer, kind Kind, body MessageBody, prov Provenance)
	onAsk     func(ctx context.Context, peer store.MeshPeer, question string, prov Provenance) (string, error)
	onRequest func(peer store.MeshPeer)
	// onCert fires when an org certificate verifies, so the runtime can turn
	// its capability scopes into grants. The mesh does not know what a Broker
	// is, and should not.
	onCert func(cert *Certificate)
}

// MessageBody is the payload of a message, broadcast, ask or answer.
type MessageBody struct {
	Text string `json:"text,omitempty"`
	// Subject is a short label so a receiving loop can route without parsing
	// the text.
	Subject string `json:"subject,omitempty"`
	// ReplyTo carries the envelope id an answer responds to.
	ReplyTo string `json:"reply_to,omitempty"`
	// Meta is free-form structured data for loops that want it.
	Meta map[string]any `json:"meta,omitempty"`
}

// peerRequestBody is what an instance sends when asking to connect.
type peerRequestBody struct {
	Name     string   `json:"name"`
	BoxPub   string   `json:"box_pub"`
	Endpoint string   `json:"endpoint"`
	Note     string   `json:"note,omitempty"`
	Scopes   []string `json:"scopes,omitempty"` // what it would like; not what it gets
}

// New builds a node.
func New(id *Identity, cfg Config, s *store.Store, log *zap.Logger) *Node {
	n := &Node{id: id, cfg: cfg, store: s, log: log, guard: NewReplayGuard()}
	n.tp = NewTransport(id, log)
	return n
}

// Identity exposes this node's identity.
func (n *Node) Identity() *Identity { return n.id }

// Public is what this node publishes about itself.
func (n *Node) Public() PublicIdentity { return n.id.Public(n.cfg.Endpoint) }

// OnMessage installs the handler for authorised messages.
func (n *Node) OnMessage(fn func(store.MeshPeer, Kind, MessageBody, Provenance)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onMessage = fn
}

// OnAsk installs the handler that answers questions from peers holding ScopeAsk.
func (n *Node) OnAsk(fn func(context.Context, store.MeshPeer, string, Provenance) (string, error)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onAsk = fn
}

// OnCertificate installs the handler for a verified org certificate.
func (n *Node) OnCertificate(fn func(*Certificate)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onCert = fn
}

// OnPeerRequest installs the notifier for inbound connection requests, so the
// operator learns about a pending decision rather than finding it later.
func (n *Node) OnPeerRequest(fn func(store.MeshPeer)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.onRequest = fn
}

// Receive processes one inbound envelope. The single entry point for anything
// arriving from the network.
func (n *Node) Receive(ctx context.Context, e *Envelope) error {
	// 1. Authentication. Nothing about the envelope is believed before this,
	//    including who it says it is from.
	if err := VerifySignature(e); err != nil {
		return err
	}
	// 2. Freshness. A signature is valid forever, so without this a captured
	//    envelope can be replayed indefinitely and will verify every time.
	if err := n.guard.CheckFreshness(e); err != nil {
		return err
	}
	// 3. Addressing. A broadcast is addressed to "*"; anything else must name
	//    this instance, so an envelope meant for someone else is refused even
	//    if it arrives here correctly signed.
	if e.To != "*" && e.To != n.id.ID() {
		return fmt.Errorf("mesh: envelope is addressed to another instance")
	}

	// 4. Authorisation.
	peer, err := n.authorise(e)
	if err != nil {
		return err
	}
	// 5. Delegation. The sender may be permitted while its cargo is not.
	if err := n.checkDelegation(e); err != nil {
		return err
	}

	_ = n.store.TouchMeshPeer(peer.ID)
	return n.dispatch(ctx, e, peer)
}

// checkDelegation refuses laundering: an instance blocked here asks a peer that
// is accepted here, which delegates onward — signature valid, sender trusted,
// block bypassed by one hop, unless it applies to everyone in the chain.
//
// An unknown instance is NOT refused; the sender vouches for it, and delegation
// to strangers is the shape of the thing.
func (n *Node) checkDelegation(e *Envelope) error {
	if len(e.Chain) == 0 {
		return nil
	}
	for _, who := range e.participants() {
		peer, err := n.store.MeshPeerByID(who)
		if err != nil {
			continue
		}
		if peer.State == store.PeerBlocked || peer.State == store.PeerRevoked {
			// Uninformative on purpose, as a direct refusal is.
			return fmt.Errorf("mesh: refusing work delegated on behalf of an instance this one does not permit")
		}
	}
	return nil
}

// authorise decides whether this sender may be acted on, and returns the peer
// record to act as.
//
// Three ways in, in descending order of how much was checked:
//
//	a) an accepted peer                       — the normal path
//	b) an org instance with a valid cert      — no pairwise connection needed
//	c) a stranger sending peer.request ONLY   — which merely files a decision
func (n *Node) authorise(e *Envelope) (store.MeshPeer, error) {
	peer, err := n.store.MeshPeerByID(e.From)

	// (b) The org path. Checked BEFORE the peer state so an org can still reach
	// a member that has not separately connected to it — which is the whole
	// point — but only with a certificate this instance's operator configured
	// it to trust.
	if e.Via != "" {
		if err := n.verifyOrgAuthority(e); err != nil {
			return store.MeshPeer{}, err
		}
		if err == nil && peer != nil {
			// A blocked instance stays blocked even holding a certificate. The
			// operator's explicit "no" outranks the org's "yes" — otherwise
			// blocking anyone in the org would be impossible.
			if peer.State == store.PeerBlocked {
				return store.MeshPeer{}, fmt.Errorf("mesh: sender is blocked on this instance")
			}
			return *peer, nil
		}
		return store.MeshPeer{
			ID: e.From, Name: e.Cert.OrgName, State: store.PeerAccepted,
			Scopes: e.Cert.Scopes, Fingerprint: fingerprintOf(e.From),
		}, nil
	}

	// (c) A stranger. The only thing an unknown instance may do is ask.
	if err != nil {
		if e.Kind != KindPeerRequest {
			return store.MeshPeer{}, fmt.Errorf("mesh: %s from an instance that is not connected", e.Kind)
		}
		return store.MeshPeer{ID: e.From, State: store.PeerPending}, nil
	}

	// (a) A known instance.
	switch peer.State {
	case store.PeerAccepted:
		return *peer, nil
	case store.PeerBlocked, store.PeerRevoked:
		// Silent on purpose. Telling a blocked instance that it is blocked
		// confirms the address is live and worth attacking.
		return store.MeshPeer{}, fmt.Errorf("mesh: sender is not permitted")
	default: // pending
		if e.Kind == KindPeerRequest || e.Kind == KindPeerAccept || e.Kind == KindPeerReject {
			return *peer, nil
		}
		return store.MeshPeer{}, fmt.Errorf("mesh: connection to this instance is still pending")
	}
}

// verifyOrgAuthority checks an envelope claiming to act under an org grant.
func (n *Node) verifyOrgAuthority(e *Envelope) error {
	if n.cfg.TrustedOrg == "" {
		return fmt.Errorf("mesh: this instance belongs to no org, so org-authorised messages are refused")
	}
	if e.Via != n.cfg.TrustedOrg {
		return fmt.Errorf("mesh: message claims authority from an org this instance does not trust")
	}
	if e.Cert == nil {
		return fmt.Errorf("mesh: org-authorised message carries no certificate")
	}
	// The certificate must be issued BY the trusted org TO this instance, and
	// the envelope must be signed by that same org. Checking only the first two
	// would let any member replay the org's certificate to impersonate it.
	if e.From != e.Via {
		return fmt.Errorf("mesh: only the org itself may send under its own authority")
	}
	if err := e.Cert.Verify(n.cfg.TrustedOrg, n.id.ID()); err != nil {
		return err
	}
	// Announced only after it verified, so an unverified certificate cannot
	// grant anything.
	n.mu.RLock()
	fn := n.onCert
	n.mu.RUnlock()
	if fn != nil {
		fn(e.Cert)
	}
	return nil
}

// dispatch acts on an authorised envelope.
func (n *Node) dispatch(ctx context.Context, e *Envelope, peer store.MeshPeer) error {
	switch e.Kind {
	case KindPeerRequest:
		return n.handlePeerRequest(e)
	case KindPeerAccept:
		return n.handlePeerAccept(e, peer)
	case KindPeerReject:
		return n.store.SetMeshPeerState(e.From, store.PeerRevoked, nil)
	case KindPing:
		return nil
	}

	// Everything below carries a body only the recipient can read.
	//
	// For an org reaching a member it has never paired with there is no stored
	// key, so the org's own certificate supplies it. That is safe precisely
	// because the certificate is signed by the org and already verified above:
	// only the org can state the org's encryption key.
	keySource := peer.BoxPub
	if e.Via != "" && e.Cert != nil && e.Cert.OrgBox != "" {
		keySource = e.Cert.OrgBox
	}
	box, err := ParseBoxKey(keySource)
	if err != nil {
		return fmt.Errorf("mesh: no usable encryption key for this sender: %w", err)
	}
	var body MessageBody
	if err := n.id.OpenBody(e, box, &body); err != nil {
		return err
	}

	switch e.Kind {
	case KindMessage, KindBroadcast, KindAnswer:
		if !hasScope(peer.Scopes, ScopeMessage) && e.Via == "" {
			return fmt.Errorf("mesh: peer is connected but not permitted to send messages")
		}
		n.record(e, peer, body)
		n.mu.RLock()
		fn := n.onMessage
		n.mu.RUnlock()
		if fn != nil {
			fn(peer, e.Kind, body, ProvenanceOf(e))
		}
		return nil

	case KindAsk:
		if !hasScope(peer.Scopes, ScopeAsk) {
			return fmt.Errorf("mesh: peer is not permitted to ask this instance questions")
		}
		n.record(e, peer, body)
		n.mu.RLock()
		fn := n.onAsk
		n.mu.RUnlock()
		if fn == nil {
			return fmt.Errorf("mesh: this instance cannot answer questions")
		}
		answer, err := fn(ctx, peer, body.Text, ProvenanceOf(e))
		if err != nil {
			return err
		}
		// The answer goes back over the same authenticated channel rather than
		// in the HTTP response, so an answer that takes a minute does not hold
		// the sender's request open.
		go func() {
			sendCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := n.Send(sendCtx, peer.ID, KindAnswer, MessageBody{
				Text: answer, ReplyTo: e.ID, Subject: body.Subject,
			}); err != nil {
				n.log.Warn("mesh: could not deliver the answer",
					zap.String("peer", peer.Name), zap.Error(err))
			}
		}()
		return nil
	}
	return fmt.Errorf("mesh: nothing handles kind %q", e.Kind)
}

func (n *Node) handlePeerRequest(e *Envelope) error {
	// The body of a connection request is NOT encrypted to us — the sender does
	// not yet have a key exchange with this instance — so it is plain JSON and
	// is treated as untrusted input: it names the sender's keys, and the
	// signature already proved the signing key. The box key it declares is
	// bound to that signature, which is what makes it safe to store.
	var req peerRequestBody
	if raw, err := decodeB64(e.Body); err == nil {
		_ = json.Unmarshal(raw, &req)
	}

	existing, err := n.store.MeshPeerByID(e.From)
	if err == nil && (existing.State == store.PeerBlocked || existing.State == store.PeerRevoked) {
		// A refused instance re-asking is not a new decision to make. Dropping
		// it silently stops "ask until they click yes" from working.
		return fmt.Errorf("mesh: sender is not permitted")
	}

	peer := store.MeshPeer{
		ID: e.From, Name: sanitizeName(req.Name), BoxPub: req.BoxPub,
		Endpoint: sanitizeEndpoint(req.Endpoint), State: store.PeerPending,
		Fingerprint: fingerprintOf(e.From), Direction: "in",
		Note: truncate(req.Note, 200),
	}
	if err := n.store.UpsertMeshPeer(peer); err != nil {
		return err
	}
	n.log.Info("mesh: connection request received — awaiting a decision",
		zap.String("name", peer.Name), zap.String("fingerprint", peer.Fingerprint))

	n.mu.RLock()
	fn := n.onRequest
	n.mu.RUnlock()
	if fn != nil {
		fn(peer)
	}
	return nil
}

func (n *Node) handlePeerAccept(e *Envelope, peer store.MeshPeer) error {
	var req peerRequestBody
	if raw, err := decodeB64(e.Body); err == nil {
		_ = json.Unmarshal(raw, &req)
	}
	if req.BoxPub != "" {
		peer.BoxPub = req.BoxPub
	}
	peer.ID = e.From
	peer.Name = sanitizeName(firstNonEmpty(req.Name, peer.Name))
	peer.Fingerprint = fingerprintOf(e.From)
	if err := n.store.UpsertMeshPeer(peer); err != nil {
		return err
	}
	// They accepted us, so we may talk to them. This does not grant them
	// anything here beyond what we already decided when we sent the request.
	n.log.Info("mesh: connection accepted by peer", zap.String("name", peer.Name))
	return n.store.SetMeshPeerState(e.From, store.PeerAccepted, defaultScopes())
}

func (n *Node) record(e *Envelope, peer store.MeshPeer, body MessageBody) {
	text := body.Text
	if body.Subject != "" {
		text = body.Subject + ": " + text
	}
	_ = n.store.RecordMeshMessage(store.MeshMessage{
		ID: e.ID, PeerID: peer.ID, PeerName: peer.Name, Kind: string(e.Kind),
		Direction: "in", Body: truncate(text, 4000), Via: e.Via,
		Origin: originOf(e),
	})
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want || s == "*" {
			return true
		}
	}
	return false
}

func defaultScopes() []string { return []string{ScopeMessage, ScopeBroadcast} }

func fingerprintOf(id string) string {
	k, err := DecodeKey(id)
	if err != nil {
		return ""
	}
	return Fingerprint(k)
}

// sanitizeName strips control characters from a self-declared label.
//
// It is rendered in a terminal and in notifications, and a peer that can put
// escape sequences in its name can forge the surrounding UI — including the
// prompt asking whether to trust it.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return truncate(b.String(), 64)
}

// sanitizeEndpoint refuses anything that is not plainly an HTTP(S) URL, so a
// peer cannot point this instance at a file:// or unix socket path.
func sanitizeEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return ""
	}
	return truncate(s, 300)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
