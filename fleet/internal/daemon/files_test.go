package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/filexfer"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// fakeExec stands in for a host's docker: it takes `docker --context pc2 exec
// -i <container> cmd…` and runs cmd on this machine, with /work and /tmp
// mapped under root. It is what a container is, to the code under test.
type fakeExec struct {
	root string
	runs [][]string
}

func (fakeExec) Run(context.Context, []string, io.Reader) ([]byte, error) {
	return nil, errors.New("fakeExec streams only")
}

func (x *fakeExec) path(p string) string {
	for _, r := range filexfer.Roots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return filepath.Join(x.root, p)
		}
	}
	return p
}

func (x *fakeExec) Stream(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer) (target.Result, error) {
	x.runs = append(x.runs, argv)
	i := 0
	for argv[i] != "exec" {
		i++
	}
	i++
	for strings.HasPrefix(argv[i], "-") { // -i, -e K=V
		if argv[i] == "-e" {
			i++
		}
		i++
	}
	cmd := append([]string{}, argv[i+1:]...) // after the container's name
	for j, a := range cmd {
		cmd[j] = x.path(a)
	}
	return target.Exec{}.Stream(ctx, cmd, stdin, stdout)
}

// fileBox is a test box whose files and commands go through fakeExec.
type fileBox struct {
	*box
	a *agent.Agent
}

func (b *fileBox) PutFiles(ctx context.Context, dir string, r io.Reader) error {
	return b.a.PutFiles(ctx, dir, r)
}
func (b *fileBox) GetFiles(ctx context.Context, p string, w io.Writer) error {
	return b.a.GetFiles(ctx, p, w)
}
func (b *fileBox) Rsh(ctx context.Context, argv []string, in io.Reader, out io.Writer) (target.Result, error) {
	return b.a.Rsh(ctx, argv, in, out)
}

