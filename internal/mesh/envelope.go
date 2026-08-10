package mesh

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The wire format.
//
// One envelope shape for everything, because a protocol with several shapes
// grows several verification paths and only one of them stays correct.
//
// Signed over a CANONICAL byte string rather than over the JSON as received:
// two encoders disagree about key order and whitespace, so signing raw JSON
// means a signature that verifies on the sender and fails on the receiver — or,
// worse, a receiver that verifies one rendering and then acts on another.

// Kind enumerates what an envelope is for. A closed set: an unknown kind is
// refused rather than ignored, so a future version cannot smuggle behaviour
// past an old instance that silently drops what it does not understand.
type Kind string

const (
	// KindPeerRequest asks an instance to accept a connection. The one kind
	// legitimately received from a stranger, and it does nothing on arrival
	// except await a human decision.
	KindPeerRequest Kind = "peer.request"
	// KindPeerAccept tells the requester it was admitted.
	KindPeerAccept Kind = "peer.accept"
	// KindPeerReject closes the request. Sent so the other side stops waiting;
	// it deliberately carries no reason, since "you are blocked" is
	// information an unwanted peer does not need.
	KindPeerReject Kind = "peer.reject"
	// KindMessage is a direct message between connected instances.
	KindMessage Kind = "message"
	// KindBroadcast is a message sent to every connected peer.
	KindBroadcast Kind = "broadcast"
	// KindAsk is a question one instance puts to another, answered by its
	// agent under that peer's scopes.
	KindAsk Kind = "ask"
	// KindAnswer replies to an ask.
	KindAnswer Kind = "answer"
	// KindPing checks liveness and clock agreement.
	KindPing Kind = "ping"
)

var validKinds = map[Kind]bool{
	KindPeerRequest: true, KindPeerAccept: true, KindPeerReject: true,
	KindMessage: true, KindBroadcast: true, KindAsk: true,
	KindAnswer: true, KindPing: true,
}

const (
	// maxBodyBytes caps a decrypted body. Without it a peer can spend this
	// instance's memory for free.
	maxBodyBytes = 256 << 10
	// clockSkew is how far out a timestamp may be. Generous enough for hosts
	// that never sync properly, tight enough that a captured envelope stops
	// being useful quickly.
	clockSkew = 5 * time.Minute
)

// Envelope is one message on the wire.
type Envelope struct {
	V     int    `json:"v"`
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	From  string `json:"from"`           // sender's signing key
	To    string `json:"to"`             // recipient's signing key; "*" for a broadcast
	TS    int64  `json:"ts"`             // unix seconds
	Nonce string `json:"nonce"`          // random per envelope; replay defence
	Body  string `json:"body,omitempty"` // sealed, base64
	// Via names the org whose certificate authorises this envelope, when it is
	// sent by an org instance to a member it has no pairwise connection with.
	Via string `json:"via,omitempty"`
	// Cert is the membership/authority certificate backing Via.
	Cert *Certificate `json:"cert,omitempty"`
	// Chain is the delegation history: who asked whom, in order, when this
	// envelope is sent on somebody else's behalf. Empty when the sender is
	// acting for itself. See delegation.go.
	Chain []Hop  `json:"chain,omitempty"`
	Sig   string `json:"sig"`
}

// signingBytes renders the fields a signature covers, in a fixed order.
//
// Everything that changes meaning is covered — including To and Via, so an
// envelope cannot be re-aimed at a different recipient or re-labelled as
// org-authorised while keeping a valid signature.
//
// Optional fields contribute a line only when present, which is what lets the
// delegation chain be added without invalidating the format: an envelope with
// no chain signs exactly the bytes it always did, so an instance built before
// chains existed and one built after still agree about every envelope neither
// of them delegates. A chained envelope simply requires both ends to know what
// a chain is, and an older instance refuses it rather than acting on a part of
// it — which is the safe direction to fail.
func (e *Envelope) signingBytes() []byte {
	var b strings.Builder
	b.WriteString("karmax-mesh-v1\n")
	fmt.Fprintf(&b, "v=%d\nid=%s\nkind=%s\nfrom=%s\nto=%s\nts=%d\nnonce=%s\nvia=%s\nbody=%s\n",
		e.V, e.ID, e.Kind, e.From, e.To, e.TS, e.Nonce, e.Via, e.Body)
	if e.Cert != nil {
		b.WriteString("cert=" + e.Cert.signingString() + "\n")
	}
	// Covered by the sender's signature, so a chain cannot be attached to,
	// stripped from, or reordered within an envelope after it was signed.
	for i := range e.Chain {
		b.WriteString("hop=" + e.Chain[i].canonical() + "\n")
	}
	return []byte(b.String())
}

