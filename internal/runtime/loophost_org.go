package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sandbox"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The organisational half of the Kit.
//
// These are the methods that separate a loop working for one person from an
// agent working inside a company: it belongs to a case, it waits on what other
// people do, it speaks into shared channels, and it is answerable afterwards.

// --- Cases ------------------------------------------------------------------

func (k *loopKit) CaseOpen(agent, key, title string) (loopkit.Case, error) {
	if strings.TrimSpace(key) == "" {
		return loopkit.Case{}, fmt.Errorf("a case needs a key")
	}
	// The pack's name, not this workflow's. Six recipes opening cases under six
	// names is six robots; one name is the colleague the operator hired.
	if strings.TrimSpace(agent) == "" {
		agent = k.loopName
	}
	c, err := k.rt.store.OpenCase(store.Case{
		ID:        uuid.New().String(),
		Agent:     agent,
		Key:       key,
		Title:     title,
		State:     "open",
		Namespace: caseNamespace(agent),
	})
	if err != nil {
		return loopkit.Case{}, err
	}
	return toKitCase(c), nil
}

func (k *loopKit) CaseGet(key string) (loopkit.Case, bool, error) {
	c, found, err := k.rt.store.CaseByKey(key)
	if err != nil || !found {
		return loopkit.Case{}, found, err
	}
	return toKitCase(c), true, nil
}

func (k *loopKit) CaseSetState(caseID, state string) error {
	if err := k.rt.store.SetCaseState(caseID, state); err != nil {
		return err
	}
	// The state change is itself an event, so a notify workflow can hang off it
	// rather than every workflow having to remember to announce itself.
	k.rt.emitCaseState(caseID, state)
	return k.Audit("case.state", caseID, state, "")
}

func (k *loopKit) CaseLog(caseID, kind, payload string) error {
	return k.rt.store.AppendCaseEvent(store.CaseEvent{
		ID: uuid.New().String(), CaseID: caseID, Kind: kind,
		Payload: payload, Actor: k.loopName,
	})
}

func (k *loopKit) CaseHistory(caseID string, limit int) ([]string, error) {
	events, err := k.rt.store.CaseHistory(caseID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s  %s  %s  %s",
			e.CreatedAt.UTC().Format(time.RFC3339), e.Actor, e.Kind, e.Payload))
	}
	return out, nil
}

// CaseSay speaks in the case's own thread, starting it on the first message.
//
// The binding is the point: every later workflow on this case joins the same
// thread instead of opening a new one, which is the difference between one
// colleague reporting progress and six bots each starting a conversation.
func (k *loopKit) CaseSay(ctx context.Context, caseID, channel, text string) error {
	c, found, err := k.rt.store.CaseByID(caseID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no case %q", caseID)
	}
	if channel == "" {
		channel = c.ThreadChannel
	}
	if channel == "" {
		return fmt.Errorf("case %s has no channel to speak in", c.Key)
	}

	ts, err := k.rt.postToCase(ctx, channel, c.ThreadTS, text)
	if err != nil {
		return err
	}
	// First message on this case: it becomes the thread.
	if c.ThreadTS == "" && ts != "" {
		if err := k.rt.store.BindCaseThread(c.ID, channel, ts); err != nil {
			k.rt.log.Warn("could not bind a case to its thread",
				zap.String("case", c.Key), zap.Error(err))
		}
	}
	_ = k.CaseLog(c.ID, "said", truncate(text, 200))
	return k.Audit("case.say", channel, "sent", truncate(text, 200))
}

func caseNamespace(agent string) string { return "agent:" + agent }

func toKitCase(c store.Case) loopkit.Case {
	return loopkit.Case{
		ID: c.ID, Key: c.Key, Agent: c.Agent, Title: c.Title, State: c.State,
		Namespace: c.Namespace, ThreadChannel: c.ThreadChannel, ThreadTS: c.ThreadTS,
	}
}

// --- Await ------------------------------------------------------------------

// Await parks the run on an event, or returns the payload of the event that
// already woke it.
//
// The two halves are one function on purpose. A resumed run replays its steps
// from the top, reaches this call again, and must this time find the answer
// waiting rather than arm a second waiter and park forever.
func (k *loopKit) Await(ctx context.Context, id string, spec loopkit.AwaitSpec) (map[string]any, error) {
	if k.executionID == "" {
		return nil, fmt.Errorf("await needs a durable execution; this run has none")
	}
	if spec.Event == "" {
		return nil, fmt.Errorf("await needs an event kind")
	}

	if res, ok, err := k.rt.store.WaiterResult(k.executionID, id); err != nil {
		return nil, err
	} else if ok {
		var out map[string]any
		if res != "" {
			if err := json.Unmarshal([]byte(res), &out); err != nil {
				return nil, fmt.Errorf("await result was not readable: %w", err)
			}
		}
		// An expired wait resolves too — with a timeout marker rather than an
		// event, so the recipe can tell "it happened" from "it never did".
		if t, ok := out["timeout"].(bool); ok && t {
			return out, fmt.Errorf("await %q timed out", spec.Event)
		}
		return out, nil
	}

	match, err := json.Marshal(spec.Match)
	if err != nil {
		return nil, err
	}
	w := store.Waiter{
		ID: uuid.New().String(), ExecutionID: k.executionID, Loop: k.loopName,
		Step: id, CaseID: spec.CaseID, EventKind: spec.Event, MatchJSON: string(match),
	}
	if spec.Timeout > 0 {
		t := time.Now().Add(spec.Timeout)
		w.ExpiresAt = &t
	}
	if err := k.rt.store.ArmWaiter(w); err != nil {
		return nil, err
	}
	k.Logf("waiting on %s (%s)", spec.Event, string(match))
	return nil, fmt.Errorf("%w: %s", loopkit.ErrSuspended, spec.Event)
}

