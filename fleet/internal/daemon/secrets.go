package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// Secrets the operator hands agents for a task. fleetd keeps them sealed on
// the ledger (internal/vault) and an agent reads one only while it is
// granted, to the agent or to its task, and only on a task the operator, an
// operator's session or the orchestrator gave it: never on a member's.
// A value is never logged, put in an event or an error, or listed.

// ErrForbidden is a read the rules refuse.
var ErrForbidden = errors.New("forbidden")

// ErrNoGrant is a revoke of a grant there is not.
var ErrNoGrant = errors.New("no such grant")

// maxSecret is the largest value fleetd keeps: an API key or an SSH key,
// not a file.
const maxSecret = 64 << 10

var secretName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func checkSecretName(name string) error {
	if !secretName.MatchString(name) {
		return fmt.Errorf("%w: secret name %q: letters, digits, '.', '_' and '-', at most 64", ErrBadInput, name)
	}
	return nil
}

// SecretPut stores a value under a name, replacing one before it.
func (f *Fleet) SecretPut(name string, value []byte, now time.Time) error {
	if err := checkSecretName(name); err != nil {
		return err
	}
	if len(value) == 0 || len(value) > maxSecret {
		return fmt.Errorf("%w: a secret is 1 byte to %d KiB (this one is %d bytes)", ErrBadInput, maxSecret>>10, len(value))
	}
	sealed, err := f.vault.Seal(name, value)
	if err != nil {
		return err
	}
	if err := f.db.PutSecret(name, sealed, now); err != nil {
		return err
	}
	f.event("", "fleet.secret.put", "", map[string]any{"secret": name, "size": len(value)})
	return nil
}

// Secrets is every secret's name, grants and last read: never a value.
func (f *Fleet) Secrets(now time.Time) ([]ledger.SecretInfo, error) { return f.db.Secrets(now) }

// SecretGrant lets an agent, or whoever works a task, read a secret; for
// ttl when it is not 0. A task's grant ends when the task closes.
func (f *Fleet) SecretGrant(name, task, agent, by string, ttl time.Duration, now time.Time) error {
	if (task == "") == (agent == "") {
		return fmt.Errorf("%w: grant to a task or an agent, one of them", ErrBadInput)
	}
	if ttl < 0 {
		return fmt.Errorf("%w: a ttl is positive", ErrBadInput)
	}
	if agent != "" {
		if err := f.known(agent); err != nil {
			return fmt.Errorf("%w: %v", ErrBadInput, err)
		}
	}
	if task != "" {
		// A task not yet run may be granted ahead; a closed one is over.
		if t, err := f.db.Task(task); err == nil && !t.Closed.IsZero() {
			return fmt.Errorf("%w: task %s is closed", ErrConflict, task)
		}
	}
	g := ledger.Grant{Secret: name, Task: task, Agent: agent, By: by, Granted: now}
	if ttl > 0 {
		g.Expires = now.Add(ttl)
	}
	if err := f.db.AddGrant(g); err != nil {
		return err
	}
	f.event(agent, "fleet.secret.grant", "", map[string]any{"secret": name, "task": task, "agent": agent, "expires": g.Expires})
	return nil
}

// SecretRevoke ends a grant.
func (f *Fleet) SecretRevoke(name, task, agent string) error {
	if (task == "") == (agent == "") {
		return fmt.Errorf("%w: revoke from a task or an agent, one of them", ErrBadInput)
	}
	ok, err := f.db.Revoke(name, task, agent)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s is not granted to %s", ErrNoGrant, name, cmp.Or(task, agent))
	}
	f.event(agent, "fleet.secret.revoke", "", map[string]any{"secret": name, "task": task, "agent": agent})
	return nil
}

// SecretRemove drops a secret and its grants.
func (f *Fleet) SecretRemove(name string) error {
	ok, err := f.db.RemoveSecret(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ledger.ErrNoSecret, name)
	}
	f.event("", "fleet.secret.rm", "", map[string]any{"secret": name})
	return nil
}

