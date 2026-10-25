package daemon

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// ReasonUnguarded is why a sandbox is disabled when its network probe finds
// it can reach what it must not, or cannot reach fleetd: the host's
// fleet-sandbox-net rules are missing.
const ReasonUnguarded = "sandbox network unguarded"

// sandboxProbeEvery is how often each sandbox's network is probed.
const sandboxProbeEvery = time.Hour

// sandboxProbe runs in a sandbox's container: each argument is an address
// and port it must not reach within 3 s, and fleetd (FLEETD_URL, its relay)
// must answer. It says what is wrong on stderr and exits 1.
const sandboxProbe = `fail=
for t in "$@"; do
  if timeout 3 bash -c 'exec 3<>"/dev/tcp/${1%:*}/${1##*:}"' _ "$t" 2>/dev/null; then fail="$fail $t answers;"; fi
done
curl -fsS -m 5 -o /dev/null "$FLEETD_URL/healthz" 2>/dev/null || fail="$fail fleetd ($FLEETD_URL) does not answer;"
[ -z "$fail" ] || { echo "$fail" >&2; exit 1; }`

// probeSandboxes checks, once an hour, from inside every sandbox, that the
// host and the LAN refuse it and fleetd answers (sandboxProbe). One that
// fails is disabled, and you are told once; one that passes again comes
// back. fleetd needs no root to check the rules this way.
func (f *Fleet) probeSandboxes(ctx context.Context, now time.Time) {
	if !f.probedAt.IsZero() && now.Sub(f.probedAt) < sandboxProbeEvery {
		return
	}
	f.probedAt = now
	var wg sync.WaitGroup
	for _, name := range f.cfg.AgentNames() {
		if !f.cfg.Agents[name].Sandbox {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			f.probeSandbox(ctx, name, now)
		}(name)
	}
	wg.Wait()
}

func (f *Fleet) probeSandbox(ctx context.Context, name string, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	argv := append([]string{"bash", "-c", sandboxProbe, "probe"}, sandboxTargets(f.cfg.Hosts[f.cfg.Agents[name].Host].FleetdURL)...)
	res, err := f.box(name).Rsh(ctx, argv, strings.NewReader(""), io.Discard)
	if err != nil {
		f.Log("probe sandbox %s: %v", name, err) // not run (the container is down): no verdict
		return
	}
	workers := append([]string{name}, f.cfg.Siblings(name)...)
	if res.Exit == 0 {
		for _, n := range workers {
			if st := f.State(n); st.Phase == reconcile.Disabled && st.DisabledReason == ReasonUnguarded {
				f.enable(n)
			}
		}
		return
	}
	why := strings.TrimSpace(res.Stderr)
	disabled := false
	for _, n := range workers {
		if f.disable(n, ReasonUnguarded) {
			disabled = true
			f.event(n, "fleet.sandbox_unguarded", "", map[string]string{"why": why})
		}
	}
	if disabled && f.notify != nil {
		host := f.cfg.Agents[name].Host
		text := fmt.Sprintf("%s disabled: %s (%s) — install or restart fleet-sandbox-net on %s (docs/AGENT-FLEET.md, Sandbox agents)", name, ReasonUnguarded, why, host)
		if err := f.notify.Notify(ctx, text); err != nil {
			f.Log("alert: %v", err)
		}
		f.event(name, "fleet.alert", "", map[string]string{"text": text})
	}
}

// sandboxTargets is what a sandbox must not reach: ssh on its host itself,
// the tailnet (Tailscale's own resolver, which answers wherever the host is
// on one), and ssh on fleetd's address when its fleetd_url names one (a LAN
// address, open to it only on fleetd's ports).
func sandboxTargets(fleetdURL string) []string {
	out := []string{"host.docker.internal:22", "100.100.100.100:53"}
	if u, err := url.Parse(fleetdURL); err == nil && net.ParseIP(u.Hostname()) != nil {
		out = append(out, net.JoinHostPort(u.Hostname(), "22"))
	}
	return out
}

// disable marks a worker disabled for why, unless it already is (for this
// or another reason); it reports whether it changed it.
func (f *Fleet) disable(name, why string) bool {
	f.locks[name].Lock()
	defer f.locks[name].Unlock()
	st := f.State(name)
	if st.Phase == reconcile.Disabled {
		return false
	}
	st.Phase, st.DisabledReason = reconcile.Disabled, why
	f.setState(st)
	w := f.worker(name)
	_ = f.db.UpsertAgent(ledger.Agent{Name: name, Host: w.Host, Account: w.Account, Models: w.Models,
		Container: w.Container, Enabled: false, Reason: why})
	return true
}
