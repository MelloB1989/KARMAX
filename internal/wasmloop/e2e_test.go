package wasmloop

import (
	"context"
	"github.com/MelloB1989/karmax/pkg/loopkit"
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

// A test kit holds no real conversation; it records that one was asked for.
func (k *countingKit) Session(key, kind string) loopkit.SessionHandle {
	return &countingSession{k: k, key: key}
}

type countingSession struct {
	k   *countingKit
	key string
}

func (s *countingSession) Send(_ context.Context, text string) (string, bool, error) {
	return "session reply for " + s.key, true, nil
}
func (s *countingSession) Close() error { return nil }

func (k *countingKit) SessionIn(key, kind, workdir, instructions string) loopkit.SessionHandle {
	return &countingSession{k: k, key: key}
}