// --- Speaking ---------------------------------------------------------------

func (k *loopKit) SendTo(ctx context.Context, channel, thread, content string) error {
	if err := k.rt.sendToChannel(ctx, channel, thread, content); err != nil {
		return err
	}
	return k.Audit("send", channel, "sent", truncate(content, 200))
}

// ProposeTo asks a role rather than a person. Everyone holding it is asked and
// the first decision settles it, so an approval does not sit behind whichever
// individual happens to be on leave.
func (k *loopKit) ProposeTo(role, title, summary, action string) (string, error) {
	members, err := k.rt.store.OrgMembersByRole(role)
	if err != nil {
		return "", err
	}
	if len(members) == 0 {
		return "", fmt.Errorf("nobody holds the role %q", role)
	}
	id := uuid.New().String()
	if err := k.rt.store.CreateProposal(store.StoredProposal{
		ID: id, AgentID: k.loopName, Kind: "role:" + role,
		Title: title, Summary: summary, ProposedAction: action, Status: "pending",
	}); err != nil {
		return "", err
	}
	k.rt.askRole(context.Background(), role, members, id, title, summary)
	return id, k.Audit("propose", "role:"+role, "pending", title)
}

// --- Sandboxed work ---------------------------------------------------------

func (k *loopKit) Sandbox(ctx context.Context, id string, spec loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
	raw, err := k.Step(id, func() (string, error) {
		res, err := k.rt.runSandbox(ctx, k.loopName, spec)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(res)
		return string(b), err
	})
	if err != nil {
		return loopkit.SandboxResult{}, err
	}
	var out loopkit.SandboxResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return loopkit.SandboxResult{}, err
	}
	return out, nil
}

// --- Answerability ----------------------------------------------------------

func (k *loopKit) Audit(verb, target, decision, detail string) error {
	return k.rt.store.AppendAudit(store.AuditEvent{
		ID: uuid.New().String(), ActorKind: "loop", ActorID: k.loopName,
		Agent: k.loopName, Recipe: k.loopName, Verb: verb,
		Target: target, Decision: decision, Detail: detail,
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runSandbox launches a container, waits for it, and keeps sandbox_runs honest
// throughout — the row is what lets a restart find a container it started.
func (rt *KarmaxRuntime) runSandbox(ctx context.Context, agent string, spec loopkit.SandboxSpec) (loopkit.SandboxResult, error) {
	drv, err := rt.sandboxDriver()
	if err != nil {
		return loopkit.SandboxResult{}, err
	}

	runID := uuid.New().String()
	image := spec.Image
	if image == "" {
		image = rt.sandboxImage()
	}

	env, err := rt.sandboxCredentials(ctx, spec.Repo, spec.Env)
	if err != nil {
		return loopkit.SandboxResult{}, err
	}

	if err := rt.store.StartSandboxRun(store.SandboxRun{
		ID: runID, CaseID: spec.CaseID, Driver: drv.Name(), Image: image,
		Status: sandbox.StateStarting, Repo: spec.Repo, Branch: spec.Branch,
		Task: spec.Task, StartedAt: time.Now(),
	}); err != nil {
		return loopkit.SandboxResult{}, err
	}

	cid, err := drv.Launch(ctx, sandbox.Spec{
		Image: image, Repo: spec.Repo, Branch: spec.Branch,
		Task: spec.Task, Env: env, Timeout: spec.Timeout,
	})
	if err != nil {
		_ = rt.store.UpdateSandboxRun(runID, sandbox.StateFailed, 0, err.Error(), "")
		return loopkit.SandboxResult{}, err
	}
	rt.log.Info("sandbox launched", zap.String("run", runID), zap.String("container", cid),
		zap.String("repo", spec.Repo))

	deadline := time.Now().Add(spec.Timeout)
	if spec.Timeout <= 0 {
		deadline = time.Now().Add(defaultSandboxTimeout)
	}
	for {
		st, err := drv.Poll(ctx, cid)
		if err != nil {
			_ = rt.store.UpdateSandboxRun(runID, sandbox.StateFailed, 0, err.Error(), "")
			return loopkit.SandboxResult{}, err
		}
		if st.State == sandbox.StateExited || st.State == sandbox.StateFailed || st.State == sandbox.StateGone {
			logs, _ := drv.Logs(ctx, cid, 200)
			_ = rt.store.UpdateSandboxRun(runID, st.State, st.ExitCode, "", logs)
			_ = drv.Kill(ctx, cid)
			return loopkit.SandboxResult{RunID: runID, Status: st.State, ExitCode: st.ExitCode, LogTail: logs}, nil
		}
		if time.Now().After(deadline) {
			logs, _ := drv.Logs(ctx, cid, 200)
			_ = drv.Kill(ctx, cid)
			_ = rt.store.UpdateSandboxRun(runID, sandbox.StateFailed, 0, "timed out", logs)
			return loopkit.SandboxResult{}, fmt.Errorf("sandbox timed out after %s", time.Since(deadline))
		}
		select {
		case <-ctx.Done():
			return loopkit.SandboxResult{}, ctx.Err()
		case <-time.After(sandboxPollEvery):
		}
	}
}
