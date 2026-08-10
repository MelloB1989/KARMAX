package wasmloop

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
)

type signer struct {
	pub  string
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{pub: base64.RawURLEncoding.EncodeToString(pub), priv: priv}
}

// fakeModule stands in for compiled wasm. Nothing here compiles it; these
// tests are about what reaches the compiler, which is the security boundary.
var fakeModule = []byte("\x00asm\x01\x00\x00\x00 pretend this is a real module")

func manifestFor(pub string) Manifest {
	return Manifest{
		Name: "tech-news", Version: "1.2.0", Description: "digests the news",
		Publisher: pub,
		Host:      []string{FnLog, FnHTTP, FnNotify},
		Capabilities: []string{
			"http:hacker-news.firebaseio.com",
			"memory:nexus",
		},
		Schedule: "0 8 * * *", MemoryMB: 32,
	}
}

func packed(t *testing.T, m Manifest, module []byte, sigs ...signer) []byte {
	t.Helper()
	// The digest has to be in the manifest before signing, since it is inside
	// the signature — Pack computes it, so sign against a packed copy first.
	probe, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Unpack(probe)
	if err != nil {
		t.Fatal(err)
	}
	m = a.Manifest

	var out []Signature
	for i, s := range sigs {
		role := RolePublisher
		if i > 0 {
			role = RoleRegistry
		}
		out = append(out, Sign(&m, role, s.priv))
	}
	data, err := Pack(m, module, out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAValidArtifactVerifies(t *testing.T) {
	pub := newSigner(t)
	reg := newSigner(t)
	data := packed(t, manifestFor(pub.pub), fakeModule, pub, reg)

	a, v, err := VerifyBytes(data, Trust{Registries: []string{reg.pub}})
	if err != nil {
		t.Fatalf("a valid artifact was refused: %v", err)
	}
	if v.Tier != TierRegistry || v.Publisher != pub.pub || v.Registry != reg.pub {
		t.Errorf("verdict = %+v", v)
	}
	if a.Manifest.Name != "tech-news" {
		t.Errorf("manifest = %+v", a.Manifest)
	}
}

// Each of these is a thing somebody would actually try.
func TestTamperedArtifactsAreRefused(t *testing.T) {
	pub := newSigner(t)
	reg := newSigner(t)
	trust := Trust{Registries: []string{reg.pub}, AllowCommunity: true}

	t.Run("the module is swapped for different code", func(t *testing.T) {
		data := packed(t, manifestFor(pub.pub), fakeModule, pub, reg)
		// Same length, different bytes: the digest is what catches it.
		swapped := append([]byte{}, data...)
		swapped[len(swapped)-1] ^= 0xff
		if _, _, err := VerifyBytes(swapped, trust); err == nil {
			t.Fatal("a swapped module verified")
		}
	})

	t.Run("capabilities are widened after signing", func(t *testing.T) {
		m := manifestFor(pub.pub)
		data := packed(t, m, fakeModule, pub, reg)
		a, _ := Unpack(data)
		a.Manifest.Capabilities = append(a.Manifest.Capabilities, "http:*")
		if _, err := Verify(a, trust); err == nil {
			t.Fatal("widened capabilities verified")
		}
	})

	t.Run("a host function is added after signing", func(t *testing.T) {
		data := packed(t, manifestFor(pub.pub), fakeModule, pub, reg)
		a, _ := Unpack(data)
		a.Manifest.Host = append(a.Manifest.Host, FnAsk)
		if _, err := Verify(a, trust); err == nil {
			t.Fatal("an added host function verified")
		}
	})

	t.Run("the publisher signature is from a different key", func(t *testing.T) {
		other := newSigner(t)
		m := manifestFor(pub.pub) // manifest names pub
		data := packed(t, m, fakeModule, other, reg)
		if _, _, err := VerifyBytes(data, trust); err == nil {
			t.Fatal("somebody signed as a publisher they are not")
		}
	})

	t.Run("no publisher signature at all", func(t *testing.T) {
		m := manifestFor(pub.pub)
		probe, _ := Pack(m, fakeModule, nil)
		a, _ := Unpack(probe)
		// Only a registry signature.
		data, _ := Pack(a.Manifest, fakeModule, []Signature{Sign(&a.Manifest, RoleRegistry, reg.priv)})
		if _, _, err := VerifyBytes(data, trust); err == nil {
			t.Fatal("an unsigned artifact verified")
		}
	})

	t.Run("not a loop artifact at all", func(t *testing.T) {
		if _, _, err := VerifyBytes([]byte("just a wasm file"), trust); err == nil {
			t.Fatal("an arbitrary file verified")
		}
	})
}

func TestCommunityTierNeedsAnExplicitDecision(t *testing.T) {
	pub := newSigner(t)
	data := packed(t, manifestFor(pub.pub), fakeModule, pub)

	// Publisher-only, and the operator has not opted in: refused, with a
	// message that says what to do.
	_, _, err := VerifyBytes(data, Trust{})
	if err == nil {
		t.Fatal("unreviewed code installed by default")
	}
	if !strings.Contains(err.Error(), "--allow-community") {
		t.Errorf("the refusal does not say how to proceed: %v", err)
	}

	_, v, err := VerifyBytes(data, Trust{AllowCommunity: true})
	if err != nil {
		t.Fatalf("opting in did not work: %v", err)
	}
	if v.Tier != TierCommunity {
		t.Errorf("tier = %s", v.Tier)
	}
}

func TestARegistrySignatureFromAnUntrustedKeyDoesNotPromote(t *testing.T) {
	pub, rogue := newSigner(t), newSigner(t)
	data := packed(t, manifestFor(pub.pub), fakeModule, pub, rogue)

	// The signature is real; the key is not one this instance trusts. It must
	// fall back to community rather than count as reviewed.
	_, v, err := VerifyBytes(data, Trust{AllowCommunity: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Tier != TierCommunity || v.Registry != "" {
		t.Errorf("an untrusted countersignature promoted it: %+v", v)
	}
}

func TestRevokedKeysAreRefused(t *testing.T) {
	pub, reg := newSigner(t), newSigner(t)
	data := packed(t, manifestFor(pub.pub), fakeModule, pub, reg)

	trust := Trust{Registries: []string{reg.pub}, Revoked: []string{pub.pub}}
	if _, _, err := VerifyBytes(data, trust); err == nil {
		t.Fatal("a revoked publisher's artifact verified")
	}
	// Revoking the registry key drops it to community rather than passing.
	trust = Trust{Registries: []string{reg.pub}, Revoked: []string{reg.pub}}
	if _, _, err := VerifyBytes(data, trust); err == nil {
		t.Fatal("a revoked registry key still countersigned")
	}
}

// recordingStore stands in for the broker's persistence.
type recordingStore struct {
	grants  []store.Grant
	revoked []string
}

func (r *recordingStore) SaveGrant(g store.Grant) error {
	r.grants = append(r.grants, g)
	return nil
}

func (r *recordingStore) RevokeSubject(s string) (int64, error) {
	r.revoked = append(r.revoked, s)
	r.grants = nil
	return 0, nil
}

func newInstaller(t *testing.T, trust Trust) (*Installer, *recordingStore) {
	t.Helper()
	rec := &recordingStore{}
	return &Installer{Dir: t.TempDir(), Broker: rec, Trust: trust, Actor: "test"}, rec
}

func TestInstallGrantsExactlyWhatTheManifestDeclared(t *testing.T) {
	pub, reg := newSigner(t), newSigner(t)
	in, rec := newInstaller(t, Trust{Registries: []string{reg.pub}})
	data := packed(t, manifestFor(pub.pub), fakeModule, pub, reg)

	p, err := in.Install(data)
	if err != nil {
		t.Fatal(err)
	}
	if p.Verdict.Tier != TierRegistry {
		t.Errorf("tier = %s", p.Verdict.Tier)
	}

	var granted []string
	for _, g := range rec.grants {
		granted = append(granted, g.Capability+":"+g.Value)
	}
	joined := strings.Join(granted, " ")
	if !strings.Contains(joined, "http:hacker-news.firebaseio.com") {
		t.Errorf("declared host not granted: %v", granted)
	}
	// Nothing beyond what it asked for.
	if strings.Contains(joined, "http:*") {
		t.Errorf("a wildcard was granted: %v", granted)
	}

	// And the operator sees it in English.
	desc := strings.Join(p.Grants, "\n")
	for _, want := range []string{"make HTTP requests to hacker-news.firebaseio.com", "send you notifications"} {
		if !strings.Contains(desc, want) {
			t.Errorf("preview missing %q:\n%s", want, desc)
		}
	}
}

func TestAnUpgradeShowsWhatChangedAndDropsWhatItNoLongerAsksFor(t *testing.T) {
	pub, reg := newSigner(t), newSigner(t)
	in, rec := newInstaller(t, Trust{Registries: []string{reg.pub}})

	if _, err := in.Install(packed(t, manifestFor(pub.pub), fakeModule, pub, reg)); err != nil {
		t.Fatal(err)
	}

	// v2 wants the whole internet and no longer reads memory.
	next := manifestFor(pub.pub)
	next.Version = "2.0.0"
	next.Capabilities = []string{"http:*"}
	p, err := in.Inspect(packed(t, next, fakeModule, pub, reg))
	if err != nil {
		t.Fatal(err)
	}
	if p.Upgrade != "1.2.0" || p.Diff == nil || p.Diff.Same {
		t.Fatalf("upgrade not detected: %+v", p)
	}
	if !contains(p.Diff.Added, "http:*") {
		t.Errorf("the widened capability was not flagged: %+v", p.Diff)
	}
	if !contains(p.Diff.Removed, "memory:nexus") {
		t.Errorf("the dropped capability was not flagged: %+v", p.Diff)
	}

	if _, err := in.Install(packed(t, next, fakeModule, pub, reg)); err != nil {
		t.Fatal(err)
	}
	for _, g := range rec.grants {
		if g.Capability == "memory" {
			t.Error("an upgrade kept a capability it stopped asking for")
		}
	}
}

// The defence against a module being replaced on disk after approval.
func TestAModuleSwappedAfterInstallIsRefusedOnLoad(t *testing.T) {
	pub, reg := newSigner(t), newSigner(t)
	in, _ := newInstaller(t, Trust{Registries: []string{reg.pub}, AllowCommunity: true})
	if _, err := in.Install(packed(t, manifestFor(pub.pub), fakeModule, pub, reg)); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Load("tech-news"); err != nil {
		t.Fatalf("a freshly installed loop would not load: %v", err)
	}

	// The attacker replaces the file with a wholly valid artifact of their own —
	// self-consistent, correctly signed by THEM. Only the lockfile catches it.
	evil := newSigner(t)
	em := manifestFor(evil.pub)
	em.Capabilities = []string{"http:*"}
	replacement := packed(t, em, []byte("\x00asm\x01\x00\x00\x00 different code entirely"), evil)
	if err := os.WriteFile(filepath.Join(in.Dir, "tech-news.kloop"), replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := in.Load("tech-news")
	if err == nil {
		t.Fatal("a replaced module loaded")
	}
	if !strings.Contains(err.Error(), "not what the lockfile recorded") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
}

func TestRemovingTakesTheCapabilitiesWithIt(t *testing.T) {
	pub, reg := newSigner(t), newSigner(t)
	in, rec := newInstaller(t, Trust{Registries: []string{reg.pub}})
	if _, err := in.Install(packed(t, manifestFor(pub.pub), fakeModule, pub, reg)); err != nil {
		t.Fatal(err)
	}
	if err := in.Remove("tech-news"); err != nil {
		t.Fatal(err)
	}
	if len(rec.revoked) == 0 {
		t.Error("removal left the capabilities in place")
	}
	if entries, _ := in.Installed(); len(entries) != 0 {
		t.Errorf("lockfile still lists %+v", entries)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, "tech-news.kloop")); !os.IsNotExist(err) {
		t.Error("the artifact was left on disk")
	}
}

func TestTheLockfileSurvivesAndIsReadable(t *testing.T) {
	pub, reg := newSigner(t), newSigner(t)
	in, _ := newInstaller(t, Trust{Registries: []string{reg.pub}})
	if _, err := in.Install(packed(t, manifestFor(pub.pub), fakeModule, pub, reg)); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(LockPath(in.Dir))
	if err != nil {
		t.Fatal(err)
	}
	// Diffable and auditable means a human can read it.
	for _, want := range []string{"tech-news", "1.2.0", pub.pub, "registry", "installed_by"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("lockfile missing %q:\n%s", want, raw)
		}
	}

	entries, err := in.Installed()
	if err != nil || len(entries) != 1 || entries[0].Tier != TierRegistry {
		t.Errorf("entries = %+v, err %v", entries, err)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
