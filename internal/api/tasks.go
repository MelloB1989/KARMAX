// A LYZN task's transcript, read back the same way a chat conversation is.
//
// A claimed LYZN task runs through claude_code.call under a session key of
// exactly "lyzn:<task id>" (see pruneStaleLyznSessions's own comment: the
// naming is deterministic by construction, no lookup table needed for the
// working directory). The CLI session uuid that key resolves to — and so the
// transcript itself — only exists once the store's key->uuid mapping has
// been minted, which happens after the first turn actually completes.
package api

import (
	"net/http"
	"path/filepath"
	"regexp"

	"github.com/MelloB1989/karmax/internal/chatlog"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
)

// lyznSessionPrefix mirrors internal/runtime/lyzntasks.go's own constant —
// duplicated rather than imported because internal/runtime already imports
// internal/api (for ChatEvent/ChatTurnOptions) and the reverse import would
// be a cycle.
const lyznSessionPrefix = "lyzn:"

// validTaskID guards a taskId that reaches this package straight from an HTTP
// path and is joined into both a session key and a directory name — the same
// concern chatlog.sessionPath has for a conversation id, just checked here
// because a LYZN task id is not a uuid chatlog would otherwise validate.
var validTaskID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// handleTaskTranscript serves a LYZN task's transcript, in the same shape as
// GET /api/chat/conversations/{id}, plus whether the task's own claude_code
// call is running right now.
func (s *Server) handleTaskTranscript(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("taskId")
	if !validTaskID.MatchString(taskID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid task id"})
		return
	}

	sessionKey := lyznSessionPrefix + taskID
	if s.store == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no transcript for that task"})
		return
	}
	cliSessionID, err := s.store.GetSessionKey(sessionKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// "" with no error is the store's own way of saying nothing has been
	// minted yet — the ordinary state before a task's first turn completes,
	// not a failure. See store.GetSessionKey.
	if cliSessionID == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no transcript for that task"})
		return
	}

	// The exact working directory pruneStaleLyznSessions recomputes from a
	// task id, and so the exact project slug the CLI wrote the transcript
	// under (chatlog.Dir).
	workdir := hostpaths.Resolve(filepath.Join("lyzn-tasks", taskID))
	msgs, err := chatlog.Read(chatlog.Dir(workdir), cliSessionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no transcript for that task"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": msgs,
		"live":     builtin.IsRunning(sessionKey),
	})
}
