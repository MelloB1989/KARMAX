package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/identity"
)

// A sandbox agent (fleet.yaml sandbox: true) holds no relay token: its one
// credential is its sandbox key (identity.SandboxKey), a bearer token that
// means exactly "sandbox agent X". With it the agent asks who it is, reads
// and waits on its own task, and tells or asks the one who gave it that task.
// Every other route refuses it.

// sandbox is the scope of a sandbox key.
const sandbox scope = "sandbox"

// sandboxRoutes are the only routes a sandbox key may use.
var sandboxRoutes = map[string]bool{
	"GET /v1/whoami":     true,
	"POST /v1/tell":      true, // to its task's assigner only (sandboxTell)
	"POST /v1/ask":       true, // the daemon asks its task's assigner
	"GET /v1/tasks/{id}": true, // its own task (sandboxOwns)
	"GET /v1/wait":       true, // on its own task
}

// withSandboxScope adds the sandbox scope to a route that allows it.
func withSandboxScope(pattern string, allowed []scope) []scope {
	if sandboxRoutes[pattern] {
		return and(allowed, sandbox)
	}
	return allowed
}

// sandboxOf is the sandbox agent a bearer token is the key of.
func (t Tokens) sandboxOf(bearer string) (string, bool) {
	return identity.SandboxOf(t.Full, bearer, t.Sandboxes)
}

// withSandbox proves which sandbox worker a request with a sandbox key is
// from: the key's agent, or a slot of it named in the identity header with
// that same key. It stands in for withAgent's check, which it hides the
// header from, so verifiedAgent answers for it.
func withSandbox(tok Tokens, isWorker func(string) bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		agent, ok := tok.sandboxOf(bearer)
		if !ok {
			h.ServeHTTP(w, r)
			return
		}
		worker := agent
		if v := r.Header.Get(identity.Header); v != "" {
			i := strings.LastIndex(v, ":")
			name, key := v[:max(i, 0)], v[i+1:]
			if i < 1 || subtle.ConstantTimeCompare([]byte(key), []byte(bearer)) != 1 {
				writeErr(w, http.StatusForbidden, fmt.Errorf("%s does not verify: a sandbox sends its own FLEET_SANDBOX_KEY", identity.Header))
				return
			}
			if identity.Base(name) != agent || !isWorker(name) {
				writeErr(w, http.StatusForbidden, fmt.Errorf("this is %s's sandbox key: it does not speak as %s", agent, name))
				return
			}
			worker = name
		}
		r = r.WithContext(context.WithValue(r.Context(), agentKey{}, worker))
		r.Header = r.Header.Clone()
		r.Header.Del(identity.Header)
		h.ServeHTTP(w, r)
	})
}

// sandboxTask is the task a sandbox worker holds now, and who gave it.
func sandboxTask(r *http.Request, ops Ops) (worker, task, assigner string, err error) {
	worker, _ = verifiedAgent(r)
	for _, row := range ops.Status(time.Now()) {
		if row.Agent == worker {
			task = row.Task
		}
	}
	if task == "" {
		return worker, "", "", forbidden{fmt.Errorf("%s has no task: a sandbox speaks only to whoever gave it its task", worker)}
	}
	assigner, err = taskAssigner(ops, task)
	if err != nil {
		return worker, task, "", forbidden{fmt.Errorf("%s: %v", worker, err)}
	}
	return worker, task, assigner, nil
}

// sandboxTell is a sandbox's tell: from itself, to its task's assigner only.
func sandboxTell(r *http.Request, ops Ops, from, to string) (string, error) {
	worker, task, assigner, err := sandboxTask(r, ops)
	if err != nil {
		return "", err
	}
	if from != "" && from != worker {
		return "", forbidden{fmt.Errorf("this is %s's sandbox key: it does not speak as %s", worker, from)}
	}
	if to != assigner {
		return "", forbidden{fmt.Errorf("%s tells only %s, who gave it task %s", worker, assigner, task)}
	}
	return worker, nil
}

// sandboxOwns refuses a sandbox any task but the one it holds.
func sandboxOwns(r *http.Request, ops Ops, task string) error {
	_, mine, _, err := sandboxTask(r, ops)
	if err != nil {
		return err
	}
	if task == "" || task != mine {
		return forbidden{fmt.Errorf("a sandbox reads and waits on its own task (%s) only", mine)}
	}
	return nil
}
