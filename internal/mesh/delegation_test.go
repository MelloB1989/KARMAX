package mesh

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func hopFor(by *Identity, asker, cause string, kind Kind) Hop {
	h := Hop{By: by.ID(), Asker: asker, Cause: cause, Kind: kind, TS: time.Now().Unix()}
	h.Sig = base64.RawURLEncoding.EncodeToString(by.Sign([]byte(h.signingString())))
	return h
}

// chained seals an envelope, attaches a chain, and re-signs it the way send does.
func chained(t *testing.T, from, to *Identity, chain []Hop) *Envelope {
	t.Helper()
	e, err := Seal(from, KindAsk, to.ID(), to.BoxPub, MessageBody{Text: "can you help"})
	if err != nil {
		t.Fatal(err)
	}
	e.Chain = chain
	e.Sig = base64.RawURLEncoding.EncodeToString(from.Sign(e.signingBytes()))
	return e
}

func TestADelegatedEnvelopeNamesItsOrigin(t *testing.T) {
	alice, sam, bob := mustIdentity(t, "alice"), mustIdentity(t, "sam"), mustIdentity(t, "bob")

	e := chained(t, sam, bob, []Hop{hopFor(sam, alice.ID(), "env-1", KindAsk)})
	if err := VerifySignature(e); err != nil {
		t.Fatalf("a valid delegated envelope was refused: %v", err)
	}
	if e.Origin() != alice.ID() {
		t.Errorf("origin is %s, want alice", short(e.Origin()))
	}
	if got := e.participants(); len(got) != 2 || got[0] != alice.ID() || got[1] != sam.ID() {
		t.Errorf("participants = %v", got)
	}
}

func TestAChainCannotBeForgedOrRewritten(t *testing.T) {
	alice, sam, bob, eve := mustIdentity(t, "alice"), mustIdentity(t, "sam"), mustIdentity(t, "bob"), mustIdentity(t, "eve")
	hop := hopFor(sam, alice.ID(), "env-1", KindAsk)

	for _, tc := range []struct {
		name  string
		build func() *Envelope
	}{
		{"chain stripped after signing", func() *Envelope {
			e := chained(t, sam, bob, []Hop{hop})
			e.Chain = nil
			return e
		}},
		{"chain attached to an envelope that never had one", func() *Envelope {
			e, _ := Seal(sam, KindAsk, bob.ID(), bob.BoxPub, MessageBody{})
			e.Chain = []Hop{hop}
			return e
		}},
		{"origin swapped after the hop was signed", func() *Envelope {
			e := chained(t, sam, bob, []Hop{hop})
			e.Chain[0].Asker = eve.ID()
			return e
		}},
		{"hop signed by a key that is not the delegate", func() *Envelope {
			bad := hop
			bad.Sig = base64.RawURLEncoding.EncodeToString(eve.Sign([]byte(bad.signingString())))
			return chained(t, sam, bob, []Hop{bad})
		}},
		{"someone else's chain reused by a different sender", func() *Envelope {
			return chained(t, eve, bob, []Hop{hop})
		}},
		{"unrelated hops spliced together", func() *Envelope {
			second := hopFor(eve, mustIdentity(t, "stranger").ID(), "env-9", KindAsk)
			return chained(t, eve, bob, []Hop{hop, second})
		}},
		{"hop dated in the future", func() *Envelope {
			future := Hop{By: sam.ID(), Asker: alice.ID(), Cause: "env-1", Kind: KindAsk,
				TS: time.Now().Add(2 * clockSkew).Unix()}
			future.Sig = base64.RawURLEncoding.EncodeToString(sam.Sign([]byte(future.signingString())))
			return chained(t, sam, bob, []Hop{future})
		}},
		{"hop naming a kind that does not exist", func() *Envelope {
			bad := Hop{By: sam.ID(), Asker: alice.ID(), Cause: "env-1", Kind: Kind("shell.exec"), TS: time.Now().Unix()}
			bad.Sig = base64.RawURLEncoding.EncodeToString(sam.Sign([]byte(bad.signingString())))
			return chained(t, sam, bob, []Hop{bad})
		}},
		{"hop naming no causing envelope", func() *Envelope {
			return chained(t, sam, bob, []Hop{hopFor(sam, alice.ID(), "", KindAsk)})
		}},
	} {
		if err := VerifySignature(tc.build()); err == nil {
			t.Errorf("%s: was not detected", tc.name)
		}
	}
}

func TestAChainThatLoopsIsRefused(t *testing.T) {
	alice, sam, bob := mustIdentity(t, "alice"), mustIdentity(t, "sam"), mustIdentity(t, "bob")

	// sam delegates to bob, bob delegates back to sam.
	back := chained(t, bob, sam, []Hop{
		hopFor(sam, alice.ID(), "env-1", KindAsk),
		hopFor(bob, sam.ID(), "env-2", KindAsk),
	})
	if err := VerifySignature(back); err == nil {
		t.Error("a chain returning to its own recipient was accepted")
	}

	// The same instance appearing twice mid-chain.
	revisit := chained(t, bob, mustIdentity(t, "carol"), []Hop{
		hopFor(sam, alice.ID(), "env-1", KindAsk),
		hopFor(alice, sam.ID(), "env-2", KindAsk),
		hopFor(bob, alice.ID(), "env-3", KindAsk),
	})
	if err := VerifySignature(revisit); err == nil {
		t.Error("a chain revisiting an instance was accepted")
	}
}

