package connectors

import (
	"context"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// Checking whether integrations still work, without waiting for someone to ask.
//
// Health used to be evaluated only when a human clicked the button in the
// console, and remembered only in memory. So a freshly booted install reported
// every connector as "not checked yet" — including the ones answering
// perfectly — and the first restart after a check threw the answer away again.
//
// A status nobody has established is not a status. This establishes it: once
// shortly after boot, then on a slow ticker.

// ProbeInterval is how often connectors are re-checked.
//
// Slow on purpose. These are third-party APIs with rate limits, and the
// question "does this credential still work" changes on the order of days —
// when a token expires or somebody revokes an app — not minutes.
const ProbeInterval = 30 * time.Minute

// probeDelay is how long after boot the first sweep runs. Long enough that a
// restart does not add three vendor round trips to the critical path.
const probeDelay = 45 * time.Second

// StartProbing keeps every configured connector's health up to date until ctx
// is cancelled.
func (h *Host) StartProbing(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(probeDelay):
		}
		h.ProbeAll(ctx)

		t := time.NewTicker(ProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.ProbeAll(ctx)
			}
		}
	}()
}

// ProbeAll checks every connector that has credentials and records the result.
func (h *Host) ProbeAll(ctx context.Context) {
	for _, m := range h.Available() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		h.Probe(ctx, m.ID)
	}
}

// Probe checks one connector and persists the verdict.
//
// Returns the verdict so a caller doing this on demand can answer with it
// rather than reading its own write back.
func (h *Host) Probe(ctx context.Context, id string) (res store.ConnectorHealth) {
	res = store.ConnectorHealth{Connector: id, CheckedAt: time.Now()}

	// Health() reaches into third-party client libraries. A panic in one of
	// them, on the background goroutine this usually runs on, would take the
	// whole daemon down — an integration being broken must not be able to stop
	// the agent answering.
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("a connector panicked during its health check",
				zap.String("connector", id), zap.Any("panic", r))
			res.Status = "failed"
			res.Detail = "the connector panicked while checking its health"
			h.record(res)
		}
	}()

	if h.store == nil {
		res.Status, res.Detail = "not_configured", "no credential store"
		return res
	}

	c, ok := h.Get(id)
	if !ok {
		res.Status, res.Detail = "not_configured", "no such connector"
		return res
	}

	cred, _ := h.store.Credential(id)
	known := connectorkit.Credentials{Config: map[string]string{}}
	if cred != nil {
		known.Config, known.AccessToken = cred.Config, cred.AccessToken
		if cred.ExpiresAt != nil {
			known.ExpiresAt = *cred.ExpiresAt
		}
	}

	switch {
	case c.Manifest().PerUser:
		// Nobody in particular is being acted for here, and checking a per-user
		// connector as the install would either fail or — worse — succeed using
		// whichever employee's token happened to be first.
		res.Status, res.Detail = perUserStatus(h, id, cred != nil)
		h.record(res)
		return res

	case cred == nil && !SelfConfigured(c, known):
		res.Status, res.Detail = "not_configured", "No credentials saved yet"
		h.record(res)
		return res
	}

	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	if err := c.Health(cctx, known); err != nil {
		res.Status, res.Detail = "failed", err.Error()
	} else {
		res.Status, res.Detail = "healthy", "Reachable"
	}
	h.record(res)
	return res
}

// perUserStatus describes a connector that authenticates as individuals.
func perUserStatus(h *Host, id string, appConfigured bool) (string, string) {
	if !appConfigured {
		return "not_configured", "No OAuth app configured yet"
	}
	people, err := h.store.ListUserCredentials(id)
	if err != nil || len(people) == 0 {
		return "degraded", "The app is set up, but nobody has connected their account yet"
	}
	if len(people) == 1 {
		return "healthy", "1 person connected"
	}
	return "healthy", itoa(len(people)) + " people connected"
}

func (h *Host) record(res store.ConnectorHealth) {
	if err := h.store.SaveConnectorHealth(res); err != nil {
		h.log.Warn("could not record a connector's health",
			zap.String("connector", res.Connector), zap.Error(err))
	}
}

// SelfConfigured asks a connector whether it has what it needs from somewhere
// other than the credential store.
func SelfConfigured(c connectorkit.Connector, known connectorkit.Credentials) bool {
	sc, ok := c.(connectorkit.SelfConfigured)
	return ok && sc.Configured(known)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