// filesPool is newPool with agent-03's container a fakeExec under a temp dir.
func filesPool(t *testing.T) (*Fleet, *fileBox, *fakeExec) {
	t.Helper()
	f, boxes := newPool(t)
	x := &fakeExec{root: t.TempDir()}
	for _, d := range []string{"work", "tmp"} {
		if err := os.MkdirAll(filepath.Join(x.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := &agent.Agent{Name: "agent-03", C: target.Container{Exec: []string{"docker", "--context", "pc2"}, Name: "agent-03", Runner: x}}
	fb := &fileBox{box: boxes["agent-03"], a: a}
	f.box = func(name string) Box {
		if name == "agent-03" {
			return fb
		}
		return boxes[name]
	}
	return f, fb, x
}

// tree makes a directory of files with modes, and a relative link.
func tree(t *testing.T, root string) string {
	t.Helper()
	src := filepath.Join(root, "proj")
	for p, mode := range map[string]os.FileMode{"run.sh": 0o755, "keep.txt": 0o600, "sub/a.txt": 0o644} {
		full := filepath.Join(src, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("body "+p), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(full, mode)
	}
	if err := os.Symlink("sub/a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	return src
}

func packTar(t *testing.T, paths ...string) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	if err := filexfer.Pack(&b, paths); err != nil {
		t.Fatal(err)
	}
	return &b
}

func TestADirectoryWithModesRoundTripsThroughTheContainer(t *testing.T) {
	f, _, x := filesPool(t)
	src := tree(t, t.TempDir())
	n, err := f.PutFiles(context.Background(), "agent-03", "/work/inbox", packTar(t, src))
	if err != nil || n == 0 {
		t.Fatalf("put: %d, %v", n, err)
	}
	for p, mode := range map[string]os.FileMode{"run.sh": 0o755, "keep.txt": 0o600, "sub/a.txt": 0o644} {
		fi, err := os.Stat(filepath.Join(x.root, "work/inbox/proj", p))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("in the container, %s: %v, %v", p, fi, err)
		}
	}
	var out bytes.Buffer
	if err := f.GetFiles(context.Background(), "agent-03", "/work/inbox/proj", &out); err != nil {
		t.Fatal(err)
	}
	back := t.TempDir()
	if err := filexfer.Extract(back, &out); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{"run.sh": 0o755, "keep.txt": 0o600, "sub/a.txt": 0o644} {
		fi, err := os.Stat(filepath.Join(back, "proj", p))
		b, _ := os.ReadFile(filepath.Join(back, "proj", p))
		if err != nil || fi.Mode().Perm() != mode || string(b) != "body "+p {
			t.Errorf("back, %s: %v %q, %v", p, fi, b, err)
		}
	}
	if to, _ := os.Readlink(filepath.Join(back, "proj/link")); to != "sub/a.txt" {
		t.Errorf("link = %q", to)
	}
	// The default destination is the inbox.
	if _, err := f.PutFiles(context.Background(), "agent-03", "", packTar(t, filepath.Join(src, "keep.txt"))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.root, "work/inbox/keep.txt")); err != nil {
		t.Error(err)
	}
}

func TestADestinationOutsideWorkAndTmpIsRefused(t *testing.T) {
	f, _, x := filesPool(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(x.root, "work/escape")); err != nil {
		t.Fatal(err)
	}
	src := tree(t, t.TempDir())
	for _, dir := range []string{"/etc", "/work/../etc", "/home/op/code", "/workshop", "/", "/work/escape", "/work/escape/deeper"} {
		if _, err := f.PutFiles(context.Background(), "agent-03", dir, packTar(t, src)); !errors.Is(err, ErrBadInput) {
			t.Errorf("put to %s: %v", dir, err)
		}
	}
	for _, p := range []string{"/etc/hosts", "/work/../etc/hosts", "/work/escape/x", "/home/op"} {
		if err := f.GetFiles(context.Background(), "agent-03", p, io.Discard); !errors.Is(err, ErrBadInput) {
			t.Errorf("get %s: %v", p, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something landed outside: %v", entries)
	}
	if err := f.GetFiles(context.Background(), "agent-03", "/work/missing", io.Discard); !errors.Is(err, ErrBadInput) || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing path: %v", err)
	}
}

func TestSymlinkEscapesAreRefusedBothWays(t *testing.T) {
	f, _, x := filesPool(t)
	// Up: an archive carrying a link out, or a file through a link.
	for name, mk := range map[string]func(root string) string{
		"absolute": func(root string) string { return "/etc/hosts" },
		"upward":   func(root string) string { return "../../outside" },
	} {
		src := filepath.Join(t.TempDir(), "bad")
		_ = os.MkdirAll(src, 0o755)
		_ = os.Symlink(mk(""), filepath.Join(src, "l"))
		var b bytes.Buffer
		if err := filexfer.Pack(&b, []string{src}); err == nil {
			t.Errorf("%s link: the client packed it", name)
		}
	}
	// What a hostile client could send instead of Pack's tar.
	var evil bytes.Buffer
	_ = filexfer.Sanitize(io.Discard, bytes.NewReader(nil))
	evil.Write(rawTar(t, "l", "/etc"))
	if _, err := f.PutFiles(context.Background(), "agent-03", "/work/inbox", &evil); !errors.Is(err, ErrBadInput) {
		t.Errorf("an absolute link upload: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(x.root, "work/inbox/l")); err == nil {
		t.Error("the link was made")
	}
	// Down: the container holds a link out of the tree.
	dir := filepath.Join(x.root, "work/proj")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "ok"), []byte("x"), 0o644)
	_ = os.Symlink("/etc/hosts", filepath.Join(dir, "out"))
	var got bytes.Buffer
	if err := f.GetFiles(context.Background(), "agent-03", "/work/proj", &got); !errors.Is(err, ErrBadInput) || !strings.Contains(err.Error(), "out") {
		t.Errorf("download of a tree with a link out: %v", err)
	}
}

func TestATransferOverTheCapIsCutWithAClearError(t *testing.T) {
	f, _, x := filesPool(t)
	f.cfg.Files.MaxBytes = 4096
	big := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(big, bytes.Repeat([]byte("z"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.PutFiles(context.Background(), "agent-03", "/work/inbox", packTar(t, big))
	if !errors.Is(err, filexfer.ErrTooLarge) || !strings.Contains(err.Error(), "4.0 KiB") {
		t.Fatalf("upload: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(x.root, "work/inbox/big.bin")); err == nil && fi.Size() >= 1<<20 {
		t.Error("the whole file was stored past the cap")
	}
	if err := os.WriteFile(filepath.Join(x.root, "work/big.bin"), bytes.Repeat([]byte("z"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := f.GetFiles(context.Background(), "agent-03", "/work/big.bin", &out); !errors.Is(err, filexfer.ErrTooLarge) {
		t.Fatalf("download: %v", err)
	}
	if out.Len() > 8192 {
		t.Errorf("%d bytes went out past a 4 KiB cap", out.Len())
	}
	// Under the cap is fine.
	if _, err := f.PutFiles(context.Background(), "agent-03", "/work/inbox", packTar(t, filepath.Join(t.TempDir()))); err != nil {
		t.Errorf("small: %v", err)
	}
}

func TestAttachedFilesAreInTheInboxBeforeTheBriefIsDelivered(t *testing.T) {
	f, fb, x := filesPool(t)
	inbox := filepath.Join(x.root, "work/inbox/T-att")
	var sawFiles bool
	fb.box.onRelay = func() {
		_, err := os.Stat(filepath.Join(inbox, "proj/keep.txt"))
		sawFiles = err == nil
	}
	ctx := context.Background()
	rep, err := f.Run(ctx, RunRequest{Brief: "look at the files", Agent: "agent-03", From: "ram@example.com", Task: "T-att", Attach: true}, t0)
	if err != nil || rep.Agent != "agent-03" {
		t.Fatalf("run: %+v, %v", rep, err)
	}
	if calls := fb.box.calls; len(calls) != 0 {
		t.Fatalf("the brief went before the files: %q", calls)
	}
	if _, err := f.PutFiles(ctx, "agent-03", strings.TrimSuffix(Inbox("T-att"), "/"), packTar(t, tree(t, t.TempDir()))); err != nil {
		t.Fatal(err)
	}
	if err := f.Brief(ctx, "T-att", "ram@example.com", "", "look at the files", t0); err != nil {
		t.Fatal(err)
	}
	want := "relay ram@example.com→agent-03: Task T-att from ram@example.com. Files sent with it are in /work/inbox/T-att/. look at the files"
	if calls := fb.box.calls; len(calls) != 1 || calls[0] != want {
		t.Errorf("calls = %q, want %q", calls, want)
	}
	if !sawFiles {
		t.Error("the files were not there when the brief was sent")
	}
	// The same run again (a retry) does not brief the agent twice.
	if err := f.Brief(ctx, "T-att", "ram@example.com", "", "look at the files", t0); err != nil {
		t.Fatal(err)
	}
	if calls := fb.box.calls; len(calls) != 1 {
		t.Errorf("the brief went twice: %q", calls)
	}
	// A task id that cannot name a directory is refused before a task is made.
	for _, id := range []string{"", "../x", "a/b", ".hidden"} {
		if _, err := f.Run(ctx, RunRequest{Brief: "x", Agent: "agent-01", Task: id, Attach: true}, t0); !errors.Is(err, ErrBadInput) {
			t.Errorf("task %q: %v", id, err)
		}
	}
}

func TestRshRunsTheCommandInTheContainerAndReturnsItsExit(t *testing.T) {
	f, _, x := filesPool(t)
	var out bytes.Buffer
	res, err := f.Rsh(context.Background(), "agent-03", []string{"sh", "-c", "cat; echo bad >&2; exit 7"}, strings.NewReader("hello"), &out)
	if err != nil || res.Exit != 7 || res.Stderr != "bad" || out.String() != "hello" {
		t.Fatalf("%+v %q, %v", res, out.String(), err)
	}
	run := x.runs[len(x.runs)-1]
	if strings.Join(run[:6], " ") != "docker --context pc2 exec -i agent-03" && strings.Join(run[:5], " ") != "docker --context pc2 exec -i" {
		t.Errorf("went through %v", run)
	}
	if _, err := f.Rsh(context.Background(), "agent-03", nil, nil, io.Discard); !errors.Is(err, ErrBadInput) {
		t.Errorf("no command: %v", err)
	}
}

// rawTar is a one-symlink archive, as a client that does not use Pack could send.
func rawTar(t *testing.T, name, link string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: link}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	return b.Bytes()
}

func TestAWorkerCopiesIntoItsAgentsContainer(t *testing.T) {
	f, fb, x := filesPool(t)
	a := f.cfg.Agents["agent-03"]
	a.Slots = 2
	f.cfg.Agents["agent-03"] = a
	prev := f.box
	f.box = func(name string) Box {
		if name == "agent-03:2" {
			return fb // the same container: its /work is shared by every slot
		}
		return prev(name)
	}
	src := tree(t, t.TempDir())
	if _, err := f.PutFiles(context.Background(), "agent-03:2", "/work/inbox/T-slot", packTar(t, src)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.root, "work/inbox/T-slot/proj/run.sh")); err != nil {
		t.Error(err)
	}
	if _, err := f.PutFiles(context.Background(), "agent-03:3", "/work/inbox", packTar(t, src)); !errors.Is(err, ErrBadInput) {
		t.Errorf("a slot that does not exist: %v", err)
	}
}
