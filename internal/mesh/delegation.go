package mesh

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Provenance: on whose behalf an envelope is being sent.
//
// The mesh already answered "who sent this". It did not answer "who is this
// actually for", and the gap is a confused deputy. Peer A puts a question to
// this instance; this instance needs help and asks peer B. B sees a request
// from an instance it has accepted, with nothing to say A originated it — so B
// answers A's question believing it answered ours. Every check B makes passes,
// and B is still wrong about who it just served.
//
// A delegation chain closes that. Each hop is signed by the instance that made
// it, which makes the chain APPEND-ONLY: a delegate can add its own step, but
// it cannot rewrite an earlier one, drop an inconvenient one, or invent a hop
// attributed to a key it does not hold, because every one of those breaks a
// signature the recipient checks.
//
// What the chain is NOT: proof that A consented to being named. A hop is the
// delegate's signed assertion about who asked it, not A's signature. Making it
// A's signature would mean A had to anticipate the delegation before it
// happened, which defeats the purpose. So the guarantee is precisely: each
// claim in the chain is attributable to a specific key, and the sender's own
// key vouches for the whole of it. That is the same trust the recipient already
// extends to the sender by accepting it as a peer — and it is enough for the
// three things the chain has to support: refusing cycles, bounding depth, and
// refusing work laundered on behalf of somebody this instance has blocked.

// maxChainHops bounds a delegation chain.
//
// The chain is attacker-influenced input and each hop costs a signature
// verification, so the length is checked before any of them is verified. Eight
// is far past any legitimate depth — work that has passed through eight
// instances has stopped being delegation and become a routing loop.
const maxChainHops = 8

// Hop is one step of delegation: instance By acted because Asker asked it to,
// in the envelope identified by Cause.
type Hop struct {
	By    string `json:"by"`    // the instance that delegated onward; signs this hop
	Asker string `json:"asker"` // who asked it, causing the delegation
	Cause string `json:"cause"` // envelope id of that causing request
	Kind  Kind   `json:"kind"`  // kind of the causing request
	TS    int64  `json:"ts"`
	Sig   string `json:"sig"` // By's signature over signingString
}

// signingString is what the hop's own signature covers.
func (h *Hop) signingString() string {
	return fmt.Sprintf("karmax-hop-v1|by=%s|asker=%s|cause=%s|kind=%s|ts=%d",
		h.By, h.Asker, h.Cause, h.Kind, h.TS)
}

// canonical is the hop as the ENVELOPE's signature covers it.
//
// Sig is included: without it a hop's signature could be swapped for another
// valid signature by the same key over different fields, and the envelope
// signature would not notice.
func (h *Hop) canonical() string { return h.signingString() + "|sig=" + h.Sig }

// verify checks one hop against the key that claims to have written it.
func (h *Hop) verify() error {
	by, err := DecodeKey(h.By)
	if err != nil {
		return fmt.Errorf("delegating instance: %w", err)
	}
	if _, err := DecodeKey(h.Asker); err != nil {
		return fmt.Errorf("asking instance: %w", err)
	}
	if strings.TrimSpace(h.Cause) == "" {
		return fmt.Errorf("names no causing envelope")
	}
	if !validKinds[h.Kind] {
		return fmt.Errorf("causing envelope has unknown kind %q", h.Kind)
	}
	// Future-dating is refused on the same bound as an envelope. Age is
	// deliberately NOT bounded: a durable loop may legitimately be asked
	// something on Monday and delegate on Thursday, and that is the product,
	// not an anomaly. Freshness of the ENVELOPE is what stops replay; a hop is
	// history, and history is allowed to be old.
	if time.Unix(h.TS, 0).After(time.Now().Add(clockSkew)) {
		return fmt.Errorf("is dated in the future")
	}
	sig, err := base64.RawURLEncoding.DecodeString(h.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is malformed")
	}
	if !ed25519.Verify(by, []byte(h.signingString()), sig) {
		return fmt.Errorf("signature does not verify")
	}
	return nil
}

