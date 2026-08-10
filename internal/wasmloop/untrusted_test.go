package wasmloop

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An unsigned artifact is refused by default, and accepted only when the
// operator says so for that one install.
func TestAnUnsignedArtifactNeedsAPerInstallDecision(t *testing.T) {
	module := buildGuest(t)
	m := Manifest{Name: "homemade", Version: "0.1.0", Host: []string{FnLog}, MemoryMB: 32}
	data, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := VerifyBytes(data, Trust{}); err == nil {
		t.Fatal("an unsigned artifact verified with no decision behind it")
	} else if !strings.Contains(err.Error(), "--untrusted") {
		t.Errorf("the refusal does not say how to proceed: %v", err)
	}

	_, v, err := VerifyBytes(data, Trust{AllowUntrusted: true})
	if err != nil {
		t.Fatalf("--untrusted did not accept an unsigned artifact: %v", err)
	}
	if v.Tier != TierUntrusted {
		t.Errorf("tier = %s, want %s", v.Tier, TierUntrusted)
	}
}

// The bytes are still pinned to the manifest. Untrusted relaxes WHO vouched for
// the code, never WHETHER the code is the code that was described — otherwise
// the preview an operator approves would be about a different module.
func TestUntrustedStillRequiresTheDigestToMatch(t *testing.T) {
	module := buildGuest(t)
	m := Manifest{Name: "homemade", Version: "0.1.0", Host: []string{FnLog}, MemoryMB: 32}
	data, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}

	a, err := Unpack(data)
	if err != nil {
		t.Fatal(err)
	}
	// Swap the module for different bytes, leaving the manifest's digest alone.
	a.Module = append(append([]byte{}, a.Module...), 0x00)
	tampered, err := Pack(a.Manifest, a.Module, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Pack recomputes the digest, so put the original claim back — this is
	// exactly what someone swapping a module would produce.
	swapped, err := Unpack(tampered)
	if err != nil {
		t.Fatal(err)
	}
	swapped.Manifest.SHA256 = m.SHA256
	if swapped.Manifest.SHA256 == "" {
		swapped.Manifest.SHA256 = a.Manifest.SHA256
	}
	repacked, err := Pack(swapped.Manifest, swapped.Module, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Pack recomputes SHA256 from the bytes, so force the mismatch directly.
	broken, err := Unpack(repacked)
	if err != nil {
		t.Fatal(err)
	}
	broken.Manifest.SHA256 = strings.Repeat("a", 64)
	if _, err := Verify(broken, Trust{AllowUntrusted: true}); err == nil {
		t.Fatal("--untrusted accepted a module that does not match its manifest")
	}
}

// Untrusted means nobody vouched for it. It does NOT mean it may do anything.
//
// This is the claim the warning makes to the operator, so it gets a test rather
// than a promise: an untrusted loop is refused a host function it did not
// declare, exactly like a registry-tier one.
func TestAnUntrustedLoopIsStillSandboxed(t *testing.T) {
	module := buildGuest(t)
	// Declares only log. The guest also tries recall.
	m := Manifest{Name: "homemade", Version: "0.1.0", Host: []string{FnLog}, MemoryMB: 32}
	data, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, v, err := VerifyBytes(data, Trust{AllowUntrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Tier != TierUntrusted {
		t.Fatalf("tier = %s", v.Tier)
	}

	core, logs := observer.New(zapcore.InfoLevel)
	log := zap.New(core)
	brk := broker.New(testStore(t), log)
	// Community trust, NOT Ungated: an unsigned loop must never be handed the
	// daemon's own authority.
	brk.SetTrust(broker.LoopSubject(m.Name), broker.Community)

	kit := &countingKit{}
	ctx := context.Background()
	r, err := NewRunner(ctx, a, Options{
		Namespace: "nexus", Kit: kit,
		Grants: brk.For(broker.LoopSubject(m.Name)), Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	if err := r.Run(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	var transcript strings.Builder
	for _, e := range logs.All() {
		for _, f := range e.Context {
			if f.Key == "message" {
				transcript.WriteString(f.String + "\n")
			}
		}
	}
	if strings.Contains(transcript.String(), "BREACH") {
		t.Fatalf("an untrusted loop escaped the sandbox:\n%s", transcript.String())
	}
	if kit.recalls != 0 {
		t.Errorf("an untrusted loop reached an undeclared host function %d times", kit.recalls)
	}
}

// The decision is recorded, so it survives the install that made it.
//
// Without this an --untrusted install succeeds and then refuses to start on the
// next boot — the daemon re-verifies from the lockfile with its own trust
// settings, which do not include a flag that was never persisted.
func TestAnUntrustedInstallStillLoadsAfterwards(t *testing.T) {
	module := buildGuest(t)
	m := Manifest{Name: "homemade", Version: "0.1.0", Host: []string{FnLog}, MemoryMB: 32}
	data, err := Pack(m, module, nil)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	rec := &recordingStore{}
	// Installed with the per-install decision.
	installer := &Installer{Dir: dir, Broker: rec, Trust: Trust{AllowUntrusted: true}, Actor: "dev"}
	p, err := installer.Install(data)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if p.Verdict.Tier != TierUntrusted {
		t.Fatalf("tier = %s", p.Verdict.Tier)
	}

	// The daemon loads with its own settings, which do NOT allow untrusted.
	daemon := &Installer{Dir: dir, Broker: rec, Trust: Trust{}}
	if _, err := daemon.Load("homemade"); err != nil {
		t.Fatalf("an untrusted loop was installed and then would not load: %v", err)
	}
}

// A community loop the operator accepted per-install also has to keep loading,
// on an instance that has not turned community trust on globally.
func TestACommunityInstallStillLoadsWithoutGlobalCommunityTrust(t *testing.T) {
	module := buildGuest(t)
	pub := newSigner(t)
	m := Manifest{Name: "signed-only", Version: "1.0.0", Publisher: pub.pub,
		Host: []string{FnLog}, MemoryMB: 32}
	data := packed(t, m, module, pub)

	dir := t.TempDir()
	rec := &recordingStore{}
	installer := &Installer{Dir: dir, Broker: rec, Trust: Trust{AllowUntrusted: true}, Actor: "dev"}
	p, err := installer.Install(data)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if p.Verdict.Tier != TierCommunity {
		t.Fatalf("tier = %s, want community — it IS publisher-signed", p.Verdict.Tier)
	}

	daemon := &Installer{Dir: dir, Broker: rec, Trust: Trust{}}
	if _, err := daemon.Load("signed-only"); err != nil {
		t.Fatalf("a community loop was installed and then would not load: %v", err)
	}
}

// The relaxation is per artifact, not per instance. A second unsigned loop that
// nobody approved must still be refused.
func TestTheRelaxationDoesNotLeakToOtherLoops(t *testing.T) {
	module := buildGuest(t)
	dir := t.TempDir()
	rec := &recordingStore{}

	approved, err := Pack(Manifest{Name: "approved", Version: "0.1.0",
		Host: []string{FnLog}, MemoryMB: 32}, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Installer{Dir: dir, Broker: rec,
		Trust: Trust{AllowUntrusted: true}, Actor: "dev"}).Install(approved); err != nil {
		t.Fatal(err)
	}

	other, err := Pack(Manifest{Name: "sneaked-in", Version: "0.1.0",
		Host: []string{FnLog}, MemoryMB: 32}, module, nil)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Installer{Dir: dir, Broker: rec, Trust: Trust{}}
	if _, err := daemon.Install(other); err == nil {
		t.Fatal("approving one unsigned loop let a different one install itself")
	}
}
