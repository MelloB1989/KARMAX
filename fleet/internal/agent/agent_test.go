package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

type fakeRunner struct {
	calls  [][]string
	stdins []string
	answer func(argv []string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	f.calls = append(f.calls, argv)
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdins = append(f.stdins, in)
	if f.answer == nil {
		return nil, nil
	}
	out, err := f.answer(argv)
	return []byte(out), err
}

func newAgent(r *fakeRunner) *Agent {
	return &Agent{Name: "agent-03", C: target.Container{Exec: []string{"docker"}, Name: "agent-03", Runner: r}}
}

const snapshot = `==pane==
4242
==current==
s1
==agents==
[{"pid":4242,"kind":"interactive","sessionId":"s1","name":"agent-03","status":"idle"},
 {"pid":5000,"kind":"interactive","sessionId":"s5","name":"agent-01","status":"idle"},
 {"pid":5100,"kind":"interactive","sessionId":"r1","name":"relay-from-agent-05","status":"busy"},
 {"pid":6000,"kind":"interactive","sessionId":"k1","name":"karmax","status":"idle"},
 {"id":"bg","kind":"background","sessionId":"b1","name":"x","state":"blocked"}]
==owners==
4242 agent-03 
5000 agent-01 
5100 agent-03 relay
6000  
==status==
{"session_id":"s1","rate_limits":{"five_hour":{"used_percentage":12,"resets_at":1791430000}}}
==transcripts==
2048 1791427770.5 /home/op/.claude/projects/-work/s1.jsonl
99 1791427000.0 /home/op/.claude/projects/-work/old.jsonl
==events==
{"t":1791427770,"event":"Stop","session":"s1"}
{"t":1791427771,"event":"StopFailure","session":"s1","raw":{"error_type":"rate_limit"}}
`

func TestParseSnapshot(t *testing.T) {
	o, err := parseSnapshot("agent-03", []byte(snapshot), time.Unix(1791427800, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !o.PaneAlive || o.PanePID != 4242 {
		t.Errorf("pane alive=%v pid=%d", o.PaneAlive, o.PanePID)
	}
	if len(o.Sessions) != 1 || o.Sessions[0].SessionID != "s1" {
		t.Errorf("sessions = %+v: only this container's own sessions — not other agents' (shared registry), not relays, not background rows", o.Sessions)
	}
	if w, ok := o.Status.FiveHour(); !ok || w.Used != 12 {
		t.Errorf("status = %+v", o.Status)
	}
	if o.TranscriptSize != 2048 || o.TranscriptMTime.Unix() != 1791427770 {
		t.Errorf("transcript size=%d mtime=%v", o.TranscriptSize, o.TranscriptMTime)
	}
	if len(o.Events) != 2 || o.Events[1].ErrorType() != "rate_limit" {
		t.Errorf("events = %+v", o.Events)
	}
}

func TestParseSnapshotWithADeadPane(t *testing.T) {
	o, err := parseSnapshot("agent-03", []byte("==pane==\n==current==\n==agents==\n[]\n==owners==\n==status==\n==transcripts==\n==events==\n"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if o.PaneAlive || o.Status != nil || len(o.Sessions) != 0 {
		t.Errorf("obs = %+v", o)
	}
}

func TestObserveRunsOneExecAndSetsContainerUp(t *testing.T) {
	r := &fakeRunner{answer: func(argv []string) (string, error) {
		if slices.Contains(argv, "inspect") {
			return "true", nil
		}
		return snapshot, nil
	}}
	o, err := newAgent(r).Observe(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !o.ContainerUp || len(r.calls) != 2 {
		t.Fatalf("up=%v calls=%d", o.ContainerUp, len(r.calls))
	}
}

func TestObserveOfAStoppedContainerExecsNothing(t *testing.T) {
	r := &fakeRunner{answer: func(argv []string) (string, error) { return "false", nil }}
	o, err := newAgent(r).Observe(context.Background(), time.Now())
	if err != nil || o.ContainerUp || len(r.calls) != 1 {
		t.Fatalf("up=%v calls=%d err=%v", o.ContainerUp, len(r.calls), err)
	}
}

func lastScript(r *fakeRunner) (string, []string) {
	c := r.calls[len(r.calls)-1]
	i := slices.Index(c, "-c")
	return c[i+1], c[i+3:]
}

// Rotation and restarts never type into the pane: they write the session to
// start from and kill the pane, and agent-run starts the next one.
func TestRestartWritesNextAndKillsThePane(t *testing.T) {
	r := &fakeRunner{}
	a := newAgent(r)
	if err := a.Restart(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	script, args := lastScript(r)
	if !strings.Contains(script, `/.fleet"`) || !strings.Contains(script, `$W/next`) || !strings.Contains(script, "kill-session") || !slices.Equal(args, []string{"resume", "s1"}) {
		t.Fatalf("script %q args %q", script, args)
	}
	if err := a.Fresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, args := lastScript(r); !slices.Equal(args, []string{"fresh", ""}) {
		t.Fatalf("fresh args %q", args)
	}
	if err := a.Restart(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, args := lastScript(r); !slices.Equal(args, []string{"continue", ""}) {
		t.Fatalf("unknown-session restart args %q", args)
	}
}

func TestPromptTypesLiterallyThenEnter(t *testing.T) {
	r := &fakeRunner{}
	if err := newAgent(r).Prompt(context.Background(), "fix it; rm -rf $HOME"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls[0][3:], []string{"tmux", "send-keys", "-t", "main", "-l", "fix it; rm -rf $HOME"}) {
		t.Fatalf("argv = %q", r.calls[0])
	}
	if !slices.Equal(r.calls[1][3:], []string{"tmux", "send-keys", "-t", "main", "Enter"}) {
		t.Fatalf("argv = %q", r.calls[1])
	}
}

func tarOf(files map[string]string) string {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"-work/s1.jsonl", "-work/s1/subagents/agent-a.jsonl"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	return buf.String()
}

func TestPullTranscriptSplitsMainAndSubagents(t *testing.T) {
	r := &fakeRunner{answer: func(argv []string) (string, error) {
		return tarOf(map[string]string{"-work/s1.jsonl": "{\"a\":1}\n", "-work/s1/subagents/agent-a.jsonl": "{\"b\":2}\n"}), nil
	}}
	tr, err := newAgent(r).PullTranscript(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Project != "-work" || string(tr.Main) != "{\"a\":1}\n" || string(tr.Sub["subagents/agent-a.jsonl"]) != "{\"b\":2}\n" {
		t.Fatalf("transcript = %q / %v", tr.Main, tr.Sub)
	}
}

func TestPullTranscriptOfAnUnknownSessionFails(t *testing.T) {
	r := &fakeRunner{answer: func(argv []string) (string, error) { return tarOf(nil), nil }}
	if _, err := newAgent(r).PullTranscript(context.Background(), "nope"); err == nil {
		t.Fatal("want an error for a session with no transcript")
	}
}

func TestParseWorktrees(t *testing.T) {
	out := `{"path":"/work/wt/fix","branch":"fleet/agent-03/fix","upstream":"origin/fleet/agent-03/fix","dirty":0,"unpushed":0}
{"path":"/work/wt/wip","branch":"fleet/agent-03/wip","upstream":"","dirty":3,"unpushed":2}
`
	wts, err := parseWorktrees([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || !wts[0].Safe() || wts[1].Safe() {
		t.Fatalf("worktrees = %+v", wts)
	}
}

// The relay runs in the receiver's container, on the receiver's subscription,
// with nothing but the messaging tools: the text comes from another host and
// is untrusted, so it must not be able to steer a session that can run code.
func TestRelayArgv(t *testing.T) {
	r := &fakeRunner{answer: func([]string) (string, error) { return "sent", nil }}
	a := newAgent(r)
	if err := a.Relay(context.Background(), "agent-03", "agent-05", "please rebase onto main"); err != nil {
		t.Fatal(err)
	}
	argv := r.calls[0]
	if !slices.Equal(argv[:6], []string{"docker", "exec", "-i", "-e", "FLEET_ROLE=relay", "agent-03"}) {
		t.Fatalf("argv = %q", argv)
	}
	rest := strings.Join(argv[6:], " ")
	// The container's settings put every session in bypass mode, so the relay
	// must override the mode and narrow the tool set itself: --allowedTools
	// alone only adds allow rules on top of bypass.
	for _, want := range []string{"claude -p", "--model haiku", "--name relay-from-agent-05",
		"--permission-mode dontAsk", "--tools SendMessage,ListAgents", "--allowedTools SendMessage,ListAgents", "--max-turns"} {
		if !strings.Contains(rest, want) {
			t.Errorf("relay argv lacks %q: %s", want, rest)
		}
	}
	if strings.Contains(rest, "dangerously") || strings.Contains(rest, "please rebase") {
		t.Errorf("relay argv must not bypass permissions or carry the text: %s", rest)
	}
	in := r.stdins[0]
	if !strings.Contains(in, "agent-03") || !strings.Contains(in, "please rebase onto main") || !strings.Contains(in, "agent-05") {
		t.Errorf("prompt = %q", in)
	}
}

// A failed `claude agents` must read as "could not observe", never as "no
// sessions" — which would restart every healthy agent on the host.
func TestAFailedSessionListIsAnObservationFailure(t *testing.T) {
	bad := strings.Replace(snapshot, "==agents==\n[", "==agents==\n!!FAILED\n[", 1)
	if _, err := parseSnapshot("agent-03", []byte(bad), time.Now()); err == nil {
		t.Fatal("a failed session list parsed as an observation")
	}
	if !strings.Contains(snapshotScript, "!!FAILED") {
		t.Fatal("the snapshot script does not mark a failed claude agents")
	}
}

func TestSnapshotCarriesTheCurrentSession(t *testing.T) {
	o, err := parseSnapshot("agent-03", []byte(snapshot), time.Now())
	if err != nil || o.CurrentSession != "s1" {
		t.Fatalf("current = %q, %v", o.CurrentSession, err)
	}
}

// The operator is not an address a reply can reach; a relay from them says so
// instead of telling the receiver to reply to "operator".
func TestRelayFromTheOperator(t *testing.T) {
	r := &fakeRunner{}
	if err := newAgent(r).Relay(context.Background(), "agent-03", "operator", "stop and push"); err != nil {
		t.Fatal(err)
	}
	if in := r.stdins[0]; !strings.Contains(in, "relayed from the operator") || strings.Contains(in, "fleetctl tell operator") {
		t.Fatalf("prompt = %q", in)
	}
}
