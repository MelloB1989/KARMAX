package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
)

type fakeSource struct {
	tr       *agent.Transcript
	hash     string
	wts      []agent.Worktree
	pushed   *agent.Transcript
	pushedID string
}

func (f *fakeSource) PullTranscript(_ context.Context, sid string) (*agent.Transcript, error) {
	if f.tr == nil {
		return nil, agent.ErrNoTranscript
	}
	return f.tr, nil
}
func (f *fakeSource) HashTranscript(context.Context, string) (string, error) { return f.hash, nil }
func (f *fakeSource) Worktrees(context.Context) ([]agent.Worktree, error)    { return f.wts, nil }
func (f *fakeSource) PushTranscript(_ context.Context, sid string, tr *agent.Transcript) error {
	f.pushed, f.pushedID = tr, sid
	return nil
}

const body = `{"type":"user","sessionId":"0123456789ab","cwd":"/work","timestamp":"2026-10-08T10:00:00Z","turnOrigin":"peer","message":{"role":"user","content":"fix it"}}
{"type":"assistant","sessionId":"0123456789ab","timestamp":"2026-10-08T10:10:00Z","message":{"id":"m1","model":"claude-sonnet","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":5,"output_tokens":9}}}
`

func sum(b string) string { h := sha256.Sum256([]byte(b)); return hex.EncodeToString(h[:]) }

func source() *fakeSource {
	return &fakeSource{
		tr:   &agent.Transcript{Project: "-work", Main: []byte(body), Sub: map[string][]byte{"subagents/a.jsonl": []byte("{}\n")}},
		hash: sum(body),
		wts: []agent.Worktree{
			{Path: "/work/wt/fix", Branch: "fleet/agent-03/fix", Upstream: "origin/fleet/agent-03/fix"},
			{Path: "/work/wt/wip", Branch: "fleet/agent-03/wip", Dirty: 2},
		},
	}
}

var at = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func TestArchiveWritesAVerifiedRecord(t *testing.T) {
	root := t.TempDir()
	m, err := Archive(context.Background(), source(), root, Info{
		Agent: "agent-03", Host: "kali", Account: "acct-3", SessionID: "0123456789ab",
		Task: "T1", Reason: "task_done", At: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "agent-03/2026-10-08-01234567" {
		t.Errorf("id = %q", m.ID)
	}
	if m.SHA256 != sum(body) || m.Turns != 1 || m.PeerMessages != 1 || m.Usage.Output != 9 ||
		m.LastAssistant != "done" || m.Project != "-work" || m.Task != "T1" {
		t.Errorf("manifest = %+v", m)
	}
	if len(m.Worktrees) != 2 || len(m.KeptWorktrees()) != 1 || m.KeptWorktrees()[0].Path != "/work/wt/wip" {
		t.Errorf("worktrees = %+v", m.Worktrees)
	}
	dir := filepath.Join(root, "agent-03", "2026-10-08-01234567")
	for _, f := range []string{"manifest.json", "transcript.jsonl.zst", "worktrees.json", "subagents/a.jsonl.zst"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	got, err := Load(root, m.ID)
	if err != nil || got.SessionID != "0123456789ab" {
		t.Fatalf("load: %+v %v", got, err)
	}
	tr, err := ReadTranscript(root, m.ID)
	if err != nil || string(tr.Main) != body || string(tr.Sub["subagents/a.jsonl"]) != "{}\n" {
		t.Fatalf("read back %v", err)
	}
}

// What was received must be what the container has: a mismatch leaves no
// archive behind, so nothing downstream ever stops the session.
func TestArchiveRefusesAHashMismatch(t *testing.T) {
	root := t.TempDir()
	src := source()
	src.hash = sum("something else")
	_, err := Archive(context.Background(), src, root, Info{Agent: "agent-03", SessionID: "0123456789ab", At: at})
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v, want ErrVerify", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "agent-03"))
	for _, e := range entries {
		t.Errorf("left behind: %s", e.Name())
	}
}

func TestArchivingTheSameSessionTwiceKeepsBoth(t *testing.T) {
	root := t.TempDir()
	m1, err := Archive(context.Background(), source(), root, Info{Agent: "agent-03", SessionID: "0123456789ab", At: at})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Archive(context.Background(), source(), root, Info{Agent: "agent-03", SessionID: "0123456789ab", At: at})
	if err != nil {
		t.Fatal(err)
	}
	if m1.ID == m2.ID {
		t.Fatal("a second archive overwrote the first")
	}
}

func TestRestorePushesTheArchivedTranscript(t *testing.T) {
	root := t.TempDir()
	m, err := Archive(context.Background(), source(), root, Info{Agent: "agent-03", SessionID: "0123456789ab", At: at})
	if err != nil {
		t.Fatal(err)
	}
	dst := &fakeSource{}
	got, err := Restore(context.Background(), dst, root, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "0123456789ab" || dst.pushedID != "0123456789ab" || string(dst.pushed.Main) != body || dst.pushed.Project != "-work" {
		t.Fatalf("pushed %q as %s", dst.pushed, dst.pushedID)
	}
}

func TestListAndPrune(t *testing.T) {
	root := t.TempDir()
	old := Info{Agent: "agent-03", SessionID: "aaaaaaaaaaaa", At: at.Add(-200 * 24 * time.Hour)}
	recent := Info{Agent: "agent-04", SessionID: "bbbbbbbbbbbb", At: at.Add(-time.Hour)}
	for _, in := range []Info{old, recent} {
		if _, err := Archive(context.Background(), source(), root, in); err != nil {
			t.Fatal(err)
		}
	}
	all, err := List(root, time.Time{})
	if err != nil || len(all) != 2 {
		t.Fatalf("list = %d, %v", len(all), err)
	}
	if all[0].Agent != "agent-04" {
		t.Errorf("newest first: %s", all[0].ID)
	}
	since, _ := List(root, at.Add(-24*time.Hour))
	if len(since) != 1 {
		t.Errorf("since = %d", len(since))
	}
	n, err := Prune(root, at.Add(-180*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if left, _ := List(root, time.Time{}); len(left) != 1 || left[0].Agent != "agent-04" {
		t.Fatalf("left %+v", left)
	}
}
