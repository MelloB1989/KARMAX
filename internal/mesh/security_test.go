package mesh

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// These tests are written as attacks. Each one is a thing a hostile instance
// would try; a failure here is a vulnerability, not a style problem.

func mustIdentity(t *testing.T, name string) *Identity {
	t.Helper()
	id, err := NewIdentity(name)
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	return id
}

func TestSealedBodyIsUnreadableToEveryoneElse(t *testing.T) {
	alice, bob, eve := mustIdentity(t, "alice"), mustIdentity(t, "bob"), mustIdentity(t, "eve")

	e, err := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{Text: "the deal is 2 lakh"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The plaintext must not appear anywhere on the wire.
	if strings.Contains(e.Body, "lakh") || strings.Contains(string(e.signingBytes()), "lakh") {
		t.Fatal("the message body is readable on the wire")
	}

	var got MessageBody
	if err := bob.OpenBody(e, alice.BoxPub, &got); err != nil {
		t.Fatalf("the intended recipient could not read it: %v", err)
	}
	if got.Text != "the deal is 2 lakh" {
		t.Errorf("decrypted to %q", got.Text)
	}
	// An eavesdropper holding the envelope cannot open it.
	if err := eve.OpenBody(e, alice.BoxPub, &got); err == nil {
		t.Fatal("a third party decrypted a message not addressed to it")
	}
}

func TestTamperingWithAnyFieldBreaksTheSignature(t *testing.T) {
	alice, bob := mustIdentity(t, "alice"), mustIdentity(t, "bob")
	base, err := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature(base); err != nil {
		t.Fatalf("an untampered envelope failed to verify: %v", err)
	}

	// Every field that changes meaning must be covered by the signature.
	// `to` especially: if it were not, an envelope could be re-aimed at a
	// different instance while still verifying.
	for _, tc := range []struct {
		name   string
		break_ func(*Envelope)
	}{
		{"re-aimed at another recipient", func(e *Envelope) { e.To = mustIdentity(t, "x").ID() }},
		{"kind escalated", func(e *Envelope) { e.Kind = KindAsk }},
		{"sender swapped", func(e *Envelope) { e.From = mustIdentity(t, "x").ID() }},
		{"timestamp moved", func(e *Envelope) { e.TS += 3600 }},
		{"nonce replaced", func(e *Envelope) { e.Nonce = "AAAAAAAAAAAAAAAAAAAAAA" }},
		{"body substituted", func(e *Envelope) { e.Body = base64.RawURLEncoding.EncodeToString([]byte("x")) }},
		{"labelled as org-authorised", func(e *Envelope) { e.Via = mustIdentity(t, "org").ID() }},
	} {
		e := *base
		tc.break_(&e)
		if err := VerifySignature(&e); err == nil {
			t.Errorf("%s: tampering was not detected", tc.name)
		}
	}
}

func TestReplayIsRefused(t *testing.T) {
	alice, bob := mustIdentity(t, "alice"), mustIdentity(t, "bob")
	e, _ := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{Text: "transfer it"})

	g := NewReplayGuard()
	if err := g.CheckFreshness(e); err != nil {
		t.Fatalf("the first delivery was refused: %v", err)
	}
	// A signature stays valid forever, so without this an attacker who captured
	// one envelope could deliver it daily and it would verify every time.
	if err := g.CheckFreshness(e); err == nil {
		t.Fatal("the same envelope was accepted twice")
	}
}

func TestStaleAndFutureEnvelopesAreRefused(t *testing.T) {
	alice, bob := mustIdentity(t, "alice"), mustIdentity(t, "bob")
	g := NewReplayGuard()

	old, _ := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{Text: "x"})
	old.TS = time.Now().Add(-2 * clockSkew).Unix()
	if err := g.CheckFreshness(old); err == nil {
		t.Error("a stale envelope was accepted")
	}
	future, _ := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{Text: "x"})
	future.TS = time.Now().Add(2 * clockSkew).Unix()
	if err := g.CheckFreshness(future); err == nil {
		t.Error("an envelope from the future was accepted")
	}
}

func TestCertificateCannotBeForgedOrMisused(t *testing.T) {
	org, other, member := mustIdentity(t, "org"), mustIdentity(t, "not-the-org"), mustIdentity(t, "member")

	good := IssueCertificate(org, "Vector", member.ID(), []string{ScopeMessage}, time.Hour)
	if err := good.Verify(org.ID(), member.ID()); err != nil {
		t.Fatalf("a valid certificate was refused: %v", err)
	}

	// Issued by someone else entirely.
	forged := IssueCertificate(other, "Vector", member.ID(), []string{"*"}, time.Hour)
	if err := forged.Verify(org.ID(), member.ID()); err == nil {
		t.Error("a certificate from an untrusted org was accepted")
	}

	// Issued for a different member, then presented here. This is the attack
	// where an org member replays a colleague's certificate at your instance.
	elsewhere := IssueCertificate(org, "Vector", other.ID(), []string{"*"}, time.Hour)
	if err := elsewhere.Verify(org.ID(), member.ID()); err == nil {
		t.Error("a certificate issued to another instance was accepted")
	}

	// Scope escalation after signing.
	escalated := *good
	escalated.Scopes = []string{"*"}
	if err := escalated.Verify(org.ID(), member.ID()); err == nil {
		t.Error("scopes were widened after issuance and still verified")
	}

	// Expiry is what bounds the damage of a leaked certificate.
	expired := IssueCertificate(org, "Vector", member.ID(), []string{ScopeMessage}, -time.Minute)
	if err := expired.Verify(org.ID(), member.ID()); err == nil {
		t.Error("an expired certificate was accepted")
	}

	// An instance that belongs to no org must refuse every certificate.
	if err := good.Verify("", member.ID()); err == nil {
		t.Error("an instance trusting no org accepted a certificate")
	}
}

