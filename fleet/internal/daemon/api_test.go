package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
)

func TestRowCarriesIdleSecondsModelAlertsAndRecordedAt(t *testing.T) {
	f, boxes, _ := newFleet(t)
	st, _ := observe.ParseStatus([]byte(`{"session_id":"s-agent-03","model":{"id":"claude-sonnet","display_name":"Sonnet 5.5"}}`))
	boxes["agent-03"].obs.Status = st
	f.Tick(context.Background(), t0)
	s := f.State("agent-03")
	s.Alerted = map[string]bool{"stuck": true, "gone": false}
	s.LastBusy = t0.Add(-90 * time.Second)
	f.setState(s)

	var row Row
	for _, r := range f.Status(t0) {
		if r.Agent == "agent-03" {
			row = r
		}
	}
	if row.IdleS != 90 || row.Model != "Sonnet 5.5" || !row.RecordedAt.Equal(t0) {
		t.Errorf("row = %+v", row)
	}
	if len(row.Alerts) != 1 || row.Alerts[0] != "stuck" {
		t.Errorf("alerts = %v, want only the active one", row.Alerts)
	}
	for _, r := range f.Status(t0) {
		if r.Alerts == nil {
			t.Errorf("%s: alerts must be [] not null", r.Agent)
		}
	}
}

func TestRotateDryRunSaysWhatItWouldDoAndDoesNothing(t *testing.T) {
	f, _, _ := newFleet(t)
	if err := f.Assign(context.Background(), "agent-03", "T1", "operator", "", t0); err != nil {
		t.Fatal(err)
	}
	plan, err := f.Rotate("agent-03", true, t0)
	if err != nil || plan.Closes != "T1" || plan.When != "next idle tick" {
		t.Fatalf("dry run = %+v, %v", plan, err)
	}
	if st := f.State("agent-03"); st.Done {
		t.Fatal("a dry run marked the agent done")
	}
	if tk, _ := f.db.Task("T1"); !tk.Closed.IsZero() {
		t.Fatal("a dry run closed the task")
	}
	if _, err := f.Rotate("agent-03", false, t0); err != nil {
		t.Fatal(err)
	}
	if tk, _ := f.db.Task("T1"); tk.Outcome != "rotated" {
		t.Fatalf("task after rotate = %+v", tk)
	}
	if _, err := f.Rotate("nobody", true, t0); err == nil {
		t.Fatal("rotating an unknown agent must fail, even as a dry run")
	}
}

func TestTodayCountsFromLocalMidnight(t *testing.T) {
	f, _, _ := newFleet(t)
	ist := time.FixedZone("IST", 5*3600+1800)
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, ist)
	_ = f.db.OpenTask("old", "agent-03", "operator", now.Add(-30*time.Hour))
	_ = f.db.CloseTask("old", now.Add(-26*time.Hour), "done", "", "")
	_ = f.db.OpenTask("new", "agent-03", "operator", now.Add(-3*time.Hour))
	_ = f.db.CloseTask("new", time.Date(2026, 10, 11, 0, 30, 0, 0, ist), "done", "", "")
	_ = f.db.EndSession(ledger.Session{ID: "s1", Agent: "agent-03", Ended: now.Add(-time.Hour), CostUSD: 0.5, Output: 100})
	_ = f.db.EndSession(ledger.Session{ID: "s0", Agent: "agent-03", Ended: now.Add(-20 * time.Hour), CostUSD: 9, Output: 9})

	got, err := f.Today(now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Since.Equal(time.Date(2026, 10, 11, 0, 0, 0, 0, ist)) {
		t.Errorf("since = %v, want local midnight", got.Since)
	}
	if got.TasksClosed != 1 || got.SpendUSD != 0.5 || got.OutputTokens != 100 {
		t.Errorf("today = %+v", got)
	}
}

func TestPeekAndPromptGoThroughTheBox(t *testing.T) {
	f, boxes, _ := newFleet(t)
	out, err := f.Peek(context.Background(), "agent-03", 0)
	if err != nil || out != "screen" || boxes["agent-03"].calls[len(boxes["agent-03"].calls)-1] != "peek 80" {
		t.Fatalf("peek = %q, %v, calls %v", out, err, boxes["agent-03"].calls)
	}
	if err := f.Prompt(context.Background(), "agent-03", "hello"); err != nil {
		t.Fatal(err)
	}
	if c := boxes["agent-03"].calls; c[len(c)-1] != "prompt hello" {
		t.Errorf("calls = %v", c)
	}
	if err := f.Prompt(context.Background(), "agent-03", "  "); err == nil {
		t.Error("an empty prompt must be refused")
	}
	if _, err := f.Peek(context.Background(), "nobody", 5); err == nil {
		t.Error("peeking an unknown agent must fail")
	}
}

// Two events in the same second are both returned exactly once across two pages.
func TestEventsPagesByIDWithoutLossOrRepeat(t *testing.T) {
	f, _, _ := newFleet(t)
	for _, k := range []string{"a", "b", "c"} {
		_ = f.db.AddEvent(ledger.Event{Agent: "agent-03", At: t0, Kind: k})
	}
	p1, _ := f.Events(EventsQuery{Limit: 2})
	p2, _ := f.Events(EventsQuery{After: p1[len(p1)-1].ID, Limit: 2})
	var kinds []string
	for _, e := range append(p1, p2...) {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, "") != "abc" {
		t.Fatalf("pages gave %v", kinds)
	}
	if none, err := f.Events(EventsQuery{After: 9999}); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("an empty page must be [] not null: %v %v", none, err)
	}
}

func TestUpAndDownRunComposeOnEachHostThatRunsSomething(t *testing.T) {
	f, _, _ := newFleet(t)
	dir := filepath.Join(f.cfg.StateDir, "render")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "compose.pc2.yaml"), []byte("services:\n  agent-05:\n    image: img:2\n    container_name: agent-05\n    environment: {FLEET_CLAUDE_MD: NEW}\n"), 0o600)
	var ran []string
	old, oldDocker := HostRun, HostDocker
	defer func() { HostRun, HostDocker = old, oldDocker }()
	HostRun = func(_ context.Context, argv []string, w io.Writer) error {
		ran = append(ran, strings.Join(argv, " "))
		return nil
	}
	HostDocker = func(_ context.Context, argv []string) ([]byte, error) {
		switch {
		case strings.Contains(strings.Join(argv, " "), "image inspect"):
			return []byte("sha-2\n"), nil
		case argv[len(argv)-2] == "inspect":
			return []byte(`[{"Image":"sha-2","Config":{"Env":["FLEET_CLAUDE_MD=OLD"],"Labels":{}}}]`), nil
		}
		return nil, errors.New("not a read-only call")
	}
	out, err := f.Up(context.Background(), []string{"pc2"})
	if err != nil || len(ran) != 1 || !strings.HasPrefix(ran[0], "docker --context pc2 compose -p fleet -f ") ||
		!strings.HasSuffix(ran[0], "up -d --remove-orphans") {
		t.Fatalf("up: ran %v, out %q, %v", ran, out, err)
	}
	// The route says what the CLI says: the plan, then the command.
	plan, cmd := strings.Index(out, "agent-05 will be RECREATED"), strings.Index(out, "$ docker")
	if plan < 0 || cmd < plan {
		t.Fatalf("out lacks the plan before the command:\n%s", out)
	}
	if _, err := f.Down(context.Background(), []string{"mars"}); err == nil {
		t.Fatal("an unknown host must be refused")
	}
}
