// Package broker decides what an extension may do, and meters what it did.
//
// One mechanism, three payoffs: it is the sandbox for third-party loops, the
// permission model an org tier hands out, and the point every billable call
// already passes through.
//
// The rule is default deny: a subject holding no grants is refused everything.
// The exception is the Ungated tier, for code compiled into the daemon that
// already holds the daemon's authority — see Trust.
package broker

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Trust decides the default for a subject that has no explicit grant.
type Trust int

const (
	// Community is third-party code: nothing is permitted unless granted.
	Community Trust = iota
	// Registry is reviewed code: still granted explicitly, but refusals are
	// recorded rather than silent, since a reviewed loop asking for something
	// it was not granted is a packaging bug worth seeing.
	Registry
	// Ungated is compiled into the daemon and already runs with the daemon's own
	// authority, so a gate it could walk around would be theatre. Calls are
	// still metered, which is the half that is not theatre.
	//
	// Every loop is Ungated today because `loops install` compiles third-party
	// Go into the binary. The tier stops being the common case when loops move
	// to WASM and arrive with a capability manifest.
	Ungated
)

// Denied reports a refused capability.
type Denied struct {
	Subject    string
	Capability string
	Value      string
	Reason     string
}

func (d *Denied) Error() string {
	return fmt.Sprintf("broker: %s may not %s %q (%s)", d.Subject, d.Capability, d.Value, d.Reason)
}

// IsDenied reports whether an error is a capability refusal rather than a
// failure to check.
func IsDenied(err error) bool {
	_, ok := err.(*Denied)
	return ok
}

type Broker struct {
	store *store.Store
	log   *zap.Logger

	mu    sync.RWMutex
	trust map[string]Trust
	// cache holds a subject's grants briefly, so a loop making many calls does
	// not re-read the table for each one. Short enough that a revocation takes
	// effect within a run.
	cache    map[string]cached
	cacheTTL time.Duration
}

type cached struct {
	grants []store.Grant
	at     time.Time
}

func New(s *store.Store, log *zap.Logger) *Broker {
	return &Broker{
		store:    s,
		log:      log,
		trust:    map[string]Trust{},
		cache:    map[string]cached{},
		cacheTTL: 5 * time.Second,
	}
}

// SetTrust records a subject's tier.
func (b *Broker) SetTrust(subject string, t Trust) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trust[subject] = t
}

// Handle is a subject's view of the broker. Extensions receive one of these
// rather than the broker itself, so they cannot ask about anybody else.
type Handle struct {
	b       *Broker
	subject string
}

// For returns the handle for one subject.
func (b *Broker) For(subject string) *Handle { return &Handle{b: b, subject: subject} }

// Subject is who this handle speaks for.
func (h *Handle) Subject() string { return h.subject }

// Check asks about any capability class by name. Used where the class is
// decided at runtime rather than at the call site.
func (h *Handle) Check(class, value string) error {
	return h.b.check(h.subject, class, value)
}

// Tool reports whether this subject may call a tool.
func (h *Handle) Tool(name string) error {
	return h.b.check(h.subject, store.CapTool, name)
}

// HTTP reports whether this subject may reach a host.
func (h *Handle) HTTP(host string) error {
	return h.b.check(h.subject, store.CapHTTP, strings.ToLower(host))
}

// Memory reports whether this subject may read, or write, a namespace.
func (h *Handle) Memory(namespace string, write bool) error {
	return h.b.check(h.subject, store.CapMemory, store.MemoryValue(namespace, write))
}

// Channel reports whether this subject may send on a comms channel.
func (h *Handle) Channel(id string) error {
	return h.b.check(h.subject, store.CapChannel, id)
}

// Spend books units against this subject's daily ceiling, refusing when it
// would be exceeded. Booked before the work, so an over-budget call does not
// happen and then get reported.
func (h *Handle) Spend(units int64) error {
	if units <= 0 {
		return nil
	}
	limit, ok, err := h.b.spendLimit(h.subject)
	if err != nil {
		return err
	}
	if ok {
		used, err := h.b.store.UsageToday(h.subject, store.CapSpend)
		if err != nil {
			return err
		}
		if used+units > limit {
			_ = h.b.store.MeterCapability(h.subject, store.CapSpend, false, 0)
			return &Denied{Subject: h.subject, Capability: store.CapSpend,
				Value:  fmt.Sprintf("%d units", units),
				Reason: fmt.Sprintf("today's ceiling of %d is used up (%d spent)", limit, used)}
		}
	}
	return h.b.store.MeterCapability(h.subject, store.CapSpend, true, units)
}

