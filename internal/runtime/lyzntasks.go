// The six-hour backstop for LYZN task sessions.
//
// Every terminal path — done, failed, a question that expired, a daemon that
// gave up its pinned tasks — is supposed to clean up after itself already.
// This exists for what falls through anyway: the tail, not the common case.
package runtime

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// lyznSessionPrefix is the deterministic key every LYZN task's durable
// session carries.
const lyznSessionPrefix = "lyzn:"

// pruneStaleLyznSessions finds coding_sessions rows nobody reclaimed and
// cleans each one up, recomputing the transcript path and working directory
// from the task id embedded in the session id itself — no lookup table is
// needed, because the naming is deterministic by construction.
func (rt *KarmaxRuntime) pruneStaleLyznSessions(before time.Time) {
	ids, err := rt.store.ListStaleCodingSessionIDs(lyznSessionPrefix, before)
	if err != nil {
		rt.log.Warn("could not list stale LYZN task sessions", zap.Error(err))
		return
	}
	tool := &builtin.ClaudeCodeTool{Store: rt.store}
	for _, sessionID := range ids {
		taskID := strings.TrimPrefix(sessionID, lyznSessionPrefix)
		workdir := hostpaths.Resolve(filepath.Join("lyzn-tasks", taskID))
		if err := tool.Cleanup(workdir, sessionID); err != nil {
			rt.log.Warn("could not clean up a stale LYZN task session",
				zap.String("session_id", sessionID), zap.Error(err))
		}
	}
}
