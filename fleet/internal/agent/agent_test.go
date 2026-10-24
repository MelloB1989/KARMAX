package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
==boot==
b0e1
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
	if !o.PaneAlive || o.PanePID != 4242 || o.Proc != "b0e1/4242" {
		t.Errorf("pane alive=%v pid=%d proc=%q", o.PaneAlive, o.PanePID, o.Proc)
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

// A container Docker could not start is observed as down, with Docker's
// error, and nothing is exec'd into it.
func TestObserveReportsAContainerDockerFailedToStart(t *testing.T) {
	r := &fakeRunner{answer: func([]string) (string, error) {
		return "false\texited\terror mounting \"/home/op/code\": no such device", nil
	}}
	o, err := newAgent(r).Observe(context.Background(), time.Now())
	if err != nil || o.ContainerUp || !strings.Contains(o.ContainerFailed, "no such device") || len(r.calls) != 1 {
		t.Fatalf("obs = %+v err=%v calls=%d", o, err, len(r.calls))
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
	if !strings.Contains(script, `/.fleet${2:+/$2}"`) || !strings.Contains(script, `$W/next`) || !strings.Contains(script, "kill-session") || !slices.Equal(args, []string{"main", "", "resume", "s1"}) {
		t.Fatalf("script %q args %q", script, args)
	}
	if err := a.Fresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, args := lastScript(r); !slices.Equal(args, []string{"main", "", "fresh", ""}) {
		t.Fatalf("fresh args %q", args)
	}
	if err := a.Restart(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, args := lastScript(r); !slices.Equal(args, []string{"main", "", "continue", ""}) {
		t.Fatalf("unknown-session restart args %q", args)
	}
}

func TestPromptTypesLiterallyThenEnter(t *testing.T) {
	r := &fakeRunner{}
	if err := newAgent(r).Prompt(context.Background(), "fix it; rm -rf $HOME"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls[0][3:], []string{"tmux", "send-keys", "-t", "=main:", "-l", "--", "fix it; rm -rf $HOME"}) {
		t.Fatalf("argv = %q", r.calls[0])
	}
	if !slices.Equal(r.calls[1][3:], []string{"tmux", "send-keys", "-t", "=main:", "Enter"}) {
		t.Fatalf("argv = %q", r.calls[1])
	}
}

// A slot's actions reach its own pane and directory, never slot 1's.
func TestASlotsActionsGoToItsOwnPaneAndDirectory(t *testing.T) {
	r := &fakeRunner{}
	a := newAgent(r)
	a.Name, a.Slot = "agent-03:2", 2
	ctx := context.Background()
	if err := a.Prompt(ctx, "go on"); err != nil {
		t.Fatal(err)
	}
	if err := a.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Peek(ctx, 5); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if i := slices.Index(c, "-t"); i < 0 || c[i+1] != "=main-2:" {
			t.Errorf("call %q: want the target =main-2:", c)
		}
	}
	if err := a.Restart(ctx, "s7"); err != nil {
		t.Fatal(err)
	}
	if _, args := lastScript(r); !slices.Equal(args, []string{"main-2", "slot-2", "resume", "s7"}) {
		t.Errorf("restart args %q", args)
	}
	if got := a.AttachArgv(); !slices.Contains(got, "=main-2") {
		t.Errorf("attach %q", got)
	}
}

func TestInterruptSendsEscapeToThePane(t *testing.T) {
	r := &fakeRunner{}
	if err := newAgent(r).Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0][3:], []string{"tmux", "send-keys", "-t", "=main:", "Escape"}) {
		t.Fatalf("calls = %q", r.calls)
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
{"path":"/work/wt/pushed","branch":"fleet/agent-03/pushed","upstream":"","dirty":0,"unpushed":0}
{"path":"/work/wt/broken","branch":"","upstream":"","dirty":-1,"unpushed":-1}
`
	wts, err := parseWorktrees([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	// Safe is clean and on a remote, upstream or not; git failing is not safe.
	if len(wts) != 4 || !wts[0].Safe() || wts[1].Safe() || !wts[2].Safe() || wts[3].Safe() {
		t.Fatalf("worktrees = %+v", wts)
	}
}

// The relay runs in the receiver's container, on the receiver's subscription,
// with nothing but the messaging tools: the text comes from another host and
// is untrusted, so it must not be able to steer a session that can run code.
// relayStream is what a relay's claude prints (stream-json): a SendMessage
// to `to` and the tool's result.
func relayStream(to string, success bool, msg string) string {
	return `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"SendMessage","input":{"to":"` + to + `","message":"hi"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"{\"success\":` + map[bool]string{true: "true", false: "false"}[success] + `,\"message\":\"` + msg + `\"}"}]}]}}
{"type":"result","subtype":"success","is_error":false,"result":"done"}
`
}

func TestRelayArgv(t *testing.T) {
	r := &fakeRunner{answer: func([]string) (string, error) { return relayStream("agent-03", true, "sent"), nil }}
	a := newAgent(r)
	if err := a.Relay(context.Background(), "agent-03", "agent-05", "please rebase onto main"); err != nil {
		t.Fatal(err)
	}
	argv := r.calls[0]
	if !slices.Equal(argv[:6], []string{"docker", "exec", "-i", "-e", "FLEET_ROLE=relay", "agent-03"}) {
		t.Fatalf("argv = %q", argv)
	}
	rest := strings.Join(argv[6:], " ")
	// Claude Code holds a peer message whose sender runs in another permission
	// mode ("mode-mismatch"), and a -p receiver never reads a held one — so the
	// relay runs in bypass, like every receiver. What it can do is the tool
	// set: only the messaging tools, and no MCP server at all (--tools does not
	// cover MCP tools, which bypass would run unasked).
	for _, want := range []string{"claude -p", "--model haiku", "--name relay-from-agent-05",
		"--permission-mode bypassPermissions", "--tools SendMessage,ListAgents", "--allowedTools SendMessage,ListAgents",
		"--strict-mcp-config", "--max-turns", `--settings {"disableAllHooks":true}`, "--output-format stream-json --verbose"} {
		if !strings.Contains(rest, want) {
			t.Errorf("relay argv lacks %q: %s", want, rest)
		}
	}
	if strings.Contains(rest, "--mcp-config") || strings.Contains(rest, "please rebase") {
		t.Errorf("relay argv must load no MCP server and not carry the text: %s", rest)
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
	r := &fakeRunner{answer: func([]string) (string, error) { return relayStream("agent-03", true, "sent"), nil }}
	if err := newAgent(r).Relay(context.Background(), "agent-03", "operator", "stop and push"); err != nil {
		t.Fatal(err)
	}
	if in := r.stdins[0]; !strings.Contains(in, "relayed from the operator") || strings.Contains(in, "fleetctl tell operator") {
		t.Fatalf("prompt = %q", in)
	}
}

func TestPromptStartingWithDashIsText(t *testing.T) {
	r := &fakeRunner{}
	if err := newAgent(r).Prompt(context.Background(), "-1 is not a flag"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.calls[0][3:], []string{"tmux", "send-keys", "-t", "=main:", "-l", "--", "-1 is not a flag"}) {
		t.Fatalf("call = %v: text must follow --", r.calls[0])
	}
}

// The relay's claude exits 0 whether or not its SendMessage got through (no
// session by that name: the agent's session is restarting, or rotating). A
// relay is delivered only when the tool itself says so, for the receiver.
func TestARelayIsDeliveredOnlyWhenSendMessageSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		ok        bool
	}{
		{"sent", relayStream("agent-03", true, "Message sent"), true},
		{"no such session", relayStream("agent-03", false, "No agent named 'agent-03' is reachable."), false},
		{"sent to someone else", relayStream("agent-05", true, "Message sent"), false},
		{"never sent", `{"type":"assistant","message":{"content":[{"type":"text","text":"I will not."}]}}` + "\n" + `{"type":"result","subtype":"success","result":"I will not."}`, false},
		{"plain text", "sent", false},
	} {
		r := &fakeRunner{answer: func([]string) (string, error) { return tc.out, nil }}
		err := newAgent(r).Relay(context.Background(), "agent-03", "operator", "go")
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	r := &fakeRunner{answer: func([]string) (string, error) {
		return relayStream("agent-03", false, "No agent named 'agent-03' is reachable."), nil
	}}
	if err := newAgent(r).Relay(context.Background(), "agent-03", "operator", "go"); err == nil || !strings.Contains(err.Error(), "No agent named") {
		t.Errorf("the reason is lost: %v", err)
	}
}

// Messages fleetctl tell queued while fleetd was away come with the
// snapshot, oldest first; a line that is not a whole message is skipped.
func TestTheSnapshotCarriesTheOutbox(t *testing.T) {
	snap := "==pane==\n==current==\n==agents==\n[]\n==outbox==\n" +
		`1700000002-9.json {"from":"agent-03","to":"karmax","text":"DONE T2","at":"2026-10-16T01:00:02Z"}` + "\n" +
		`1700000001-9.json {"from":"agent-03","to":"operator","text":"DONE T1\nbranch x","at":"2026-10-16T01:00:01Z"}` + "\n" +
		`1700000003-9.json {"from":"agent-03","to":` + "\n" +
		"==events==\n"
	o, err := parseSnapshot("agent-03", []byte(snap), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Outbox) != 2 || o.Outbox[0].File != "1700000001-9.json" || o.Outbox[0].Text != "DONE T1\nbranch x" ||
		o.Outbox[0].To != "operator" || o.Outbox[1].To != "karmax" || !o.Outbox[0].At.Equal(time.Date(2026, 10, 16, 1, 0, 1, 0, time.UTC)) {
		t.Fatalf("outbox = %+v", o.Outbox)
	}
	if !strings.Contains(snapshotScript, "==outbox==") {
		t.Fatal("the snapshot script does not read the outbox")
	}
}

func TestAckOutboxRemovesOnlyTheNamedFiles(t *testing.T) {
	r := &fakeRunner{}
	if err := newAgent(r).AckOutbox(context.Background(), []string{"1-2.json", "3-4.json"}); err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(r.calls[0], " ")
	if !strings.Contains(argv, "1-2.json 3-4.json") || !strings.Contains(argv, "outbox") {
		t.Fatalf("argv = %s", argv)
	}
}

// What a rotated session started and left running — a job it detached with
// setsid and nohup, a dev server — is ended: it carries the session's id in
// its environment, and nothing else does (claude itself does not).
func TestKillSessionProcsEndsWhatTheSessionLeftRunning(t *testing.T) {
	r := &fakeRunner{answer: func([]string) (string, error) { return "9147\n9150\n", nil }}
	n, err := newAgent(r).KillSessionProcs(context.Background(), "961a95d2")
	if err != nil || n != 2 {
		t.Fatalf("killed %d, %v", n, err)
	}
	argv := strings.Join(r.calls[0], " ")
	if !strings.Contains(argv, "CLAUDE_CODE_SESSION_ID=$1") || !strings.Contains(argv, "FLEET_AGENT=$2") ||
		!strings.HasSuffix(argv, "961a95d2 agent-03") {
		t.Fatalf("argv = %s", argv)
	}
	if _, err := newAgent(r).KillSessionProcs(context.Background(), ""); err == nil {
		t.Fatal("no session id must kill nothing")
	}
}

// The script itself, on this machine's /proc: the session's detached job dies;
// the same session id under another agent, and another session, live on.
func TestKillSessionProcsOnRealProcesses(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("no /proc")
	}
	start := func(env ...string) *exec.Cmd {
		c := exec.Command("setsid", "sleep", "60")
		c.Env = append(os.Environ(), env...)
		if err := c.Start(); err != nil {
			t.Skip("cannot start setsid sleep:", err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
		// Until it has exec'd, the child's environ is still the test's: under
		// load the script would look before it is there.
		for i := 0; i < 200; i++ {
			if b, _ := os.ReadFile("/proc/" + strconv.Itoa(c.Process.Pid) + "/environ"); strings.Contains(string(b), env[0]) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return c
	}
	mine := start("CLAUDE_CODE_SESSION_ID=s-gone", "FLEET_AGENT=agent-03")
	other := start("CLAUDE_CODE_SESSION_ID=s-gone", "FLEET_AGENT=agent-04")
	later := start("CLAUDE_CODE_SESSION_ID=s-new", "FLEET_AGENT=agent-03")
	a := &Agent{Name: "agent-03", C: target.Container{}} // native: this machine
	n, err := a.KillSessionProcs(context.Background(), "s-gone")
	if err != nil || n != 1 {
		t.Fatalf("killed %d, %v", n, err)
	}
	done := make(chan error, 1)
	go func() { done <- mine.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session's job still runs")
	}
	for _, c := range []*exec.Cmd{other, later} {
		if err := c.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("pid %d was killed: %v", c.Process.Pid, err)
		}
	}
}

// A session file can outlive its process, and a process can end while the
// scripts walk /proc: neither prints "cannot open …/environ", which would
// crowd the real error out of a failed look. (The shell opens a redirect, so
// stderr must be sent away before the file is named.)
func TestTheScriptsAreQuietAboutProcessesThatAreGone(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(home+"/.claude/sessions", 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(home+"/.claude/sessions/999999999.json", []byte(`{}`), 0o600)
	for name, cmd := range map[string]*exec.Cmd{
		"snapshot": exec.Command("sh", "-c", snapshotScript),
		"reap":     exec.Command("sh", "-c", killSessionScript, "sh", "no-such-session", "agent-99"),
	} {
		cmd.Env = append(os.Environ(), "HOME="+home, "FLEET_WORK="+t.TempDir())
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		_ = cmd.Run()
		if strings.Contains(stderr.String(), "environ") {
			t.Errorf("%s: %s", name, stderr.String())
		}
	}
}
