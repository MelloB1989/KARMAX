package desk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// testDesk is a desk on a tmux server of its own, on a socket of its own
// under a TMUX_TMPDIR of its own: nothing reaches the operator's tmux. Its
// claude is a fake that records how it was started and writes the session
// file Claude Code writes; its home is a temp dir; its boot id is the
// test's to change.
type testDesk struct {
	*Manager
	t      *testing.T
	home   string
	claude string // the fake's log: one line of argv per start
	boot   string
}

func newTestDesk(t *testing.T) *testDesk {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	// A unix socket's path is short: TMUX_TMPDIR is kept short too.
	tmp, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) > 60 {
		os.RemoveAll(tmp)
		if tmp, err = os.MkdirTemp("/tmp", "dk"); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMUX_TMPDIR", tmp)
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "code", "api"), 0o755)
	d := &testDesk{t: t, home: home, claude: filepath.Join(t.TempDir(), "claude.log"), boot: "boot-1"}
	fake := filepath.Join(t.TempDir(), "claude")
	cdir := filepath.Join(home, ".claude")
	script := `#!/bin/sh
echo "$*" >> '` + d.claude + `'
id=
while [ $# -gt 0 ]; do
	case "$1" in
	--session-id|--resume) id=$2; shift ;;
	esac
	shift
done
mkdir -p '` + cdir + `/sessions'
printf '{"pid":%d,"sessionId":"%s","cwd":"%s","status":"idle","kind":"interactive"}\n' $$ "$id" "$PWD" > '` + cdir + `/sessions/'$$.json
echo "fake claude $id in $PWD"
exec sleep 600
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m := New("dt-"+randHex(4), filepath.Join(t.TempDir(), "state"))
	m.Conf = "/dev/null"
	m.Shell = "/bin/sh"
	m.Claude = fake
	m.ClaudeDir = cdir
	m.Home = home
	m.SystemdRun = ""
	m.Host = "kali"
	m.BootID = func() string { return d.boot }
	m.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMUX_TMPDIR=" + tmp, "SHELL=/bin/sh", "LANG=C.UTF-8"}
	m.Log = t.Logf
	d.Manager = m
	t.Cleanup(func() {
		d.tmux("kill-server")
		os.RemoveAll(tmp)
	})
	return d
}

// tmux runs a tmux command on the desk's server, as the test.
func (d *testDesk) tmux(args ...string) string {
	out, _ := exec.Command("tmux", append([]string{"-L", d.Socket}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// restart is fleetd starting afresh: a new desk with the same settings,
// knowing only what its state file says.
func (d *testDesk) restart() {
	o := d.Manager
	m := New(o.Socket, filepath.Dir(o.State))
	m.Conf, m.Shell, m.Claude, m.ClaudeDir, m.Home = o.Conf, o.Shell, o.Claude, o.ClaudeDir, o.Home
	m.SystemdRun, m.Host, m.BootID, m.Env, m.Log = o.SystemdRun, o.Host, o.BootID, o.Env, o.Log
	d.Manager = m
}

func (d *testDesk) create(c fleetapi.DeskCreate) fleetapi.DeskSession {
	d.t.Helper()
	s, err := d.Create(context.Background(), c)
	if err != nil {
		d.t.Fatalf("create %+v: %v", c, err)
	}
	return s
}

func (d *testDesk) list(lines int) []fleetapi.DeskSession {
	d.t.Helper()
	l, err := d.List(context.Background(), lines)
	if err != nil {
		d.t.Fatal(err)
	}
	if l.Host != "kali" {
		d.t.Fatalf("host = %q", l.Host)
	}
	return l.Sessions
}

// starts is each start of the fake claude, its argv.
func (d *testDesk) starts() []string {
	b, _ := os.ReadFile(d.claude)
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s in 10s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (d *testDesk) saved() state {
	d.t.Helper()
	b, err := os.ReadFile(d.State)
	if err != nil {
		d.t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		d.t.Fatal(err)
	}
	return st
}

func names(ss []fleetapi.DeskSession) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Name+":"+strconv.Itoa(s.Order))
	}
	return out
}

func TestCreateStartsClaudeCodeWithASessionOfItsOwn(t *testing.T) {
	d := newTestDesk(t)
	s := d.create(fleetapi.DeskCreate{Dir: "~/code"})
	if s.Name != "code" || s.Dir != filepath.Join(d.home, "code") || s.Command != "claude" || s.Order != 0 || !s.Running || s.Claude == "" {
		t.Fatalf("created %+v", s)
	}
	eventually(t, "claude's start", func() bool { return len(d.starts()) == 1 })
	if got := d.starts()[0]; got != "--session-id "+s.Claude {
		t.Fatalf("claude started with %q, want its own session id", got)
	}
	var got fleetapi.DeskSession
	eventually(t, "its screen", func() bool {
		got = d.list(10)[0]
		return strings.Contains(got.Text, "fake claude "+s.Claude)
	})
	if !got.Running || got.Cols != 200 || got.At.IsZero() || got.Clients != 0 || got.Claude != s.Claude {
		t.Fatalf("listed %+v", got)
	}
	if !strings.Contains(got.Text, "in "+filepath.Join(d.home, "code")) {
		t.Fatalf("not started in its dir: %q", got.Text)
	}
	// Without lines, no screen.
	if s := d.list(0)[0]; s.Text != "" {
		t.Fatalf("lines=0 gave text %q", s.Text)
	}
	fi, err := os.Stat(d.State)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v, want 0600", fi, err)
	}
	if st := d.saved(); st.Boot != "boot-1" || len(st.Sessions) != 1 || st.Sessions[0].Claude != s.Claude || st.Sessions[0].Dir != s.Dir {
		t.Fatalf("saved %+v", st)
	}
}

func TestCreateNamesAndChecksTheDirectory(t *testing.T) {
	d := newTestDesk(t)
	os.WriteFile(filepath.Join(d.home, "a-file"), nil, 0o644)
	os.MkdirAll(filepath.Join(d.home, "web.app"), 0o755)
	for _, dir := range []string{"~/nowhere", "~/a-file", "relative/../nowhere"} {
		if _, err := d.Create(context.Background(), fleetapi.DeskCreate{Dir: dir, Command: "shell"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("dir %q: %v, want ErrInvalid", dir, err)
		}
	}
	got := []string{
		d.create(fleetapi.DeskCreate{Dir: "~/code", Command: "shell"}).Name,
		d.create(fleetapi.DeskCreate{Dir: d.home + "/code/", Command: "shell"}).Name,
		d.create(fleetapi.DeskCreate{Dir: "~/code", Command: "shell"}).Name,
		d.create(fleetapi.DeskCreate{Dir: "~/web.app", Command: "shell"}).Name, // tmux makes . into _: so do we
		d.create(fleetapi.DeskCreate{Dir: "~", Name: "my  api!", Command: "shell"}).Name,
		d.create(fleetapi.DeskCreate{Dir: "", Name: "code", Command: "shell"}).Name, // "" is home
		d.create(fleetapi.DeskCreate{Dir: "code/api", Name: "!!", Command: "shell"}).Name,
	}
	want := []string{"code", "code-2", "code-3", "web_app", "my-api", "code-4", "desk"}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	live := strings.Fields(d.tmux("list-sessions", "-F", "#{session_name}"))
	slices.Sort(live)
	slices.Sort(want)
	if !slices.Equal(live, want) {
		t.Fatalf("tmux has %v, want %v", live, want)
	}
	if ss := d.list(0); ss[6].Dir != filepath.Join(d.home, "code", "api") || ss[5].Dir != d.home {
		t.Fatalf("dirs %q %q", ss[6].Dir, ss[5].Dir)
	}
	if len(d.starts()) != 0 {
		t.Fatalf("a shell started claude: %v", d.starts())
	}
}

func TestTheCommandRunsInTheLoginShellAndTheSessionOutlivesIt(t *testing.T) {
	d := newTestDesk(t)
	s := d.create(fleetapi.DeskCreate{Dir: "~/code", Command: "claude --model opus"})
	o := d.create(fleetapi.DeskCreate{Dir: "~/code", Name: "echo", Command: "echo made-by-$((6*7))"})
	eventually(t, "claude's start", func() bool { return len(d.starts()) == 1 })
	if got := d.starts()[0]; got != "--session-id "+s.Claude+" --model opus" {
		t.Fatalf("claude started with %q", got)
	}
	if o.Claude != "" || o.Command != "echo made-by-$((6*7))" {
		t.Fatalf("a command not claude: %+v", o)
	}
	eventually(t, "the command's output", func() bool {
		ss := d.list(20)
		return len(ss) == 2 && strings.Contains(ss[1].Text, "made-by-42")
	})
	time.Sleep(200 * time.Millisecond) // the command is done: the shell after it holds the session
	if ss := d.list(0); len(ss) != 2 || !ss[1].Running {
		t.Fatalf("the session ended with its command: %+v", ss)
	}
}

func TestEndForgetsASessionAndTheRestCloseUp(t *testing.T) {
	d := newTestDesk(t)
	for _, n := range []string{"a", "b", "c"} {
		d.create(fleetapi.DeskCreate{Name: n, Dir: "~", Command: "shell"})
	}
	if err := d.End(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if got := names(d.list(0)); !slices.Equal(got, []string{"a:0", "c:1"}) {
		t.Fatalf("after end: %v", got)
	}
	if out := d.tmux("has-session", "-t", "=b"); !strings.Contains(out, "can't find session") {
		t.Fatalf("tmux session b is still there: %q", out)
	}
	if st := d.saved(); len(st.Sessions) != 2 || st.Sessions[1].Name != "c" || st.Sessions[1].Order != 1 {
		t.Fatalf("saved %+v", st)
	}
	if err := d.End(context.Background(), "b"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("ending it again: %v, want ErrNoSession", err)
	}
}

// Claude Code's session changes under the desk (/clear, /resume): the desk
// follows it from the session file of the claude in the pane.
func TestTheDeskFollowsClaudeCodesSession(t *testing.T) {
	d := newTestDesk(t)
	s := d.create(fleetapi.DeskCreate{Dir: "~/code"})
	var file string
	eventually(t, "claude's session file", func() bool {
		m, _ := filepath.Glob(filepath.Join(d.ClaudeDir, "sessions", "*.json"))
		if len(m) == 1 {
			file = m[0]
		}
		return file != ""
	})
	if got := d.list(0)[0]; got.Claude != s.Claude || got.Busy {
		t.Fatalf("listed %+v", got)
	}
	pid := strings.TrimSuffix(filepath.Base(file), ".json")
	cleared := "0b3c9f5e-0d6a-4c39-9d43-5e0f7a1c2b3d"
	os.WriteFile(file, []byte(`{"pid":`+pid+`,"sessionId":"`+cleared+`","status":"busy"}`), 0o600)
	if got := d.list(0)[0]; got.Claude != cleared || !got.Busy {
		t.Fatalf("after a /clear: %+v", got)
	}
	if st := d.saved(); st.Sessions[0].Claude != cleared {
		t.Fatalf("saved %+v, want the new session", st.Sessions[0])
	}
	// A stale file of a process that is not in the pane says nothing.
	os.WriteFile(filepath.Join(d.ClaudeDir, "sessions", "1.json"), []byte(`{"pid":1,"sessionId":"someone-else"}`), 0o600)
	if got := d.list(0)[0]; got.Claude != cleared {
		t.Fatalf("took another process's session: %+v", got)
	}
}

// The machine restarts: every session comes back, in its order, in its
// directory, its Claude Code session resumed when it has a transcript.
func TestARebootBringsEverySessionBackInOrder(t *testing.T) {
	d := newTestDesk(t)
	a := d.create(fleetapi.DeskCreate{Name: "a", Dir: "~/code"})
	b := d.create(fleetapi.DeskCreate{Name: "b", Dir: "~/code/api", Command: "claude --model opus"})
	d.create(fleetapi.DeskCreate{Name: "c", Dir: "~", Command: "shell"})
	d.create(fleetapi.DeskCreate{Name: "d", Dir: "~", Command: "echo back-again"})
	eventually(t, "both claudes", func() bool { return len(d.starts()) == 2 })
	// a's session has a transcript (it was used); b's was never written to.
	os.MkdirAll(filepath.Join(d.ClaudeDir, "projects", "-home-op-code"), 0o755)
	os.WriteFile(filepath.Join(d.ClaudeDir, "projects", "-home-op-code", a.Claude+".jsonl"), []byte("{}\n"), 0o600)
	if st := d.saved(); st.Boot != "boot-1" {
		t.Fatalf("saved boot %q", st.Boot)
	}

	// The reboot: tmux is gone, the boot id is new, fleetd starts afresh.
	d.tmux("kill-server")
	os.Truncate(d.claude, 0)
	d.boot = "boot-2"
	d.restart()
	before := time.Now()
	if err := d.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	ss := d.list(0)
	if got := names(ss); !slices.Equal(got, []string{"a:0", "b:1", "c:2", "d:3"}) {
		t.Fatalf("after the reboot: %v", got)
	}
	for _, s := range ss {
		if !s.Running || s.Restored.Before(before) || s.Error != "" {
			t.Errorf("%s: %+v, want running and restored", s.Name, s)
		}
	}
	// tmux numbers sessions as they are made: they were made in order.
	if got := d.tmux("list-sessions", "-F", "#{session_name}", "-f", "1"); strings.Join(strings.Fields(got), ",") != "a,b,c,d" {
		t.Fatalf("tmux's sessions %q", got)
	}
	ids := strings.Fields(d.tmux("list-sessions", "-F", "#{session_id}:#{session_name}"))
	slices.SortFunc(ids, func(x, y string) int {
		nx, _ := strconv.Atoi(strings.TrimPrefix(strings.Split(x, ":")[0], "$"))
		ny, _ := strconv.Atoi(strings.TrimPrefix(strings.Split(y, ":")[0], "$"))
		return nx - ny
	})
	var made []string
	for _, id := range ids {
		made = append(made, strings.Split(id, ":")[1])
	}
	if !slices.Equal(made, []string{"a", "b", "c", "d"}) {
		t.Fatalf("made in the order %v", made)
	}
	eventually(t, "both claudes again", func() bool { return len(d.starts()) == 2 })
	starts := d.starts()
	slices.Sort(starts)
	want := []string{"--resume " + a.Claude, "--session-id " + b.Claude + " --model opus"}
	if !slices.Equal(starts, want) {
		t.Fatalf("claude restarted with %q, want %q", starts, want)
	}
	eventually(t, "d's command again", func() bool { return strings.Contains(d.list(10)[3].Text, "back-again") })
	if st := d.saved(); st.Boot != "boot-2" || st.Sessions[0].Restored.IsZero() {
		t.Fatalf("saved %+v", st)
	}
	// A second sync on the same boot brings back nothing more.
	if err := d.Sync(context.Background()); err != nil || len(d.starts()) != 2 {
		t.Fatalf("a second sync: %v, starts %v", err, d.starts())
	}
}

// On the same boot a session that is gone was closed by the operator: it
// is forgotten, never brought back.
func TestAClosedSessionIsForgotten(t *testing.T) {
	d := newTestDesk(t)
	for _, n := range []string{"a", "b", "c"} {
		d.create(fleetapi.DeskCreate{Name: n, Dir: "~", Command: "shell"})
	}
	d.tmux("kill-session", "-t", "=b")
	if got := names(d.list(0)); !slices.Equal(got, []string{"a:0", "c:1"}) {
		t.Fatalf("after b closed: %v", got)
	}
	if st := d.saved(); len(st.Sessions) != 2 {
		t.Fatalf("saved %+v", st)
	}
	// The whole server gone, on the same boot: everything was closed.
	d.tmux("kill-server")
	// While it is still going, tmux answers "server exited unexpectedly"
	// (Sync errs and leaves the desk alone): ask until it is gone.
	var err error
	for range 100 {
		if err = d.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "exited unexpectedly") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if ss := d.list(0); len(ss) != 0 {
		t.Fatalf("after the server went: %+v", ss)
	}
}

// A session whose directory went away while the machine was down cannot
// come back: it says so, and stays until ended.
func TestASessionThatCannotComeBackSaysWhy(t *testing.T) {
	d := newTestDesk(t)
	gone := filepath.Join(d.home, "gone")
	os.MkdirAll(gone, 0o755)
	d.create(fleetapi.DeskCreate{Name: "a", Dir: "~", Command: "shell"})
	d.create(fleetapi.DeskCreate{Name: "b", Dir: gone, Command: "shell"})
	d.tmux("kill-server")
	os.RemoveAll(gone)
	d.boot = "boot-2"
	if err := d.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	ss := d.list(0)
	if len(ss) != 2 || !ss[0].Running || ss[1].Running || !strings.Contains(ss[1].Error, "not restored") {
		t.Fatalf("after the reboot: %+v", ss)
	}
	d.Sync(context.Background())
	if ss := d.list(0); len(ss) != 2 {
		t.Fatalf("the unrestored one was forgotten: %+v", ss)
	}
	if err := d.End(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if got := names(d.list(0)); !slices.Equal(got, []string{"a:0"}) {
		t.Fatalf("after end: %v", got)
	}
}

// The tmux server is started in a scope of its own when systemd-run is
// there, so that fleetd's restart (which ends everything in its cgroup)
// does not end it; once it runs, sessions are made in it plainly.
func TestTheServerStartsInAScopeOfItsOwn(t *testing.T) {
	d := newTestDesk(t)
	log := filepath.Join(t.TempDir(), "systemd-run.log")
	fake := filepath.Join(t.TempDir(), "systemd-run")
	os.WriteFile(fake, []byte(`#!/bin/sh
