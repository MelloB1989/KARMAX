package wasmloop

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The whole path: sign, install, load, run — with the Broker deciding.

// countingKit records what the guest actually managed to reach.
type countingKit struct {
	nullKit
	recalls, notifies int
	urls              []string
}

func (k *countingKit) Recall(q string, n int) ([]string, error) {
	k.recalls++
	return []string{"a memory about " + q}, nil
}

func (k *countingKit) Notify(title, body string) error {
	k.notifies++
	return nil
}

func (k *countingKit) HTTP(_ context.Context, method, url string, _ map[string]string, _ string) (string, int, error) {
	k.urls = append(k.urls, url)
	return `{"ok":true}`, 200, nil
}

func TestPublishInstallLoadRun(t *testing.T) {
	module := buildGuest(t)
	pub, reg := newSigner(t), newSigner(t)

	m := Manifest{
		Name: "escape-attempt", Version: "1.0.0", Description: "tries things",
		Publisher: pub.pub,
		Host:      []string{FnLog},
		MemoryMB:  32,
	}
	data := packed(t, m, module, pub, reg)

	rec := &recordingStore{}
	in := &Installer{
		Dir: t.TempDir(), Broker: rec,
		Trust: Trust{Registries: []string{reg.pub}}, Actor: "test",
	}

	p, err := in.Install(data)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if p.Verdict.Tier != TierRegistry {
		t.Fatalf("tier = %s", p.Verdict.Tier)
	}

	// Loaded back through the lockfile, which is how the daemon does it.
	a, err := in.Load("escape-attempt")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	core, logs := observer.New(zapcore.InfoLevel)
	log := zap.New(core)
	brk := broker.New(nil, log)
	brk.SetTrust(broker.LoopSubject(m.Name), broker.Ungated)

	kit := &countingKit{}
	ctx := context.Background()
	r, err := NewRunner(ctx, a, Options{
		Namespace: "nexus", Kit: kit,
		Grants: brk.For(broker.LoopSubject(m.Name)), Log: log,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	defer r.Close(ctx)

	if err := r.Run(ctx, 30*time.Second); err != nil {
		t.Fatalf("run: %v", err)
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
		t.Fatalf("the installed loop escaped:\n%s", transcript.String())
	}
	// The guest calls Recall, which its manifest does not declare. The host must
	// have refused it before the Kit was ever reached.
	if kit.recalls != 0 {
		t.Errorf("an undeclared host function reached the Kit %d times", kit.recalls)
	}
}

// The Broker is the second gate, independent of the manifest: a loop can
// declare a capability and still be refused by the operator.
func TestADeclaredCapabilityIsStillSubjectToTheBroker(t *testing.T) {
	module := buildGuest(t)

	// This time recall IS declared, so the manifest gate passes.
	m := Manifest{Name: "escape-attempt", Version: "1.0.0",
		Host: []string{FnLog, FnRecall}, MemoryMB: 32}
	packedBytes, _ := Pack(m, module, nil)
	a, _ := Unpack(packedBytes)

	core, logs := observer.New(zapcore.InfoLevel)
	log := zap.New(core)

	// A real broker with a real store, granting nothing.
	db := testStore(t)
	brk := broker.New(db, log)
	brk.SetTrust(broker.LoopSubject(m.Name), broker.Community)

	kit := &countingKit{}
	ctx := context.Background()
	r, err := NewRunner(ctx, a, Options{Namespace: "nexus", Kit: kit,
		Grants: brk.For(broker.LoopSubject(m.Name)), Log: log})
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
	if kit.recalls != 0 {
		t.Errorf("the Broker let through a capability it never granted (%d calls)", kit.recalls)
	}
	if !strings.Contains(transcript.String(), "undeclared host function refused") {
		t.Errorf("the guest did not see a refusal:\n%s", transcript.String())
	}

	// And with the grant in place, the same call works — otherwise this test
	// would pass against a broker that refuses everything.
	// The class recall is actually checked against — memory, not tool. Granting
	// the wrong class here is what the old code effectively did.
	if err := brk.Grant(store.Grant{
		Subject: broker.LoopSubject(m.Name), Capability: store.CapMemory, Value: "nexus",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if kit.recalls == 0 {
		t.Error("a granted capability was still refused")
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(t.TempDir()+"/k.db", zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The bug this pins: every host-function capability was checked as if it were a
// tool, so a loop granted "memory:nexus" was refused when it called recall —
// the check asked for tool "memory:nexus" and the grant was memory "nexus".
// cold-scan would have installed cleanly and then quietly done nothing.
func TestEachHostFunctionIsCheckedAgainstItsOwnCapabilityClass(t *testing.T) {
	db := testStore(t)
	log := zap.NewNop()
	brk := broker.New(db, log)
	subject := broker.LoopSubject("classes")
	brk.SetTrust(subject, broker.Community)

	// Granted exactly what a manifest would declare.
	for _, g := range []store.Grant{
		{Subject: subject, Capability: store.CapMemory, Value: "nexus"},
		{Subject: subject, Capability: store.CapMemory, Value: "nexus:write"},
		{Subject: subject, Capability: store.CapTool, Value: "summarize"},
		{Subject: subject, Capability: store.CapChannel, Value: "whatsapp"},
	} {
		if err := brk.Grant(g); err != nil {
			t.Fatal(err)
		}
	}

	r := &Runner{namespace: "nexus", grants: brk.For(subject)}
	for _, tc := range []struct {
		fn    string
		allow bool
	}{
		{FnRecall, true},    // memory:nexus
		{FnRemember, true},  // memory:nexus:write
		{FnChatGet, true},   // memory:nexus
		{FnChatSave, true},  // memory:nexus:write
		{FnSummarize, true}, // tool:summarize
		{FnSendWA, true},    // channel:whatsapp
		{FnHarness, false},  // tool:harness — not granted
		{FnNotify, false},   // tool:app.push — not granted
		{FnAsk, false},      // tool:agent.ask — not granted
	} {
		capFor, ok := capabilityFor[tc.fn]
		if !ok {
			t.Fatalf("%s has no capability mapping", tc.fn)
		}
		class, value := capFor(r)
		err := r.grants.Check(class, value)
		if tc.allow && err != nil {
			t.Errorf("%s (%s:%s) was refused despite being granted: %v", tc.fn, class, value, err)
		}
		if !tc.allow && err == nil {
			t.Errorf("%s (%s:%s) was permitted without a grant", tc.fn, class, value)
		}
	}
}