func TestScopesAreEnforcedNotAdvisory(t *testing.T) {
	// Being connected is not the same as being allowed to read memory.
	if hasScope([]string{ScopeMessage}, ScopeAsk) {
		t.Error("a message-only peer was granted ask")
	}
	if !hasScope([]string{ScopeMessage}, ScopeMessage) {
		t.Error("a granted scope was not recognised")
	}
	if !hasScope([]string{"*"}, ScopeAsk) {
		t.Error("wildcard should grant everything")
	}
	if hasScope(nil, ScopeMessage) {
		t.Error("a peer with no scopes was granted one")
	}
}

func TestEndpointsThatWouldTurnUsIntoAProxyAreRefused(t *testing.T) {
	// A peer supplies its own endpoint, so without these checks it could aim
	// this instance's authenticated requests at the host's cloud metadata
	// service — SSRF with a signature attached.
	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"gopher://internal:70/x",
		"ftp://example.com",
		"http://0.0.0.0/",
		"not a url at all",
		"",
	} {
		if err := checkEndpoint(bad); err == nil {
			t.Errorf("endpoint %q should have been refused", bad)
		}
	}
	for _, ok := range []string{
		"http://127.0.0.1:9090/mesh", // two instances on one host is normal
		"https://karmax.example.com/mesh",
	} {
		if err := checkEndpoint(ok); err != nil {
			t.Errorf("endpoint %q should be allowed: %v", ok, err)
		}
	}
}

func TestUnknownKindsAreRefused(t *testing.T) {
	alice, bob := mustIdentity(t, "alice"), mustIdentity(t, "bob")
	if _, err := Seal(alice, Kind("shell.exec"), bob.ID(), bob.BoxPub, nil); err == nil {
		t.Error("an unknown kind was sealed rather than refused")
	}
	// And on the receiving side: an old instance must refuse a kind it does not
	// understand rather than dropping it silently, or a new version could
	// smuggle behaviour past it.
	e, _ := Seal(alice, KindMessage, bob.ID(), bob.BoxPub, MessageBody{})
	e.Kind = Kind("shell.exec")
	if err := VerifySignature(e); err == nil {
		t.Error("an unknown kind verified")
	}
}

func TestNamesCannotForgeTheApprovalPrompt(t *testing.T) {
	// The name is rendered in the terminal prompt that asks whether to trust
	// this peer. Escape sequences there could redraw that prompt.
	got := sanitizeName("evil\x1b[2Ktrusted\x00\nadmin")
	if strings.ContainsAny(got, "\x1b\x00\n") {
		t.Errorf("control characters survived sanitisation: %q", got)
	}
	if len(sanitizeName(strings.Repeat("a", 500))) > 64 {
		t.Error("an over-long name was not truncated")
	}
}

func TestFingerprintIsStableAndReadable(t *testing.T) {
	id := mustIdentity(t, "a")
	if Fingerprint(id.SignPub) != Fingerprint(id.SignPub) {
		t.Error("fingerprints are not stable")
	}
	if Fingerprint(id.SignPub) == Fingerprint(mustIdentity(t, "b").SignPub) {
		t.Error("two identities produced the same fingerprint")
	}
	// Grouped, because the only defence against accepting an attacker's key is
	// a human reading it aloud correctly.
	if !strings.Contains(id.Fingerprint(), "-") {
		t.Errorf("fingerprint is not grouped for reading: %s", id.Fingerprint())
	}
}

func TestKeysOfTheWrongLengthAreRefused(t *testing.T) {
	// A silently truncated key can make verification fail open in some
	// libraries, so length is checked before anything else uses it.
	if _, err := DecodeKey(base64.RawURLEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("a short signing key was accepted")
	}
	if _, err := ParseBoxKey(base64.RawURLEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("a short box key was accepted")
	}
	if _, err := DecodeKey("!!!not base64!!!"); err == nil {
		t.Error("a malformed key was accepted")
	}
}

func TestBothSidesDeriveTheSameSecret(t *testing.T) {
	alice, bob := mustIdentity(t, "alice"), mustIdentity(t, "bob")
	a, err := alice.sharedSecret(bob.BoxPub)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bob.sharedSecret(alice.BoxPub)
	if err != nil {
		t.Fatal(err)
	}
	// Derivation sorts the two public keys, so it does not matter who is
	// sending — otherwise a reply would be undecryptable.
	if a != b {
		t.Error("the two endpoints derived different keys")
	}
	eve := mustIdentity(t, "eve")
	c, _ := alice.sharedSecret(eve.BoxPub)
	if a == c {
		t.Error("a different peer derived the same secret")
	}
}
