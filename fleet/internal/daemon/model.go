package daemon

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// modelFamilies are the aliases Claude Code takes for /model and --model.
var modelFamilies = []string{"sonnet", "opus", "haiku"}

// modelID is a full model id, as typed: letters, digits, dots and dashes,
// and an optional context suffix such as [1m]. Nothing else reaches the pane.
var modelID = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]*(\[[0-9a-z]+\])?$`)

// NormalizeModel checks a model name the operator gave: sonnet, opus,
// haiku or a full id. Aliases are lowercased.
func NormalizeModel(s string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(s))
	if !modelID.MatchString(m) {
		return "", fmt.Errorf("%w: model %q: want sonnet, opus, haiku or a full model id", ErrBadInput, s)
	}
	return m, nil
}

// family is the alias a model name is in ("" when it is in none).
func family(m string) string {
	for _, f := range modelFamilies {
		if strings.Contains(m, f) {
			return f
		}
	}
	return ""
}

// ModelAllowed reports whether an agent's models: list allows m: the same
// name, or an alias and a full id in its family.
func ModelAllowed(list []string, m string) bool {
	for _, e := range list {
		e = strings.ToLower(e)
		if e == m || (e == family(m) && family(e) == e) || (m == family(m) && family(e) == m) {
			return true
		}
	}
	return false
}

// modelIs reports whether the live model (the status line's id and display
// name) is the model wanted: an alias matches its family, a full id matches
// the live id, which may lack a date suffix.
func modelIs(want, id, display string) bool {
	if id == "" && display == "" {
		return false
	}
	id, display = strings.ToLower(id), strings.ToLower(display)
	want = strings.TrimSuffix(want, "[1m]")
	if want == family(want) {
		return strings.Contains(id, want) || strings.Contains(display, want)
	}
	return id != "" && (strings.HasPrefix(want, id) || strings.HasPrefix(id, want)) || strings.EqualFold(display, want)
}

// modelAskGrace is how long a session has, after it is asked for a model,
// to show it on the status line before you are told it did not take.
const modelAskGrace = 2 * time.Minute

// ModelReply is fleetctl model's answer.
type ModelReply struct {
	Agent string `json:"agent"`
	// Default is the model in fleet.yaml, Override the one set with
	// fleetctl model, Task the open task's (run --model).
	Default  string `json:"default,omitempty"`
	Override string `json:"override,omitempty"`
	Task     string `json:"task,omitempty"`
	// Wanted is the one of those that applies; Live is what the status
	// line showed last.
	Wanted string `json:"wanted,omitempty"`
	Live   string `json:"live,omitempty"`
}

// wantedModel is the model an agent's session should run: its open task's,
// else its override, else the fleet.yaml default; "" for the CLI's own.
func (f *Fleet) wantedModel(name string, st reconcile.State) ModelReply {
	r := ModelReply{Agent: name, Default: f.worker(name).Model, Live: st.Model}
	var err error
	if r.Override, err = f.db.ModelOverride(name); err != nil {
		f.Log("ledger: model override of %s: %v", name, err)
	}
	if st.Task != "" && !st.Done {
		if t, err := f.db.Task(st.Task); err == nil && t.Closed.IsZero() {
			r.Task = t.Model
		}
	}
	r.Wanted = firstNonEmpty(r.Task, r.Override, r.Default)
	return r
}

// Model says which model an agent is set to run and which it does.
func (f *Fleet) Model(name string) (ModelReply, error) {
	if err := f.known(name); err != nil {
		return ModelReply{}, err
	}
	return f.wantedModel(name, f.State(name)), nil
}

// SetModel sets an agent's model override, which the ledger keeps across
// restarts; an empty model clears it. fleetd types it into the session at
// the next idle tick.
func (f *Fleet) SetModel(name, model string, now time.Time) error {
	if err := f.known(name); err != nil {
		return fmt.Errorf("%w: %v", ErrBadInput, err)
	}
	if model != "" {
		var err error
		if model, err = NormalizeModel(model); err != nil {
			return err
		}
	}
	if err := f.db.SetModelOverride(name, model, now); err != nil {
		return err
	}
	f.event(name, "fleet.model_set", "", map[string]string{"model": model})
	return nil
}

// applyModel types /model into an idle session whose model is not the one
// wanted, once per change (and once more in each fresh session or process).
// It returns the state to keep. A session that was asked and still shows
// another model after modelAskGrace is alerted once.
func (f *Fleet) applyModel(ctx context.Context, name string, b Box, st reconcile.State, status bool, idle bool, now time.Time) reconcile.State {
	if st.SessionID == "" || st.Done {
		return st
	}
	want := f.wantedModel(name, st).Wanted
	key := st.SessionID + "/" + st.Proc
	if want == "" || (status && modelIs(want, st.ModelID, st.Model)) {
		if status && want != "" {
			delete(st.Alerted, "model")
		}
		return st
	}
	if !status {
		return st // the live model is not known yet: no status line from this session
	}
	if st.ModelAsked == want && st.ModelAskedKey == key {
		if now.Sub(st.ModelAskedAt) >= modelAskGrace && !st.Alerted["model"] && f.notify != nil {
			if st.Alerted == nil {
				st.Alerted = map[string]bool{}
			}
			st.Alerted["model"] = true
			text := fmt.Sprintf("%s was asked for %s and still shows %s", name, want, st.Model)
			if f.firstAlert(text, now) {
				if err := f.notify.Notify(ctx, text); err != nil {
					f.Log("alert %s: %v", name, err)
				}
			}
			f.event(name, "fleet.alert", "", map[string]string{"text": text})
		}
		return st
	}
	if !idle {
		return st // never mid-turn
	}
	if err := b.Prompt(ctx, "/model "+want); err != nil {
		f.Log("model %s: %v", name, err)
		return st // asked again next tick
	}
	f.event(name, "fleet.model", st.SessionID, map[string]string{"model": want, "was": st.Model})
	st.ModelAsked, st.ModelAskedKey, st.ModelAskedAt = want, key, now
	return st
}

// setModelNow asks a session for its task's model before the brief goes in,
// so the task starts on it: the agent was picked idle, and what the last
// tick saw of its model is the live one, if it saw one.
func (f *Fleet) setModelNow(ctx context.Context, name string) {
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	st = f.applyModel(ctx, name, f.box(name), st, true, true, time.Now())
	f.setState(st)
}