// SecretRead is a secret's value for an agent, whose name the caller has
// verified. The rest is the ledger's word, not the agent's: its task is the
// one fleetd gave it, open, assigned by someone who may hand out secrets, and
// the secret is granted to the agent or that task. Every read is an event,
// and so is every refusal; neither carries the value.
func (f *Fleet) SecretRead(agent, name string, now time.Time) ([]byte, error) {
	task, err := f.mayUseSecrets(agent)
	if err == nil {
		var ok bool
		if ok, err = f.db.Granted(name, agent, task, now); err == nil && !ok {
			err = fmt.Errorf("%w: %s is not granted to %s or its task %s", ErrForbidden, name, agent, task)
		}
	}
	var value []byte
	if err == nil {
		var sealed []byte
		if sealed, err = f.db.SealedSecret(name); err == nil {
			value, err = f.vault.Open(name, sealed)
		}
	}
	if err != nil {
		f.event(agent, "fleet.secret.denied", "", map[string]any{"secret": name, "task": task, "error": err.Error()})
		return nil, err
	}
	if err := f.db.SecretRead(name, agent, task, now); err != nil {
		f.Log("ledger: secret read: %v", err)
	}
	f.event(agent, "fleet.secret.read", "", map[string]any{"secret": name, "task": task})
	return value, nil
}

// mayUseSecrets is the agent's open task when that task lets it use secrets
// (and memory, and the gateway): one the operator, an operator's session or
// the orchestrator gave it.
func (f *Fleet) mayUseSecrets(agent string) (string, error) {
	if err := f.known(agent); err != nil {
		return "", fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	task := f.State(agent).Task
	if task == "" {
		return "", fmt.Errorf("%w: %s has no task", ErrForbidden, agent)
	}
	t, err := f.db.Task(task)
	if err != nil || t.Agent != agent || !t.Closed.IsZero() {
		return task, fmt.Errorf("%w: %s has no open task", ErrForbidden, agent)
	}
	if !f.operatorAssigned(t.Assigner) {
		return task, fmt.Errorf("%w: task %s was assigned by %s, a member: its agent may not use the operator's secrets",
			ErrForbidden, task, cmp.Or(t.Assigner, "nobody on record"))
	}
	return task, nil
}

// operatorAssigned reports whether a task's assigner may hand its agent the
// operator's data. Only the full scope names an assigner (a member's run is
// always theirs, by login), so a name without an @ is the operator, an
// operator's session or the orchestrator; a login is a member unless the
// tailnet lists it as an operator.
func (f *Fleet) operatorAssigned(assigner string) bool {
	switch {
	case assigner == "":
		return false
	case !strings.Contains(assigner, "@"):
		return true
	}
	return f.cfg.Tailnet.Role(assigner) == "operator"
}

// redactSecrets is the one hook on the message path: every stored secret's
// value in a message is replaced with [secret:<name>] before the message is
// stored or delivered. It covers every secret, granted or not — an agent that
// read one before a revoke still knows it — and each long line of a
// multi-line value (part of a key is still a leak).
func (f *Fleet) redactSecrets(text string) string {
	sealed, err := f.db.SealedSecrets()
	if err != nil || len(sealed) == 0 {
		return text
	}
	type needle struct{ value, name string }
	var ns []needle
	for name, s := range sealed {
		v, err := f.vault.Open(name, s)
		if err != nil {
			f.Log("redact: secret %s: %v", name, err)
			continue
		}
		whole := strings.TrimSpace(string(v))
		if len(whole) >= 4 {
			ns = append(ns, needle{whole, name})
		}
		if strings.Contains(whole, "\n") {
			for _, line := range strings.Split(whole, "\n") {
				if line = strings.TrimSpace(line); len(line) >= 20 {
					ns = append(ns, needle{line, name})
				}
			}
		}
	}
	slices.SortFunc(ns, func(a, b needle) int { return len(b.value) - len(a.value) }) // the whole before its parts
	for _, n := range ns {
		text = strings.ReplaceAll(text, n.value, "[secret:"+n.name+"]")
	}
	return text
}
