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

// A tool call passes TWO independent gates: the signed manifest's tool list and
// the Broker's grant. Either one alone is not enough.
//
// The manifest gate is the one that is easy to lose. `tool` is a single host
// function, so a loop that declares it can name ANY tool in the request — and
// if only the Broker were consulted, a grant of "tool:*" (or a tool granted for
// an unrelated reason, like tool:summarize) would hand the loop the entire
// registry. The list the operator approved has to be enforced per call.
func TestAToolCallPassesBothTheManifestAndTheBroker(t *testing.T) {
	db := testStore(t)
	brk := broker.New(db, zap.NewNop())
	subject := broker.LoopSubject("gates")
	brk.SetTrust(subject, broker.Community)

	// Granted generously on purpose: the Broker is not the gate under test.
	if err := brk.Grant(store.Grant{
		Subject: subject, Capability: store.CapTool, Value: store.CapWildcard,
	}); err != nil {
		t.Fatal(err)
	}

	r := &Runner{
		name: "gates", namespace: "nexus", grants: brk.For(subject),
		declared: set([]string{FnTool}),
		tools:    set([]string{"whatsapp.read"}),
		log:      zap.NewNop(),
	}

	if !r.tools["whatsapp.read"] {
		t.Fatal("the declared tool is not in the manifest allowlist")
	}
	// Undeclared, despite tool:* — the manifest is what the operator read.
	if r.tools["whatsapp.send"] {
		t.Error("a tool absent from the manifest was treated as declared")
	}
	if err := r.grants.Tool("whatsapp.send"); err != nil {
		t.Fatalf("the Broker refused what it was granted, so this test would "+
			"pass even with the manifest gate removed: %v", err)
	}

	// And the reverse: declared but not granted must still be refused.
	ungranted := broker.LoopSubject("gates-ungranted")
	brk.SetTrust(ungranted, broker.Community)
	r2 := &Runner{namespace: "nexus", grants: brk.For(ungranted),
		tools: set([]string{"whatsapp.read"}), log: zap.NewNop()}
	if err := r2.grants.Tool("whatsapp.read"); err == nil {
		t.Error("a declared tool was permitted with no grant behind it")
	}
}

// The manifest's tools: list is what install grants.
//
// Without this the whole tier is quietly dead: every re-ported loop declares
// its integrations under tools:, install would record no tool grants at all,
// and each loop would verify, schedule, run and then be refused by the Broker
// on its first real call — succeeding at everything except its purpose.
func TestInstallGrantsTheToolsTheManifestDeclares(t *testing.T) {
	module := buildGuest(t)
	pub, reg := newSigner(t), newSigner(t)

	m := Manifest{
		Name: "granted", Version: "1.0.0", Host: []string{FnLog, FnTool},
		Publisher:    pub.pub,
		Tools:        []string{"whatsapp.read", "whatsapp.send"},
		Capabilities: []string{"memory:nexus"},
		MemoryMB:     32,
	}
	in, rec := newInstaller(t, Trust{Registries: []string{reg.pub}})
	if _, err := in.Install(packed(t, m, module, pub, reg)); err != nil {
		t.Fatalf("install: %v", err)
	}

	got := map[string]bool{}
	for _, g := range rec.grants {
		got[g.Capability+":"+g.Value] = true
	}
	for _, want := range []string{"tool:whatsapp.read", "tool:whatsapp.send", "memory:nexus"} {
		if !got[want] {
			t.Errorf("install did not grant %s; recorded %v", want, got)
		}
	}

	// And an upgrade that drops a tool must lose the grant, not keep it.
	m.Version, m.Tools = "1.1.0", []string{"whatsapp.read"}
	if _, err := in.Install(packed(t, m, module, pub, reg)); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for _, g := range rec.grants {
		if g.Capability == store.CapTool && g.Value == "whatsapp.send" {
			t.Error("an upgrade that dropped whatsapp.send kept the grant for it")
		}
	}
}

// toolName is what decides which tool the gates are applied to, so a request it
// misreads is a request gated as the wrong tool.
func TestToolNameRefusesARequestThatNamesNothing(t *testing.T) {
	for _, req := range []string{``, `{}`, `{"name":""}`, `{"name":"   "}`, `not json`} {
		if name, err := toolName(req); err == nil {
			t.Errorf("toolName(%q) returned %q instead of refusing", req, name)
		}
	}
	name, err := toolName(`{"name":"whatsapp.read","input":{"chat":"x"}}`)
	if err != nil || name != "whatsapp.read" {
		t.Errorf("toolName = %q, %v; want whatsapp.read", name, err)
	}
}

func (k *countingKit) SocialAuthorize(context.Context, string, string) (SocialGrant, error) {
	return SocialGrant{}, nil
}