// verifyChain checks the delegation history carried by an envelope.
//
// Called from VerifySignature, so no path can authenticate an envelope without
// also validating its chain — a chain checked only where someone remembered to
// check it is a chain that eventually goes unchecked.
func verifyChain(e *Envelope) error {
	if len(e.Chain) == 0 {
		return nil
	}
	if len(e.Chain) > maxChainHops {
		return fmt.Errorf("mesh: delegation chain is %d hops; the limit is %d",
			len(e.Chain), maxChainHops)
	}

	// seen doubles as the cycle check. An instance that appears twice in one
	// chain means the work has come back to somewhere it already was, and
	// following it further is how two instances delegate to each other forever.
	seen := make(map[string]bool, len(e.Chain)+1)
	for i := range e.Chain {
		h := &e.Chain[i]
		if err := h.verify(); err != nil {
			return fmt.Errorf("mesh: delegation hop %d %v", i, err)
		}
		if i == 0 {
			seen[h.Asker] = true
		} else if h.Asker != e.Chain[i-1].By {
			// Each hop must name the previous delegate as its asker. Without
			// this, unrelated hops signed by real keys could be spliced into a
			// chain that never happened.
			return fmt.Errorf("mesh: delegation chain breaks between hop %d and %d", i-1, i)
		}
		if seen[h.By] {
			return fmt.Errorf("mesh: delegation chain revisits an instance it already passed through")
		}
		seen[h.By] = true
	}

	// The sender must be the instance that made the last hop. Otherwise any
	// peer could attach somebody else's chain to its own envelope and inherit
	// the authority of an origin it was never delegated by.
	if last := e.Chain[len(e.Chain)-1]; last.By != e.From {
		return fmt.Errorf("mesh: the sender did not make the last delegation hop")
	}
	// And it must not be coming back to us. A broadcast has no single
	// recipient, so there is nothing to compare.
	if e.To != "*" && seen[e.To] {
		return fmt.Errorf("mesh: delegation chain returns to its own recipient")
	}
	return nil
}

// Origin is the instance the work ultimately traces back to: the first asker in
// the chain, or the sender when there is no chain.
func (e *Envelope) Origin() string {
	if len(e.Chain) > 0 {
		return e.Chain[0].Asker
	}
	return e.From
}

// participants lists every instance this envelope has passed through, origin
// first and sender last.
func (e *Envelope) participants() []string {
	if len(e.Chain) == 0 {
		return []string{e.From}
	}
	out := make([]string, 0, len(e.Chain)+1)
	out = append(out, e.Chain[0].Asker)
	for _, h := range e.Chain {
		out = append(out, h.By)
	}
	return out
}

// Provenance is why this instance is acting: the request that asked it to, and
// the delegation history behind that request.
//
// Its fields are unexported on purpose. A Provenance can only be obtained from
// an envelope that already verified, so a caller cannot hand-assemble one
// claiming an origin that never asked for anything.
type Provenance struct {
	chain []Hop
	cause string
	asker string
	kind  Kind
}

// ProvenanceOf records what an inbound envelope authorises this instance to act
// on behalf of. Pass the result to SendOnBehalf when delegating onward.
func ProvenanceOf(e *Envelope) Provenance {
	return Provenance{
		chain: append([]Hop(nil), e.Chain...),
		cause: e.ID,
		asker: e.From,
		kind:  e.Kind,
	}
}

// Origin is the instance the work traces back to.
func (p Provenance) Origin() string {
	if len(p.chain) > 0 {
		return p.chain[0].Asker
	}
	return p.asker
}

// Asker is who put this request to this instance directly — the last hop
// before us, which is not the origin once there is a chain.
func (p Provenance) Asker() string { return p.asker }

// Cause is the envelope this instance is acting on.
func (p Provenance) Cause() string { return p.cause }

// Depth is how many delegations happened before this instance was asked.
func (p Provenance) Depth() int { return len(p.chain) }

// Delegated reports whether this work reached us through another instance
// rather than from the peer that originated it.
func (p Provenance) Delegated() bool { return len(p.chain) > 0 }

// Participants lists every instance the work has passed through, origin first,
// ending with the peer that asked us.
func (p Provenance) Participants() []string {
	if len(p.chain) == 0 {
		return []string{p.asker}
	}
	out := make([]string, 0, len(p.chain)+1)
	out = append(out, p.chain[0].Asker)
	for _, h := range p.chain {
		out = append(out, h.By)
	}
	return out
}

// Chain returns a copy of the delegation history, for display and audit.
func (p Provenance) Chain() []Hop { return append([]Hop(nil), p.chain...) }

// extend appends this instance's own hop, signed, producing the chain an
// outbound envelope should carry.
//
// The limits are enforced HERE as well as on receipt. A sender that discovers
// its own cycle gets a usable error; one that only finds out when the far side
// refuses gets a delivery failure and no reason.
func (p Provenance) extend(id *Identity, to string) ([]Hop, error) {
	if strings.TrimSpace(p.cause) == "" || strings.TrimSpace(p.asker) == "" {
		return nil, fmt.Errorf("mesh: this provenance names no request to act on behalf of")
	}
	if len(p.chain)+1 > maxChainHops {
		return nil, fmt.Errorf("mesh: delegating again would make the chain %d hops; the limit is %d",
			len(p.chain)+1, maxChainHops)
	}
	self := id.ID()
	for _, who := range p.Participants() {
		if who == self {
			return nil, fmt.Errorf("mesh: this instance is already in the delegation chain")
		}
		if who == to {
			return nil, fmt.Errorf("mesh: that instance is already in the delegation chain")
		}
	}
	h := Hop{By: self, Asker: p.asker, Cause: p.cause, Kind: p.kind, TS: time.Now().Unix()}
	h.Sig = base64.RawURLEncoding.EncodeToString(id.Sign([]byte(h.signingString())))
	return append(append([]Hop(nil), p.chain...), h), nil
}