// check is the whole decision.
func (b *Broker) check(subject, capability, value string) error {
	b.mu.RLock()
	trust := b.trust[subject]
	b.mu.RUnlock()

	if trust == Ungated {
		// Compiled into the daemon: it already has the daemon's authority, and
		// a gate it could walk around is worse than no gate.
		return b.store.MeterCapability(subject, capability, true, 0)
	}

	grants, err := b.grantsFor(subject)
	if err != nil {
		// Fail closed. A broker that cannot read its own table must not become
		// a broker that permits everything.
		return fmt.Errorf("broker: could not read grants for %s: %w", subject, err)
	}
	for _, g := range grants {
		if g.Capability != capability {
			continue
		}
		if matches(g.Value, value) {
			return b.store.MeterCapability(subject, capability, true, 0)
		}
	}

	_ = b.store.MeterCapability(subject, capability, false, 0)
	b.log.Warn("capability refused",
		zap.String("subject", subject), zap.String("capability", capability),
		zap.String("value", value))
	return &Denied{Subject: subject, Capability: capability, Value: value,
		Reason: "not granted"}
}

// matches compares a granted pattern against a requested value.
//
// Only two forms, because a grant language nobody can read at a glance is a
// grant language that gets approved without being read: "*" for everything in
// the class, and a "prefix.*" or "*.suffix" wildcard.
func matches(pattern, value string) bool {
	if pattern == store.CapWildcard || pattern == value {
		return true
	}
	if strings.HasSuffix(pattern, ".*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(value, strings.TrimPrefix(pattern, "*"))
	}
	return false
}

func (b *Broker) grantsFor(subject string) ([]store.Grant, error) {
	b.mu.RLock()
	c, ok := b.cache[subject]
	b.mu.RUnlock()
	if ok && time.Since(c.at) < b.cacheTTL {
		return c.grants, nil
	}

	grants, err := b.store.Grants(subject)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cache[subject] = cached{grants: grants, at: time.Now()}
	b.mu.Unlock()
	return grants, nil
}

func (b *Broker) spendLimit(subject string) (int64, bool, error) {
	grants, err := b.grantsFor(subject)
	if err != nil {
		return 0, false, err
	}
	for _, g := range grants {
		if g.Capability != store.CapSpend {
			continue
		}
		if g.Value == store.CapWildcard {
			return 0, false, nil // uncapped
		}
		var n int64
		if _, err := fmt.Sscanf(g.Value, "%d", &n); err == nil && n > 0 {
			return n, true, nil
		}
	}
	// No spend grant at all means no spending, which is the default-deny rule
	// applied to the one capability where zero is a meaningful answer.
	return 0, true, nil
}

// Grant records a permission and drops the cached decision for that subject.
func (b *Broker) Grant(g store.Grant) error {
	if err := b.store.SaveGrant(g); err != nil {
		return err
	}
	b.forget(g.Subject)
	return nil
}

// Revoke withdraws one permission.
func (b *Broker) Revoke(subject, capability, value string) error {
	if err := b.store.RevokeGrant(subject, capability, value); err != nil {
		return err
	}
	b.forget(subject)
	return nil
}

// RevokeAll withdraws everything a subject holds.
func (b *Broker) RevokeAll(subject string) error {
	if _, err := b.store.RevokeSubject(subject); err != nil {
		return err
	}
	b.forget(subject)
	return nil
}

// Describe renders a subject's permissions in the plain English an operator has
// to approve at install time.
func (b *Broker) Describe(subject string) ([]string, error) {
	grants, err := b.store.Grants(subject)
	if err != nil {
		return nil, err
	}
	if len(grants) == 0 {
		return []string{"nothing — this subject holds no permissions"}, nil
	}
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, describe(g))
	}
	return out, nil
}

func describe(g store.Grant) string {
	switch g.Capability {
	case store.CapTool:
		if g.Value == store.CapWildcard {
			return "call ANY tool"
		}
		return "call the tool " + g.Value
	case store.CapHTTP:
		if g.Value == store.CapWildcard {
			return "make HTTP requests to ANY host"
		}
		return "make HTTP requests to " + g.Value
	case store.CapMemory:
		ns, write := store.SplitMemoryValue(g.Value)
		if write {
			return "WRITE memory in " + ns
		}
		return "read memory in " + ns
	case store.CapChannel:
		if g.Value == store.CapWildcard {
			return "send on ANY channel"
		}
		return "send messages on " + g.Value
	case store.CapSpend:
		if g.Value == store.CapWildcard {
			return "spend without a daily limit"
		}
		return "spend up to " + g.Value + " units per day"
	}
	return g.Capability + ": " + g.Value
}

func (b *Broker) forget(subject string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cache, subject)
}

// LoopSubject names a loop as a broker subject.
func LoopSubject(name string) string { return "loop:" + name }

// PeerSubject names a mesh peer as a broker subject.
func PeerSubject(id string) string { return "peer:" + id }

// ConnectorSubject names a connector as a broker subject.
func ConnectorSubject(id string) string { return "connector:" + id }
