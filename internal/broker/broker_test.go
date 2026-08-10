package broker

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func newBroker(t *testing.T) (*Broker, *store.Store) {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, zap.NewNop()), s
}

func grant(t *testing.T, b *Broker, subject, capability, value string) {
	t.Helper()
	if err := b.Grant(store.Grant{Subject: subject, Capability: capability, Value: value}); err != nil {
		t.Fatal(err)
	}
}

func TestASubjectWithNoGrantsGetsNothing(t *testing.T) {
	b, _ := newBroker(t)
	h := b.For(LoopSubject("stranger"))

	if err := h.Tool("comms.send"); !IsDenied(err) {
		t.Errorf("tool call was permitted: %v", err)
	}
	if err := h.HTTP("api.github.com"); !IsDenied(err) {
		t.Errorf("HTTP was permitted: %v", err)
	}
	if err := h.Memory("nexus", true); !IsDenied(err) {
		t.Errorf("memory write was permitted: %v", err)
	}
	if err := h.Spend(1); !IsDenied(err) {
		t.Errorf("spending was permitted: %v", err)
	}
}

func TestAGrantPermitsExactlyWhatItNames(t *testing.T) {
	b, _ := newBroker(t)
	sub := LoopSubject("digest")
	grant(t, b, sub, store.CapHTTP, "api.github.com")
	grant(t, b, sub, store.CapTool, "memory.recall")

	h := b.For(sub)
	if err := h.HTTP("api.github.com"); err != nil {
		t.Errorf("the granted host was refused: %v", err)
	}
	if err := h.HTTP("evil.example.com"); !IsDenied(err) {
		t.Error("an ungranted host was permitted")
	}
	if err := h.Tool("memory.recall"); err != nil {
		t.Errorf("the granted tool was refused: %v", err)
	}
	// The near miss that matters: a different tool, not a different class.
	if err := h.Tool("comms.send"); !IsDenied(err) {
		t.Error("an ungranted tool was permitted")
	}
}

func TestWildcardsMatchOnlyWhatTheySay(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "anything", true},
		{"memory.recall", "memory.recall", true},
		{"memory.recall", "memory.write", false},
		{"memory.*", "memory.recall", true},
		{"memory.*", "comms.send", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com.evil.net", false},
		{"*.github.com", "api.gitlab.com", false},
		// The classic bypass: a prefix that is not a segment boundary.
		{"api.*", "api.github.com", true},
		{"api.*", "apiXgithub.com", false},
	}
	for _, tc := range cases {
		if got := matches(tc.pattern, tc.value); got != tc.want {
			t.Errorf("matches(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestReadAccessDoesNotImplyWrite(t *testing.T) {
	b, _ := newBroker(t)
	sub := LoopSubject("reader")
	grant(t, b, sub, store.CapMemory, store.MemoryValue("nexus", false))

	h := b.For(sub)
	if err := h.Memory("nexus", false); err != nil {
		t.Errorf("granted read was refused: %v", err)
	}
	if err := h.Memory("nexus", true); !IsDenied(err) {
		t.Error("a read grant permitted a write")
	}
	if err := h.Memory("someone-else", false); !IsDenied(err) {
		t.Error("a grant on one namespace permitted another")
	}
}

func TestSpendingStopsAtTheCeiling(t *testing.T) {
	b, _ := newBroker(t)
	sub := LoopSubject("expensive")
	grant(t, b, sub, store.CapSpend, "1000")
	h := b.For(sub)

	if err := h.Spend(600); err != nil {
		t.Fatalf("first spend refused: %v", err)
	}
	if err := h.Spend(300); err != nil {
		t.Fatalf("second spend refused: %v", err)
	}
	// 900 spent, 1000 allowed: this one does not fit and must not happen.
	if err := h.Spend(200); !IsDenied(err) {
		t.Error("spending past the ceiling was permitted")
	}
	if err := h.Spend(100); err != nil {
		t.Errorf("a spend that exactly fits was refused: %v", err)
	}
}

func TestUngatedSubjectsPassButAreStillMetered(t *testing.T) {
	b, s := newBroker(t)
	sub := LoopSubject("compiled-in")
	b.SetTrust(sub, Ungated)

	if err := b.For(sub).Tool("anything.at.all"); err != nil {
		t.Fatalf("an ungated subject was refused: %v", err)
	}
	readings, err := s.Meter(time.Now().AddDate(0, 0, -1), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) == 0 || readings[0].Allowed == 0 {
		t.Error("an ungated call was not metered — the half that is not theatre")
	}
}

func TestRefusalsAreMetered(t *testing.T) {
	b, s := newBroker(t)
	sub := LoopSubject("nosy")
	_ = b.For(sub).Tool("comms.send")

	readings, _ := s.Meter(time.Now().AddDate(0, 0, -1), 10)
	if len(readings) == 0 || readings[0].Refused == 0 {
		t.Errorf("a refusal left no trace: %+v", readings)
	}
}

func TestRevokingTakesEffect(t *testing.T) {
	b, _ := newBroker(t)
	sub := LoopSubject("temp")
	grant(t, b, sub, store.CapHTTP, "api.github.com")
	if err := b.For(sub).HTTP("api.github.com"); err != nil {
		t.Fatal(err)
	}

	// Through the broker, so the cached decision is dropped with it.
	if err := b.Revoke(sub, store.CapHTTP, "api.github.com"); err != nil {
		t.Fatal(err)
	}
	if err := b.For(sub).HTTP("api.github.com"); !IsDenied(err) {
		t.Error("a revoked capability still worked")
	}
}

func TestAnExpiredGrantStopsWorking(t *testing.T) {
	b, s := newBroker(t)
	sub := LoopSubject("expiring")
	past := time.Now().Add(-time.Minute)
	if err := s.SaveGrant(store.Grant{
		Subject: sub, Capability: store.CapHTTP, Value: "api.github.com", ExpiresAt: &past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.For(sub).HTTP("api.github.com"); !IsDenied(err) {
		t.Error("an expired grant was honoured")
	}
}

func TestGrantsAreDescribedInEnglish(t *testing.T) {
	b, _ := newBroker(t)
	sub := LoopSubject("readable")
	grant(t, b, sub, store.CapHTTP, "*")
	grant(t, b, sub, store.CapMemory, store.MemoryValue("nexus", true))

	lines, err := b.Describe(sub)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	// The operator approving this at install time has to understand it.
	for _, want := range []string{"ANY host", "WRITE memory in nexus"} {
		if !contains(joined, want) {
			t.Errorf("description missing %q:\n%s", want, joined)
		}
	}

	empty, _ := b.Describe(LoopSubject("nobody"))
	if len(empty) != 1 || !contains(empty[0], "nothing") {
		t.Errorf("a subject with no grants described as %v", empty)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