func TestChainDepthIsCapped(t *testing.T) {
	origin := mustIdentity(t, "origin")
	sender := mustIdentity(t, "sender")
	to := mustIdentity(t, "to")

	var chain []Hop
	prev := origin.ID()
	for i := 0; i < maxChainHops; i++ {
		id := mustIdentity(t, fmt.Sprintf("relay-%d", i))
		if i == maxChainHops-1 {
			id = sender
		}
		chain = append(chain, hopFor(id, prev, fmt.Sprintf("env-%d", i), KindAsk))
		prev = id.ID()
	}
	if err := VerifySignature(chained(t, sender, to, chain)); err != nil {
		t.Fatalf("a chain at the limit was refused: %v", err)
	}

	over := append(chain, hopFor(sender, prev, "env-x", KindAsk))
	if err := VerifySignature(chained(t, sender, to, over)); err == nil {
		t.Error("a chain past the limit was accepted")
	}
}

func TestDelegatingRefusesToBuildACycle(t *testing.T) {
	alice, sam, bob := mustIdentity(t, "alice"), mustIdentity(t, "sam"), mustIdentity(t, "bob")
	e := chained(t, sam, bob, []Hop{hopFor(sam, alice.ID(), "env-1", KindAsk)})
	p := ProvenanceOf(e)

	if _, err := p.extend(bob, alice.ID()); err == nil {
		t.Error("delegating back to the origin was allowed")
	}
	if _, err := p.extend(sam, mustIdentity(t, "carol").ID()); err == nil {
		t.Error("an instance already in the chain was allowed to re-delegate")
	}
	if _, err := p.extend(bob, mustIdentity(t, "carol").ID()); err != nil {
		t.Errorf("a legitimate delegation was refused: %v", err)
	}
	if _, err := (Provenance{}).extend(bob, alice.ID()); err == nil {
		t.Error("a provenance naming no request produced a hop")
	}
}

func TestProvenanceCarriesTheWholePath(t *testing.T) {
	alice, sam, bob := mustIdentity(t, "alice"), mustIdentity(t, "sam"), mustIdentity(t, "bob")
	e := chained(t, sam, bob, []Hop{hopFor(sam, alice.ID(), "env-1", KindAsk)})
	p := ProvenanceOf(e)

	if p.Origin() != alice.ID() {
		t.Error("origin was lost")
	}
	if p.Asker() != sam.ID() {
		t.Error("the direct asker was lost")
	}
	if !p.Delegated() || p.Depth() != 1 {
		t.Errorf("depth = %d, delegated = %v", p.Depth(), p.Delegated())
	}
	// A caller must not be able to reach in and rewrite history.
	p.Chain()[0].Asker = bob.ID()
	if p.Origin() != alice.ID() {
		t.Error("Chain() handed out the live slice")
	}

	direct, _ := Seal(alice, KindAsk, bob.ID(), bob.BoxPub, MessageBody{})
	if d := ProvenanceOf(direct); d.Delegated() || d.Origin() != alice.ID() {
		t.Error("an undelegated request did not report itself as direct")
	}
}

func TestAnEnvelopeWithNoChainSignsExactlyAsBefore(t *testing.T) {
	alice, bob := mustIdentity(t, "alice"), mustIdentity(t, "bob")
	e, err := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("karmax-mesh-v1\nv=%d\nid=%s\nkind=%s\nfrom=%s\nto=%s\nts=%d\nnonce=%s\nvia=%s\nbody=%s\n",
		e.V, e.ID, e.Kind, e.From, e.To, e.TS, e.Nonce, e.Via, e.Body)
	if got := string(e.signingBytes()); got != want {
		t.Errorf("the wire format changed for undelegated envelopes:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(string(e.signingBytes()), "hop=") {
		t.Error("an empty chain contributed to the signed bytes")
	}
}

func TestCertificateCapabilitiesAreSeparateFromTransportVerbs(t *testing.T) {
	org, member := mustIdentity(t, "org"), mustIdentity(t, "member")
	cert := IssueCertificate(org, "Vector", member.ID(), []string{
		ScopeMessage, ScopeAsk,
		"tool:github.issues", "memory:org-vector", "spend:100000",
	}, time.Hour)

	// Transport authority is unchanged by the extra scopes.
	if !cert.HasScope(ScopeAsk) || cert.HasScope(ScopeBroadcast) {
		t.Error("transport verbs were disturbed by capability scopes")
	}

	caps := cert.Capabilities()
	if len(caps) != 3 {
		t.Fatalf("capabilities = %+v", caps)
	}
	want := map[string]string{
		"tool": "github.issues", "memory": "org-vector", "spend": "100000",
	}
	for _, c := range caps {
		if want[c.Class] != c.Value {
			t.Errorf("capability %s = %q, want %q", c.Class, c.Value, want[c.Class])
		}
	}

	// Capability scopes are inside the signature like every other scope, so an
	// org cannot be given more access after the fact.
	escalated := *cert
	escalated.Scopes = append(append([]string{}, cert.Scopes...), "tool:*")
	if err := escalated.Verify(org.ID(), member.ID()); err == nil {
		t.Error("capabilities were added after issuance and still verified")
	}
}

func TestACertificateWithOnlyTransportVerbsGrantsNoCapabilities(t *testing.T) {
	org, member := mustIdentity(t, "org"), mustIdentity(t, "member")
	cert := IssueCertificate(org, "Vector", member.ID(), []string{ScopeMessage}, time.Hour)
	if caps := cert.Capabilities(); len(caps) != 0 {
		t.Errorf("a message-only certificate granted %+v", caps)
	}
}
