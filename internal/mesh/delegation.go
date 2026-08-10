package mesh

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Provenance: on whose behalf an envelope is sent, not merely who sent it.
//
// Each hop is signed by the instance that made it, so the chain is append-only:
// a delegate can add its own step but cannot rewrite, drop or forge an earlier
// one. It does NOT prove the origin consented to being named — that would need
// the origin to anticipate the delegation. Each claim is attributable to a key
// and the sender vouches for the whole, which is enough to refuse cycles, bound
// depth, and refuse work laundered for someone this instance has blocked.

// maxChainHops is checked before any hop is verified, since each costs a
// signature check and the chain is attacker-influenced.
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

// canonical is the hop as the ENVELOPE's signature covers it, Sig included so
// one valid signature cannot be swapped for another.
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
	// Only future-dating. Age is deliberately unbounded: a loop may be asked on
	// Monday and delegate on Thursday, and envelope freshness stops replay.
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

// verifyChain checks an envelope's delegation history. Called from
// VerifySignature so no path can authenticate without validating the chain.
func verifyChain(e *Envelope) error {
	if len(e.Chain) == 0 {
		return nil
	}
	if len(e.Chain) > maxChainHops {
		return fmt.Errorf("mesh: delegation chain is %d hops; the limit is %d",
			len(e.Chain), maxChainHops)
	}

	// seen doubles as the cycle check.
	seen := make(map[string]bool, len(e.Chain)+1)
	for i := range e.Chain {
		h := &e.Chain[i]
		if err := h.verify(); err != nil {
			return fmt.Errorf("mesh: delegation hop %d %v", i, err)
		}
		if i == 0 {
			seen[h.Asker] = true
		} else if h.Asker != e.Chain[i-1].By {
			// Otherwise real hops could be spliced into a chain that never happened.
			return fmt.Errorf("mesh: delegation chain breaks between hop %d and %d", i-1, i)
		}
		if seen[h.By] {
			return fmt.Errorf("mesh: delegation chain revisits an instance it already passed through")
		}
		seen[h.By] = true
	}

	// Otherwise a peer could attach someone else's chain and inherit its origin.
	if last := e.Chain[len(e.Chain)-1]; last.By != e.From {
		return fmt.Errorf("mesh: the sender did not make the last delegation hop")
	}
	// A broadcast has no single recipient, so there is nothing to compare.
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

// Provenance is why this instance is acting. Fields are unexported so one can
// only come from an envelope that already verified.
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

// Asker is who put the request to this instance directly, which is not the
// origin once there is a chain.
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

// extend appends this instance's signed hop. Limits are enforced here as well
// as on receipt, so a sender gets a usable error rather than a refusal.
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
