package googleacct

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Client reaches every discovered account. It implements gcal.API and
// gchat.API, and the actions KARMAX takes as the operator.
type Client struct {
	h   *Host
	loc *time.Location

	mu     sync.Mutex
	health map[string]Health
}

// NewClient builds a client over a host. loc is where all-day dates fall.
func NewClient(h *Host, loc *time.Location) *Client {
	if loc == nil {
		loc = time.UTC
	}
	return &Client{h: h, loc: loc, health: map[string]Health{}}
}

// Host is the machine the client reaches Google through.
func (c *Client) Host() *Host { return c.h }

// Health is the last thing learned about reaching one account.
type Health struct {
	Account string
	// Path and Dir name the login the last call used.
	Path Path
	Dir  string
	// Err is the last call's failure, nil when it worked.
	Err error
	At  time.Time
}

// Health returns the last outcome for an account.
func (c *Client) Health(account string) Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health[strings.ToLower(account)]
}

func (c *Client) record(account string, l Login, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health[strings.ToLower(account)] = Health{Account: strings.ToLower(account), Path: l.Path, Dir: l.Dir, Err: err, At: time.Now()}
}

// AuthError says an account could not be reached by any login, and what to
// run to fix it.
type AuthError struct {
	Account string
	Service string
	// Tried are the logins that failed, best first.
	Tried []Login
	// Why is each login's failure, in the same order.
	Why []string
}

func (e *AuthError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Google account %s cannot be reached", e.Account)
	if e.Service != "" {
		fmt.Fprintf(&b, " for %s", e.Service)
	}
	if len(e.Why) > 0 {
		var parts []string
		for i, w := range e.Why {
			parts = append(parts, e.Tried[i].String()+": "+w)
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, "; "))
	}
	fmt.Fprintf(&b, ". To fix, run on the KARMAX host: %s", strings.Join(e.Fixes(), "  — or —  "))
	return b.String()
}

// Fixes are the commands that would restore access, best first.
func (e *AuthError) Fixes() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range e.Tried {
		if f := l.Fix(); !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if !seen[(Login{Email: e.Account, Path: Gog}).Fix()] {
		out = append(out, (Login{Email: e.Account, Path: Gog}).Fix())
	}
	return out
}

// UnknownAccountError is a call for an address no CLI holds.
type UnknownAccountError struct {
	Account string
	Known   []string
}

func (e *UnknownAccountError) Error() string {
	known := "none"
	if len(e.Known) > 0 {
		known = strings.Join(e.Known, ", ")
	}
	return fmt.Sprintf("no Google login on this host is for %s (signed in: %s); add it with `gog auth add %s --services calendar,chat`",
		e.Account, known, e.Account)
}

// with runs op through an account's logins, best first, moving to the next
// only when a login's sign-in is the problem. Any other failure is the
// answer: a second login would only fail the same way.
func (c *Client) with(ctx context.Context, account, service string, op func(Login) error) error {
	account = strings.ToLower(strings.TrimSpace(account))
	d := c.h.Accounts(ctx)
	a, ok := d.Find(account)
	if !ok {
		known := d.Emails()
		sort.Strings(known)
		err := &UnknownAccountError{Account: account, Known: known}
		c.record(account, Login{}, err)
		return err
	}
	ae := &AuthError{Account: account, Service: service}
	for _, l := range a.Logins {
		if l.Problem != "" {
			ae.Tried, ae.Why = append(ae.Tried, l), append(ae.Why, l.Problem)
			continue
		}
		if !l.Can(service) {
			ae.Tried, ae.Why = append(ae.Tried, l), append(ae.Why, "not granted "+service)
			continue
		}
		err := op(l)
		var ce *cliError
		if errors.As(err, &ce) && ce.auth {
			ae.Tried, ae.Why = append(ae.Tried, l), append(ae.Why, ce.detail)
			continue
		}
		// Health is about reaching the account. A refused request — not a
		// guest of that event, an expired sync token — reached it fine.
		reach := error(nil)
		if errors.As(err, &ce) && !ce.gone {
			reach = err
		}
		c.record(account, l, reach)
		return err
	}
	// Something about the host may have changed — a login removed or added —
	// so the next call looks again rather than trusting the cache.
	c.h.Forget()
	c.record(account, Login{}, ae)
	return ae
}

// gws runs a gws command as one login and decodes its JSON output.
func (c *Client) gws(ctx context.Context, l Login, op string, out any, args ...string) error {
	env := append(append([]string{}, c.h.env()...), "GOOGLE_WORKSPACE_CLI_CONFIG_DIR="+l.Dir)
	stdout, stderr, code, err := c.h.Run(ctx, env, c.h.Gws, args...)
	if err != nil {
		return classify(Gws, op, code, stdout, stderr, err)
	}
	if out == nil {
		return nil
	}
	if err := decodeJSON(stdout, out); err != nil {
		return fmt.Errorf("gws %s: %v", op, err)
	}
	return nil
}

// gwsPages runs a paginated gws list and hands each page to fn.
func (c *Client) gwsPages(ctx context.Context, l Login, op string, fn func([]byte) error, args ...string) error {
	env := append(append([]string{}, c.h.env()...), "GOOGLE_WORKSPACE_CLI_CONFIG_DIR="+l.Dir)
	args = append(args, "--page-all", "--page-limit", "40")
	stdout, stderr, code, err := c.h.Run(ctx, env, c.h.Gws, args...)
	if err != nil {
		return classify(Gws, op, code, stdout, stderr, err)
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if err := fn([]byte(line)); err != nil {
			return fmt.Errorf("gws %s: %v", op, err)
		}
	}
	return nil
}

// gog runs a gog command as one login and decodes its JSON output.
func (c *Client) gog(ctx context.Context, l Login, op string, out any, args ...string) error {
	full := append(append([]string{}, args...), "--account", l.Email, "--json", "--no-input")
	stdout, stderr, code, err := c.h.Run(ctx, c.h.env(), c.h.Gog, full...)
	if err != nil {
		return classify(Gog, op, code, stdout, stderr, err)
	}
	if out == nil {
		return nil
	}
	if err := decodeJSON(stdout, out); err != nil {
		return fmt.Errorf("gog %s: %v", op, err)
	}
	return nil
}
