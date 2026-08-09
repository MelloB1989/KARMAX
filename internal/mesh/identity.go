// Package mesh lets KARMAX instances find each other, agree to talk, and
// exchange messages — one instance per person, plus an instance for the
// organisation they belong to.
//
// The security model, stated plainly because everything else follows from it:
//
//   - An instance IS its key. Names are labels a peer chooses for itself and
//     can lie about; the 32-byte Ed25519 public key is the identity, and its
//     fingerprint is what an operator compares out of band.
//   - Nothing is accepted from a stranger. A peer must be explicitly admitted
//     by the operator before any message from it is processed. Default deny.
//   - The org is a SECOND trust root, not a bypass. An org instance reaches a
//     member without a pairwise connection because it holds a certificate that
//     member's operator chose to trust, not because it skipped a check.
//   - Every envelope is signed, and every body is encrypted to the recipient.
//     A message that is merely signed leaks its contents to anyone on the path.
//   - A valid message replayed is a new message unless something remembers it,
//     so nonces are tracked and stale timestamps refused.
package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// Identity is one instance's cryptographic self.
//
// Two key pairs, deliberately. Ed25519 signs — it proves who sent a thing and
// is what certificates are issued against. X25519 encrypts — it is what a
// sender uses to seal a body only the recipient can open. Reusing one key for
// both is a well-known footgun: the algorithms have different security
// arguments and conflating them has broken real systems.
type Identity struct {
	// Name is a human label, freely chosen and NOT trusted. It exists so
	// `karmax mesh peers` is readable; every decision is made on the key.
	Name string `json:"name"`

	SignPub  ed25519.PublicKey `json:"sign_pub"`
	signPriv ed25519.PrivateKey
	BoxPub   [32]byte `json:"box_pub"`
	boxPriv  [32]byte
}

// Fingerprint is the short, human-comparable form of an identity.
//
// Rendered in groups because the only defence against accepting an attacker's
// key is a person reading it aloud and another person agreeing — and an
// unbroken 52-character string does not get read aloud correctly.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	b32 := strings.ToUpper(base64.RawURLEncoding.EncodeToString(sum[:12]))
	var out []string
	for i := 0; i < len(b32); i += 4 {
		end := i + 4
		if end > len(b32) {
			end = len(b32)
		}
		out = append(out, b32[i:end])
	}
	return strings.Join(out, "-")
}

// ID is the full, unambiguous address of an identity: the base64 signing key.
// Fingerprints are for humans; this is what the protocol matches on.
func (id *Identity) ID() string { return EncodeKey(id.SignPub) }

// Fingerprint is this identity's human-comparable form.
func (id *Identity) Fingerprint() string { return Fingerprint(id.SignPub) }

// EncodeKey renders a public key for transport and storage.
func EncodeKey(k []byte) string { return base64.RawURLEncoding.EncodeToString(k) }

