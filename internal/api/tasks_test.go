package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/chatlog"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// newTaskTestServer builds a Server with a real (sqlite) store and no auth —
// this endpoint's own logic is what these tests exercise, not the bearer gate
// every other handler already covers.
func newTaskTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "tasks.db"), zap.NewNop())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New("127.0.0.1:0", 0, "", "", nil, db, nil, nil, &config.KarmaxConfig{}, zap.NewNop())
}

func taskTranscript(srv *Server, taskID string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskID+"/transcript", nil)
	r.SetPathValue("taskId", taskID)
	w := httptest.NewRecorder()
	srv.handleTaskTranscript(w, r)
	return w
}

// A task id reaches this package straight off an HTTP path and is joined into
// both a session key and a directory — the same escape a conversation id is
// guarded against in chatlog.
func TestTaskTranscriptRejectsBadIDs(t *testing.T) {
	srv := newTaskTestServer(t)
	for _, id := range []string{
		"../../etc/passwd",
		"..",
		"has/slash",
		`back\slash`,
		"",
		strings.Repeat("a", 129), // one over the 128-char ceiling
	} {
		w := taskTranscript(srv, id)
		if w.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400", id, w.Code)
		}
	}
}

// A task whose session key has never been minted — nobody has run a turn for
// it yet — is not found, not a server error.
func TestTaskTranscriptNotFoundWhenNothingMinted(t *testing.T) {
	srv := newTaskTestServer(t)
	w := taskTranscript(srv, "task-never-run")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no transcript for that task") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// Once a turn has actually run, the mapping resolves to a real transcript —
// read back from the exact working directory a LYZN task always uses
// (lyzn-tasks/<task id>, see pruneStaleLyznSessions), in the same shape a
// chat conversation's history is.
func TestTaskTranscriptReadsTheMintedSession(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KARMAX_WORKDIR", work)
	hostpaths.ResetWorkDirForTest()
	t.Cleanup(hostpaths.ResetWorkDirForTest)

	srv := newTaskTestServer(t)
	taskID := "task-1"
	cliSessionID := "11111111-1111-1111-1111-111111111111"
	if err := srv.store.SaveSessionKey(lyznSessionPrefix+taskID, cliSessionID, "claude_code"); err != nil {
		t.Fatalf("SaveSessionKey: %v", err)
	}

	workdir := hostpaths.Resolve(filepath.Join("lyzn-tasks", taskID))
	dir := chatlog.Dir(workdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","timestamp":"2026-09-14T10:00:00Z","message":{"content":[{"type":"text","text":"do the thing"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, cliSessionID+".jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	w := taskTranscript(srv, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "do the thing") {
		t.Errorf("body missing the transcript's own message: %s", body)
	}
	if !strings.Contains(body, `"live":false`) {
		t.Errorf("nothing is running for this task; body = %s", body)
	}
}