echo "$*" >> '`+log+`'
while [ $# -gt 0 ]; do
	case "$1" in
	--*) shift ;;
	*) exec "$@" ;;
	esac
done
`), 0o755)
	d.SystemdRun = fake
	d.create(fleetapi.DeskCreate{Name: "a", Dir: "~", Command: "shell"})
	d.create(fleetapi.DeskCreate{Name: "b", Dir: "~", Command: "shell"})
	b, _ := os.ReadFile(log)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 1 {
		t.Fatalf("systemd-run calls %q, want one: the server's start", calls)
	}
	for _, want := range []string{"--user --scope --collect", "--unit=" + d.Socket + "-", "tmux -L " + d.Socket, "new-session -d -s a"} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("systemd-run %q lacks %q", calls[0], want)
		}
	}
	if got := names(d.list(0)); !slices.Equal(got, []string{"a:0", "b:1"}) {
		t.Fatalf("sessions %v", got)
	}

	// A systemd-run that cannot start a scope: the server starts plainly.
	d.tmux("kill-server")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'Failed to connect to bus' >&2\nexit 1\n"), 0o755)
	d.create(fleetapi.DeskCreate{Name: "c", Dir: "~", Command: "shell"})
	if got := d.tmux("has-session", "-t", "=c"); got != "" {
		t.Fatalf("no session c: %q", got)
	}
}

func TestTerminalIsTheSessionOnThisMachine(t *testing.T) {
	d := newTestDesk(t)
	d.create(fleetapi.DeskCreate{Name: "api", Dir: "~/code/api", Command: "shell"})
	tg, ok := d.Terminal("api")
	if !ok || tg.Worker != "desk:api" || tg.Socket != d.Socket || tg.Pane != "api" || tg.Dir != filepath.Join(d.home, "code", "api") || tg.Shell != "/bin/sh" || tg.Host != "kali" {
		t.Fatalf("terminal = %+v %v", tg, ok)
	}
	if len(tg.Container.Exec) != 0 || tg.Container.Name != "" {
		t.Fatalf("a desk terminal has a container: %+v", tg.Container)
	}
	if _, ok := d.Terminal("nope"); ok {
		t.Fatal("a terminal for no session")
	}
}

func TestDirsCompleteWhatIsTyped(t *testing.T) {
	d := newTestDesk(t)
	for _, p := range []string{"code/web", "code/Api2", "code/.git", "code/apple", "Code-old", ".config/x", ".cache"} {
		os.MkdirAll(filepath.Join(d.home, p), 0o755)
	}
	os.WriteFile(filepath.Join(d.home, "code", "app.txt"), nil, 0o644)
	h := d.home
	d.create(fleetapi.DeskCreate{Dir: "~/code/web", Command: "shell"})
	d.create(fleetapi.DeskCreate{Dir: "~/code/api", Command: "shell"})
	for _, c := range []struct {
		prefix string
		want   []string
	}{
		// Nothing typed: home, the code, and the latest sessions' dirs.
		{"", []string{h, h + "/code", h + "/code/api", h + "/code/web"}},
		// A directory: its subdirectories, no dot-dirs, no files.
		{"~/code", []string{h + "/code/api", h + "/code/Api2", h + "/code/apple", h + "/code/web"}},
		{"~/code/", []string{h + "/code/api", h + "/code/Api2", h + "/code/apple", h + "/code/web"}},
		// A start: its siblings that begin so, whatever the case.
		{"~/code/ap", []string{h + "/code/api", h + "/code/Api2", h + "/code/apple"}},
		{h + "/co", []string{h + "/code", h + "/Code-old"}},
		{"~/code/.", []string{h + "/code/.git"}},
		{"~/.c", []string{h + "/.cache", h + "/.config"}},
		{"~/nowhere/x", nil},
	} {
		if got := d.Dirs(c.prefix); !slices.Equal(got, c.want) {
			t.Errorf("Dirs(%q) = %q, want %q", c.prefix, got, c.want)
		}
	}
	for i := range 40 {
		os.MkdirAll(filepath.Join(d.home, "many", "d"+strconv.Itoa(i)), 0o755)
	}
	if got := d.Dirs("~/many"); len(got) != 30 {
		t.Fatalf("%d dirs, want at most 30", len(got))
	}
}

func TestTheSessionsEnvironmentIsNotFleetds(t *testing.T) {
	got := sessionEnv([]string{"HOME=/home/op", "PATH=/usr/bin", "FLEET_CONFIG=/x", "FLEET_TOKEN=s", "INVOCATION_ID=1", "JOURNAL_STREAM=8:9",
		"TMUX=/tmp/x,1,0", "TMUX_PANE=%1", "GITHUB_TOKEN=t", "AWS_SECRET_ACCESS_KEY=k", "SSH_AUTH_SOCK=/run/a", "XDG_RUNTIME_DIR=/run/user/1000", "MEMORY_PRESSURE_WATCH=/x"})
	want := []string{"HOME=/home/op", "PATH=/usr/bin", "SSH_AUTH_SOCK=/run/a", "XDG_RUNTIME_DIR=/run/user/1000"}
	if !slices.Equal(got, want) {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

// A restore resumes the session it ran last, whatever chose the session at
// its launch; its other arguments stay.
func TestTheResumeReplacesTheLaunchesChoiceOfSession(t *testing.T) {
	const id = "5e0f7a1c-0d6a-4c39-9d43-0b3c9f5e2b3d"
	for _, c := range []struct{ cmd, want string }{
		{"claude", "'claude' --resume " + id},
		{"claude --model opus", "'claude' --resume " + id + " --model opus"},
		{"claude --resume old --model opus", "'claude' --resume " + id + " --model opus"},
		{"claude -r old -c --fork-session --session-id=x --model opus", "'claude' --resume " + id + " --model opus"},
		{"claude -r --model opus", "'claude' --resume " + id + " --model opus"},
	} {
		if got := claudeLine("claude", c.cmd, id, true); got != c.want {
			t.Errorf("%q: %q, want %q", c.cmd, got, c.want)
		}
	}
	if got := claudeLine("claude", "claude --continue", "", false); got != "'claude' --continue" {
		t.Errorf("a launch that chooses its own session: %q", got)
	}
}

// A session file whose process start time is not the process's is a
// stale one, of a process that had the pid before.
func TestAStaleSessionFileIsNotTheProcesss(t *testing.T) {
	cdir := t.TempDir()
	os.MkdirAll(filepath.Join(cdir, "sessions"), 0o755)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	pid := cmd.Process.Pid
	start, ok := procStart(pid)
	if !ok {
		t.Fatal("no start time")
	}
	file := filepath.Join(cdir, "sessions", strconv.Itoa(pid)+".json")
	const sid = "9d430b3c-9f5e-4c39-0d6a-5e0f7a1c2b3d"
	m := &Manager{ClaudeDir: cdir}
	os.WriteFile(file, []byte(`{"pid":`+strconv.Itoa(pid)+`,"sessionId":"`+sid+`","procStart":"1"}`), 0o600)
	if s, ok := m.claudeUnder(os.Getpid()); ok {
		t.Fatalf("took a stale file: %+v", s)
	}
	os.WriteFile(file, []byte(`{"pid":`+strconv.Itoa(pid)+`,"sessionId":"`+sid+`","procStart":"`+start+`","status":"busy"}`), 0o600)
	if s, ok := m.claudeUnder(os.Getpid()); !ok || s.ID != sid || s.Status != "busy" {
		t.Fatalf("claude under the test: %+v %v", s, ok)
	}
}

// Only tmux saying there is no server means none: a server too busy to
// answer must not have every session forgotten.
func TestOnlyNoServerIsAServerGone(t *testing.T) {
	for msg, want := range map[string]bool{
		"no server running on /tmp/tmux-1000/fleet-desk":                                   true,
		"error connecting to /tmp/tmux-1000/fleet-desk (No such file or directory)":        true,
		"error connecting to /tmp/tmux-1000/fleet-desk (Connection refused)":               true,
		"can't find session: =api":                                                         true,
		"error connecting to /tmp/tmux-1000/fleet-desk (Resource temporarily unavailable)": false,
		"error connecting to /tmp/tmux-1000/fleet-desk (Permission denied)":                false,
		"signal: killed": false,
	} {
		if got := gone(errors.New(msg)); got != want {
			t.Errorf("gone(%q) = %v, want %v", msg, got, want)
		}
	}
}

// A session id from a session file goes into a shell line: only an id.
func TestASessionFileWithAStrangeIDIsIgnored(t *testing.T) {
	cdir := t.TempDir()
	os.MkdirAll(filepath.Join(cdir, "sessions"), 0o755)
	pid := os.Getpid()
	os.WriteFile(filepath.Join(cdir, "sessions", strconv.Itoa(pid)+".json"), []byte(`{"sessionId":"x; touch /tmp/pwned"}`), 0o600)
	if s, ok := (&Manager{ClaudeDir: cdir}).claudeUnder(pid); ok {
		t.Fatalf("took %+v", s)
	}
	if got := claudeLine("claude", "claude", "x; true", true); got != "'claude'" {
		t.Fatalf("a strange id reached the line: %q", got)
	}
}

// fleetd stopping in the middle of a restore leaves the rest to its next
// start: none is marked as not restored, and the boot stays the old one.
func TestAnInterruptedRestoreIsFinishedLater(t *testing.T) {
	d := newTestDesk(t)
	for _, n := range []string{"a", "b"} {
		d.create(fleetapi.DeskCreate{Name: n, Dir: "~", Command: "shell"})
	}
	d.tmux("kill-server")
	d.boot = "boot-2"
	// A tmux that answers the list, and hangs making a session until
	// fleetd stops.
	ctx, cancel := context.WithCancel(context.Background())
	real := d.Tmux
	path, err := exec.LookPath(real)
	if err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(t.TempDir(), "tmux")
	os.WriteFile(stop, []byte("#!/bin/sh\ncase \"$*\" in *new-session*) sleep 5 ;; esac\nexec '"+path+"' \"$@\"\n"), 0o755)
	d.Tmux = stop
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	if err := d.Sync(ctx); err == nil {
		t.Fatal("an interrupted restore said nothing")
	}
	if st := d.saved(); st.Boot != "boot-1" || len(st.Sessions) != 2 || st.Sessions[0].Error != "" || st.Sessions[1].Error != "" {
		t.Fatalf("saved %+v", st)
	}
	d.Tmux = real
	if err := d.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ss := d.list(0); len(ss) != 2 || !ss[0].Running || !ss[1].Running {
		t.Fatalf("after the next start: %+v", ss)
	}
}