// DecodeKey parses a public key, rejecting anything of the wrong length.
//
// The length check is not pedantry: a short key silently truncated would make
// signature verification fail open in some libraries.
func DecodeKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("mesh: bad key encoding: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("mesh: key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// NewIdentity generates a fresh instance identity.
func NewIdentity(name string) (*Identity, error) {
	signPub, signPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	var boxPriv [32]byte
	if _, err := rand.Read(boxPriv[:]); err != nil {
		return nil, err
	}
	boxPub, err := curve25519.X25519(boxPriv[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	id := &Identity{Name: strings.TrimSpace(name), SignPub: signPub, signPriv: signPriv, boxPriv: boxPriv}
	copy(id.BoxPub[:], boxPub)
	return id, nil
}

// storedIdentity is the on-disk form. Private keys are included, which is why
// the file is written 0600 and why this type is not exported.
type storedIdentity struct {
	Name     string `json:"name"`
	SignPub  string `json:"sign_pub"`
	SignPriv string `json:"sign_priv"`
	BoxPub   string `json:"box_pub"`
	BoxPriv  string `json:"box_priv"`
}

// LoadOrCreateIdentity returns the instance's identity, generating one on first
// use. The private key never leaves this file.
func LoadOrCreateIdentity(dir, name string) (*Identity, error) {
	path := filepath.Join(dir, "identity.json")

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return decodeIdentity(data)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("mesh: read identity: %w", err)
	}

	id, err := NewIdentity(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	blob, err := json.MarshalIndent(storedIdentity{
		Name:     id.Name,
		SignPub:  EncodeKey(id.SignPub),
		SignPriv: base64.RawURLEncoding.EncodeToString(id.signPriv),
		BoxPub:   base64.RawURLEncoding.EncodeToString(id.BoxPub[:]),
		BoxPriv:  base64.RawURLEncoding.EncodeToString(id.boxPriv[:]),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	// 0600 before anything is written, not after: a private key must never
	// exist on disk world-readable, even briefly.
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return nil, fmt.Errorf("mesh: write identity: %w", err)
	}
	return id, nil
}

func decodeIdentity(data []byte) (*Identity, error) {
	var st storedIdentity
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("mesh: identity file is unreadable: %w", err)
	}
	signPub, err := DecodeKey(st.SignPub)
	if err != nil {
		return nil, err
	}
	signPriv, err := base64.RawURLEncoding.DecodeString(st.SignPriv)
	if err != nil || len(signPriv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("mesh: identity signing key is malformed")
	}
	boxPub, err := base64.RawURLEncoding.DecodeString(st.BoxPub)
	if err != nil || len(boxPub) != 32 {
		return nil, fmt.Errorf("mesh: identity box key is malformed")
	}
	boxPriv, err := base64.RawURLEncoding.DecodeString(st.BoxPriv)
	if err != nil || len(boxPriv) != 32 {
		return nil, fmt.Errorf("mesh: identity box private key is malformed")
	}
	id := &Identity{Name: st.Name, SignPub: signPub, signPriv: ed25519.PrivateKey(signPriv)}
	copy(id.BoxPub[:], boxPub)
	copy(id.boxPriv[:], boxPriv)
	return id, nil
}

// Sign produces a detached signature over msg.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.signPriv, msg) }

// sharedSecret derives the symmetric key shared with a peer's box key.
//
// Hashed rather than used raw: the raw X25519 output is not uniformly
// distributed, and feeding it straight to a cipher is the classic mistake.
// Both endpoints must derive the same key, so the peer keys are sorted before
// hashing rather than ordered by who is sending.
func (id *Identity) sharedSecret(peerBox [32]byte) ([32]byte, error) {
	raw, err := curve25519.X25519(id.boxPriv[:], peerBox[:])
	if err != nil {
		return [32]byte{}, fmt.Errorf("mesh: key agreement failed: %w", err)
	}
	a, b := id.BoxPub[:], peerBox[:]
	if string(a) > string(b) {
		a, b = b, a
	}
	h := sha256.New()
	h.Write([]byte("karmax-mesh-v1"))
	h.Write(raw)
	h.Write(a)
	h.Write(b)
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key, nil
}

// PublicIdentity is what an instance publishes about itself: enough to address
// it and to encrypt to it, and nothing more.
type PublicIdentity struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	BoxPub      string `json:"box_pub"`
	Fingerprint string `json:"fingerprint"`
	Endpoint    string `json:"endpoint,omitempty"`
}

// Public returns the shareable half of this identity.
func (id *Identity) Public(endpoint string) PublicIdentity {
	return PublicIdentity{
		Name:        id.Name,
		ID:          id.ID(),
		BoxPub:      base64.RawURLEncoding.EncodeToString(id.BoxPub[:]),
		Fingerprint: id.Fingerprint(),
		Endpoint:    endpoint,
	}
}

// ParseBoxKey decodes a peer's encryption key.
func ParseBoxKey(s string) ([32]byte, error) {
	var out [32]byte
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return out, fmt.Errorf("mesh: bad box key encoding: %w", err)
	}
	if len(b) != 32 {
		return out, fmt.Errorf("mesh: box key is %d bytes, want 32", len(b))
	}
	copy(out[:], b)
	return out, nil
}