// Seal builds a signed, encrypted envelope.
//
// body is JSON-marshalled, sealed to the recipient, and only then signed —
// sign-then-encrypt would leave the signature outside the ciphertext where it
// identifies the sender to anyone watching.
func Seal(id *Identity, kind Kind, toID string, toBox [32]byte, body any) (*Envelope, error) {
	if !validKinds[kind] {
		return nil, fmt.Errorf("mesh: refusing to send unknown kind %q", kind)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mesh: encode body: %w", err)
	}
	if len(raw) > maxBodyBytes {
		return nil, fmt.Errorf("mesh: body is %d bytes; the limit is %d", len(raw), maxBodyBytes)
	}
	sealed, err := id.sealBody(toBox, raw)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	e := &Envelope{
		V: 1, ID: newID(), Kind: kind,
		From: id.ID(), To: toID, TS: time.Now().Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		Body:  sealed,
	}
	e.Sig = base64.RawURLEncoding.EncodeToString(id.Sign(e.signingBytes()))
	return e, nil
}

// sealBody encrypts to the recipient with AES-256-GCM under the derived shared
// secret. AEAD, not raw encryption: without authentication a body can be
// tampered with even though it cannot be read.
func (id *Identity) sealBody(peerBox [32]byte, plaintext []byte) (string, error) {
	key, err := id.sharedSecret(peerBox)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	// Nonce prefixed to the ciphertext, which is standard and avoids a second
	// field that could be dropped or swapped independently.
	out := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// OpenBody decrypts an envelope's body using the sender's box key.
func (id *Identity) OpenBody(e *Envelope, senderBox [32]byte, into any) error {
	if strings.TrimSpace(e.Body) == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(e.Body)
	if err != nil {
		return fmt.Errorf("mesh: body is not valid base64: %w", err)
	}
	key, err := id.sharedSecret(senderBox)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if len(raw) < gcm.NonceSize() {
		return fmt.Errorf("mesh: body is too short to contain a nonce")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// Deliberately uninformative: distinguishing "wrong key" from "tampered"
		// hands an attacker an oracle.
		return fmt.Errorf("mesh: could not decrypt the body")
	}
	if len(plain) > maxBodyBytes {
		return fmt.Errorf("mesh: decrypted body exceeds the size limit")
	}
	return json.Unmarshal(plain, into)
}

// VerifySignature checks that the envelope was signed by the key it claims.
//
// This proves ORIGIN only. Whether that origin is allowed to say this is a
// separate question, answered by the peer store — conflating the two is how
// systems end up authenticating strangers perfectly and then obeying them.
func VerifySignature(e *Envelope) error {
	if e.V != 1 {
		return fmt.Errorf("mesh: unsupported protocol version %d", e.V)
	}
	if !validKinds[e.Kind] {
		return fmt.Errorf("mesh: unknown kind %q", e.Kind)
	}
	from, err := DecodeKey(e.From)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(e.Sig)
	if err != nil {
		return fmt.Errorf("mesh: bad signature encoding: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("mesh: signature is the wrong length")
	}
	if !ed25519.Verify(from, e.signingBytes(), sig) {
		return fmt.Errorf("mesh: signature does not verify")
	}
	// Only now, once the envelope is known to be genuine. A chain costs one
	// signature verification per hop, and doing that for anything that arrives
	// would let an unauthenticated sender multiply its own cost by eight.
	return verifyChain(e)
}

// CheckFreshness rejects envelopes that are too old, too far in the future, or
// already seen.
//
// A signature makes a message authentic forever, which is exactly the problem:
// without this, an attacker who captured one "delete everything" message could
// replay it daily and it would verify every time.
func (s *ReplayGuard) CheckFreshness(e *Envelope) error {
	now := time.Now()
	ts := time.Unix(e.TS, 0)
	if ts.After(now.Add(clockSkew)) {
		return fmt.Errorf("mesh: timestamp is %s in the future", ts.Sub(now).Round(time.Second))
	}
	if now.Sub(ts) > clockSkew {
		return fmt.Errorf("mesh: envelope is %s old; the freshness window is %s",
			now.Sub(ts).Round(time.Second), clockSkew)
	}
	if !s.remember(e.From + ":" + e.Nonce) {
		return fmt.Errorf("mesh: this envelope has already been seen")
	}
	return nil
}

// ReplayGuard remembers recently seen envelopes.
//
// Bounded by the freshness window rather than by count: anything older than the
// window is refused on its timestamp anyway, so remembering it buys nothing and
// an unbounded set is its own denial of service.
type ReplayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewReplayGuard() *ReplayGuard {
	return &ReplayGuard{seen: make(map[string]time.Time)}
}

// remember records a nonce, returning false when it was already present.
func (s *ReplayGuard) remember(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if len(s.seen) > 4096 {
		for k, t := range s.seen {
			if now.Sub(t) > clockSkew {
				delete(s.seen, k)
			}
		}
	}
	if _, dup := s.seen[key]; dup {
		return false
	}
	s.seen[key] = now
	return true
}

// Certificate is the org's grant of authority over a member instance.
//
// This is what lets an org instance address a member with no pairwise
// connection. It is not a bypass: the member's operator chose to trust the org
// key, the grant is scoped, and it expires.
type Certificate struct {
	Org     string `json:"org"` // org's signing key
	OrgName string `json:"org_name"`
	// OrgBox is the org's ENCRYPTION key, carried here because a member the org
	// has never paired with has no other way to obtain it — and fetching it
	// over the network at delivery time would mean trusting whatever answered.
	// Inside the signature, so only the org can state its own key.
	OrgBox  string   `json:"org_box"`
	Subject string   `json:"subject"` // member's signing key
	Scopes  []string `json:"scopes"`
	Issued  int64    `json:"issued"`
	Expires int64    `json:"expires"`
	Sig     string   `json:"sig"` // org's signature over signingString
}

// signingString is the certificate's canonical form. Scopes are sorted so the
// same grant always produces the same bytes.
func (c *Certificate) signingString() string {
	scopes := append([]string(nil), c.Scopes...)
	sort.Strings(scopes)
	return fmt.Sprintf("karmax-cert-v1|org=%s|orgbox=%s|sub=%s|scopes=%s|iat=%d|exp=%d",
		c.Org, c.OrgBox, c.Subject, strings.Join(scopes, ","), c.Issued, c.Expires)
}

// IssueCertificate signs a membership grant. Called on the org instance.
func IssueCertificate(org *Identity, orgName, subject string, scopes []string, ttl time.Duration) *Certificate {
	now := time.Now()
	c := &Certificate{
		Org: org.ID(), OrgName: orgName, Subject: subject, Scopes: scopes,
		OrgBox: base64.RawURLEncoding.EncodeToString(org.BoxPub[:]),
		Issued: now.Unix(), Expires: now.Add(ttl).Unix(),
	}
	c.Sig = base64.RawURLEncoding.EncodeToString(org.Sign([]byte(c.signingString())))
	return c
}

// Verify checks a certificate against the org key this instance trusts.
//
// trustedOrg is supplied by the caller from local configuration and is never
// read out of the certificate: a certificate that names its own issuer as
// authoritative proves nothing at all.
func (c *Certificate) Verify(trustedOrg, subject string) error {
	if c == nil {
		return fmt.Errorf("mesh: no certificate")
	}
	if strings.TrimSpace(trustedOrg) == "" {
		return fmt.Errorf("mesh: this instance trusts no org")
	}
	// Constant time: comparing identity strings with == leaks how much of a
	// guess was right to anyone who can time the response.
	if subtle.ConstantTimeCompare([]byte(c.Org), []byte(trustedOrg)) != 1 {
		return fmt.Errorf("mesh: certificate was issued by an org this instance does not trust")
	}
	if subtle.ConstantTimeCompare([]byte(c.Subject), []byte(subject)) != 1 {
		return fmt.Errorf("mesh: certificate was issued to a different instance")
	}
	now := time.Now().Unix()
	if c.Expires <= now {
		return fmt.Errorf("mesh: certificate expired %s ago",
			time.Duration(now-c.Expires)*time.Second)
	}
	if c.Issued > now+int64(clockSkew.Seconds()) {
		return fmt.Errorf("mesh: certificate is not valid yet")
	}
	orgKey, err := DecodeKey(c.Org)
	if err != nil {
		return err
	}
	sig, err := base64.RawURLEncoding.DecodeString(c.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("mesh: certificate signature is malformed")
	}
	if !ed25519.Verify(orgKey, []byte(c.signingString()), sig) {
		return fmt.Errorf("mesh: certificate signature does not verify")
	}
	return nil
}

// HasScope reports whether the certificate grants a transport verb.
func (c *Certificate) HasScope(want string) bool {
	for _, s := range c.Scopes {
		if s == want || s == "*" {
			return true
		}
	}
	return false
}

// Capabilities returns the scopes that are not transport verbs — "tool:x",
// "memory:ns:write", "spend:100000" and so on.
//
// A certificate carries them; it does not enforce them. Transport authority
// answers "may this instance speak to me", capability authority answers "may
// the work it sends touch this", and the two diverge — which is why the Broker
// is a separate mechanism that takes these as one of its inputs.
func (c *Certificate) Capabilities() []Capability {
	var out []Capability
	for _, s := range c.Scopes {
		class, value, ok := strings.Cut(s, ":")
		if !ok || isTransportVerb(s) {
			continue
		}
		out = append(out, Capability{Class: class, Value: value})
	}
	return out
}

// Capability is one non-transport grant carried by a certificate.
type Capability struct {
	Class string
	Value string
}

func isTransportVerb(s string) bool {
	switch s {
	case ScopeMessage, ScopeAsk, ScopeBroadcast, "*":
		return true
	}
	return false
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
