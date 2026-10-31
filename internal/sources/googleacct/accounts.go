// Package googleacct finds every Google account this host can act as, and
// reaches each one through the most robust way it has.
//
// Two CLIs hold Google logins on the operator's machines, and neither is a
// superset of the other:
//
//   - gog holds any number of accounts in one place, and is the only one that
//     can use a service account, which never needs a person to sign in again.
//     Its Chat output is reshaped, though: no mentions, no sender type, no
//     space activity times, and its calendar listing hides cancellations.
//   - gws holds one account per config directory and speaks the raw API:
//     calendar sync tokens, mentions, activity times.
//
// So each account's logins are ranked — a service account first, then gws,
// then gog's OAuth — and a call falls back to the next login when one has lost
// its sign-in. Accounts are discovered afresh (with a short cache) on every
// use, so an account signed in tomorrow morning is picked up without a
// restart, and one signed out stops being used.
package googleacct

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Path names a way of reaching Google.
type Path string

const (
	Gog Path = "gog"
	Gws Path = "gws"
)

// Login is one way this host can act as one Google account.
type Login struct {
	Email string
	Path  Path
	// Dir is the gws config directory holding the login (gws only).
	Dir string
	// ServiceAccount is a gog login through a service account with
	// domain-wide delegation.
	ServiceAccount bool
	// Services are what the login was granted ("calendar", "chat", and the
	// Gmail powers GmailRead, GmailBody, GmailModify and GmailSend), when the
	// path says; empty means unknown and is not held against it.
	Services []string
	// Scopes are the OAuth scopes Google granted the login, when the path
	// lists them: what a fix must ask for again so it costs nothing.
	Scopes []string
	// Problem says why the login exists but cannot be used.
	Problem string

	defaultDir bool   // Dir is the one gws uses with no override
	keyring    string // the keyring backend this host runs gws with
}

// Can reports whether the login was granted a service.
func (l Login) Can(service string) bool {
	if l.ServiceAccount || len(l.Services) == 0 {
		return true
	}
	for _, s := range l.Services {
		if s == service {
			return true
		}
	}
	return false
}

// Fix is the command the operator runs on this host to sign the login in
// again.
func (l Login) Fix() string { return l.FixFor("") }

// FixFor is the command that signs the login in again with service granted
// as well as everything it already has — so adding Gmail's send scope does
// not cost it the calendar. An empty service, or one the login already
// holds, is a plain sign-in again.
func (l Login) FixFor(service string) string {
	adding := service != "" && !l.Can(service)
	switch l.Path {
	case Gws:
		var env []string
		if l.keyring != "" {
			// The daemon reads gws's store with this backend; a login made
			// without it lands somewhere the daemon cannot see.
			env = append(env, "GOOGLE_WORKSPACE_CLI_KEYRING_BACKEND="+l.keyring)
		}
		if !l.defaultDir && l.Dir != "" {
			env = append(env, "GOOGLE_WORKSPACE_CLI_CONFIG_DIR="+l.Dir)
		}
		cmd := "gws auth login"
		if adding && len(l.Scopes) > 0 {
			// gws signs in with exactly the scopes it is given (adding the
			// identity ones itself), so the fix lists what the login has as
			// well as what it lacks.
			cmd += " --scopes " + strings.Join(scopesWith(l.Scopes, service), ",")
		}
		return strings.TrimSpace(strings.Join(env, " ") + " " + cmd)
	default:
		if l.ServiceAccount {
			return fmt.Sprintf("gog auth service-account set %s --key <service-account.json>", l.Email)
		}
		var services []string
		for _, s := range l.Services {
			// Every service gog granted is kept (drive, docs, …); the Gmail
			// powers are KARMAX's reading of its "gmail".
			if s != "none" && !strings.HasPrefix(s, "gmail.") {
				services = appendOnce(services, s)
			}
		}
		if len(services) == 0 {
			services = []string{"calendar", "chat", "gmail"}
		}
		if gs := gogService(service); gs != "" {
			services = appendOnce(services, gs)
		}
		cmd := fmt.Sprintf("gog auth add %s --services %s", l.Email, strings.Join(services, ","))
		if adding && (service == GmailModify || service == GmailSend) {
			cmd += " --gmail-scope full" // gog grants gmail.modify, not read-only
		}
		return cmd + " --force-consent"
	}
}

func (l Login) rank() int {
	switch {
	case l.Problem != "":
		return 0
	case l.ServiceAccount:
		return 3
	case l.Path == Gws:
		return 2
	default:
		return 1
	}
}

func (l Login) String() string {
	if l.Path == Gws {
		return fmt.Sprintf("gws (%s)", l.Dir)
	}
	if l.ServiceAccount {
		return "gog (service account)"
	}
	return "gog"
}

// Account is one Google address and the logins that reach it, best first.
type Account struct {
	Email  string
	Logins []Login
}

// Lacks reports whether no working login of the account was granted service.
func (a Account) Lacks(service string) bool {
	for _, l := range a.Logins {
		if l.Problem == "" && l.Can(service) {
			return false
		}
	}
	return true
}

// FixFor is the command that grants service to the account's best login.
func (a Account) FixFor(service string) string {
	for _, l := range a.Logins {
		if l.Problem == "" {
			return l.FixFor(service)
		}
	}
	if len(a.Logins) > 0 {
		return a.Logins[0].FixFor(service)
	}
	return (Login{Email: a.Email, Path: Gog}).FixFor(service)
}

// Discovery is what one look at the host found.
type Discovery struct {
	Accounts []Account
	// Errors are CLIs that could not be asked; their accounts are missing.
	Errors []string
	At     time.Time
}

// Find returns the account with this address.
func (d Discovery) Find(email string) (Account, bool) {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, a := range d.Accounts {
		if a.Email == email {
			return a, true
		}
	}
	return Account{}, false
}

// Emails lists the discovered addresses.
func (d Discovery) Emails() []string {
	out := make([]string, 0, len(d.Accounts))
	for _, a := range d.Accounts {
		out = append(out, a.Email)
	}
	return out
}

// Host is this machine's Google CLIs.
type Host struct {
	// Gog and Gws are the binaries; empty when not installed.
	Gog, Gws string
	// GwsDirs lists the gws config directories to look in, one login each.
	GwsDirs func() []string
	// DefaultGwsDir is the directory gws uses with no override.
	DefaultGwsDir string
	// HasLogin reports whether a gws directory holds a stored login, so an
	// empty directory costs no process.
	HasLogin func(dir string) bool
	// Stamp identifies a gws directory's stored credentials (their size and
	// time). `gws auth status` asks Google about the token, which takes
	// seconds, so a directory's login is reused while its stamp is unchanged.
	// Nil asks every time.
	Stamp func(dir string) string
	Run   Runner
	// Env is the environment the CLIs run with; nil means this process's.
	Env []string
	// TTL is how long a discovery is reused (60s).
	TTL time.Duration

	mu      sync.Mutex
	last    Discovery
	gwsSeen map[string]gwsCached
}

// gwsCached is one gws directory's login as last asked.
type gwsCached struct {
	stamp string
	at    time.Time
	login Login
}

// gwsRecheck is how long a gws login is trusted without asking again, even
// with unchanged credentials: a token revoked at Google changes nothing on
// disk.
const gwsRecheck = 30 * time.Minute

// NewHost describes this machine: gog at gogPath, gws found on PATH or where
// bun installs it, and gws directories at the default plus every
// ~/.config/gws-* beside it and any listed in KARMAX_GWS_CONFIG_DIRS.
func NewHost(gogPath string, getenv func(string) string) *Host {
	home, _ := os.UserHomeDir()
	def := strings.TrimSpace(getenv("GOOGLE_WORKSPACE_CLI_CONFIG_DIR"))
	if def == "" {
		def = filepath.Join(home, ".config", "gws")
	}
	extra := strings.TrimSpace(getenv("KARMAX_GWS_CONFIG_DIRS"))
	h := &Host{
		Gog:           runnable(gogPath),
		Gws:           findGws(getenv, home),
		DefaultGwsDir: def,
		Run:           ExecRunner,
	}
	h.Stamp = func(dir string) string {
		var parts []string
		for _, f := range []string{"credentials.enc", "credentials.json"} {
			if st, err := os.Stat(filepath.Join(dir, f)); err == nil && st.Size() > 0 {
				parts = append(parts, fmt.Sprintf("%s:%d:%d", f, st.Size(), st.ModTime().UnixNano()))
			}
		}
		return strings.Join(parts, ",")
	}
	h.HasLogin = func(dir string) bool { return h.Stamp(dir) != "" }
	h.GwsDirs = func() []string {
		dirs := []string{def}
		if siblings, err := filepath.Glob(filepath.Join(filepath.Dir(def), filepath.Base(def)+"-*")); err == nil {
			dirs = append(dirs, siblings...)
		}
		for _, d := range filepath.SplitList(extra) {
			if d = strings.TrimSpace(d); d != "" {
				dirs = append(dirs, d)
			}
		}
		return dirs
	}
	return h
}

func runnable(p string) string {
	if p == "" {
		return ""
	}
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func findGws(getenv func(string) string, home string) string {
	if p := strings.TrimSpace(getenv("KARMAX_GWS_PATH")); p != "" {
		return runnable(p)
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if p := runnable(filepath.Join(dir, "gws")); p != "" {
			return p
		}
	}
	for _, p := range []string{".bun/bin/gws", ".local/bin/gws", "go/bin/gws"} {
		if r := runnable(filepath.Join(home, p)); r != "" {
			return r
		}
	}
	return ""
}

func (h *Host) env() []string {
	if h.Env != nil {
		return h.Env
	}
	return os.Environ()
}

func (h *Host) envValue(k string) string {
	for _, kv := range h.env() {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v
		}
	}
	return ""
}

// Accounts returns a recent discovery, looking again when it is stale.
func (h *Host) Accounts(ctx context.Context) Discovery {
	ttl := h.TTL
	if ttl <= 0 {
		ttl = time.Minute
	}
	h.mu.Lock()
	last := h.last
	h.mu.Unlock()
	if !last.At.IsZero() && time.Since(last.At) < ttl {
		return last
	}
	d := h.Discover(ctx)
	h.mu.Lock()
	h.last = d
	h.mu.Unlock()
	return d
}

// Forget drops the cached discovery and every cached login, so the next call
// looks again.
func (h *Host) Forget() {
	h.mu.Lock()
	h.last = Discovery{}
	h.gwsSeen = nil
	h.mu.Unlock()
}

// gwsLoginCached is gwsLogin, reusing the last answer while the directory's
// credentials are unchanged.
func (h *Host) gwsLoginCached(ctx context.Context, dir string) (Login, error) {
	stamp := ""
	if h.Stamp != nil {
		stamp = h.Stamp(dir)
	}
	if stamp != "" {
		h.mu.Lock()
		c, ok := h.gwsSeen[dir]
		h.mu.Unlock()
		if ok && c.stamp == stamp && time.Since(c.at) < gwsRecheck {
			return c.login, nil
		}
	}
	l, err := h.gwsLogin(ctx, dir)
	if err != nil || stamp == "" {
		return l, err
	}
	h.mu.Lock()
	if h.gwsSeen == nil {
		h.gwsSeen = map[string]gwsCached{}
	}
	h.gwsSeen[dir] = gwsCached{stamp: stamp, at: time.Now(), login: l}
	h.mu.Unlock()
	return l, nil
}

// Discover asks both CLIs which accounts they hold.
func (h *Host) Discover(ctx context.Context) Discovery {
	d := Discovery{At: time.Now()}
	byEmail := map[string]*Account{}
	add := func(l Login) {
		l.Email = strings.ToLower(strings.TrimSpace(l.Email))
		if l.Email == "" {
			return
		}
		a := byEmail[l.Email]
		if a == nil {
			a = &Account{Email: l.Email}
			byEmail[l.Email] = a
		}
		a.Logins = append(a.Logins, l)
	}

	if h.Gog != "" {
		logins, err := h.gogLogins(ctx)
		if err != nil {
			d.Errors = append(d.Errors, err.Error())
		}
		for _, l := range logins {
			add(l)
		}
	}
	if h.Gws != "" && h.GwsDirs != nil {
		seen := map[string]bool{}
		for _, dir := range h.GwsDirs() {
			dir = filepath.Clean(dir)
			if seen[dir] || (h.HasLogin != nil && !h.HasLogin(dir)) {
				continue
			}
			seen[dir] = true
			l, err := h.gwsLoginCached(ctx, dir)
			if err != nil {
				d.Errors = append(d.Errors, err.Error())
				continue
			}
			add(l)
		}
	}

	for _, a := range byEmail {
		sort.SliceStable(a.Logins, func(i, j int) bool { return a.Logins[i].rank() > a.Logins[j].rank() })
		d.Accounts = append(d.Accounts, *a)
	}
	sort.Slice(d.Accounts, func(i, j int) bool { return d.Accounts[i].Email < d.Accounts[j].Email })
	return d
}

func (h *Host) gogLogins(ctx context.Context) ([]Login, error) {
	stdout, stderr, code, err := h.Run(ctx, h.env(), h.Gog, "auth", "list", "--json", "--no-input")
	if err != nil {
		return nil, classify(Gog, "auth list", code, stdout, stderr, err)
	}
	var listed struct {
		Accounts []struct {
			Email    string   `json:"email"`
			Services []string `json:"services"`
			Scopes   []string `json:"scopes"`
			Auth     string   `json:"auth"`
			Error    string   `json:"error"`
		} `json:"accounts"`
	}
	if err := decodeJSON(stdout, &listed); err != nil {
		return nil, fmt.Errorf("gog auth list: unreadable output: %v", err)
	}
	var out []Login
	for _, a := range listed.Accounts {
		l := Login{Email: a.Email, Path: Gog, ServiceAccount: strings.Contains(a.Auth, "service_account"), Scopes: a.Scopes}
		for _, s := range a.Services {
			if s != "service-account" {
				l.Services = append(l.Services, s)
			}
		}
		if contains(l.Services, "gmail") {
			// gog grants Gmail as gmail.modify unless it was signed in
			// read-only, which its scopes then say.
			powers := []string{GmailRead, GmailBody, GmailModify, GmailSend}
			if len(a.Scopes) > 0 {
				powers = gmailPowers(a.Scopes)
			}
			for _, p := range powers {
				l.Services = appendOnce(l.Services, p)
			}
		}
		if a.Error != "" {
			l.Problem = "gog cannot read its stored login: " + redact(a.Error)
		}
		out = append(out, l)
	}
	return out, nil
}

func (h *Host) gwsLogin(ctx context.Context, dir string) (Login, error) {
	env := append(append([]string{}, h.env()...), "GOOGLE_WORKSPACE_CLI_CONFIG_DIR="+dir)
	stdout, stderr, code, err := h.Run(ctx, env, h.Gws, "auth", "status")
	if err != nil {
		return Login{}, classify(Gws, "auth status ("+dir+")", code, stdout, stderr, err)
	}
	var st struct {
		User            string   `json:"user"`
		HasRefreshToken bool     `json:"has_refresh_token"`
		Scopes          []string `json:"scopes"`
	}
	if err := decodeJSON(stdout, &st); err != nil {
		return Login{}, fmt.Errorf("gws auth status (%s): unreadable output: %v", dir, err)
	}
	if strings.TrimSpace(st.User) == "" {
		return Login{}, fmt.Errorf("gws in %s holds credentials but names no account; sign it in again with `GOOGLE_WORKSPACE_CLI_CONFIG_DIR=%s gws auth login`", dir, dir)
	}
	l := Login{Email: st.User, Path: Gws, Dir: dir, defaultDir: filepath.Clean(dir) == filepath.Clean(h.DefaultGwsDir),
		keyring: h.envValue("GOOGLE_WORKSPACE_CLI_KEYRING_BACKEND"), Scopes: st.Scopes}
	for _, s := range st.Scopes {
		switch {
		case strings.Contains(s, "/auth/calendar"):
			l.Services = appendOnce(l.Services, "calendar")
		case strings.Contains(s, "/auth/chat.messages"):
			l.Services = appendOnce(l.Services, "chat")
		}
	}
	l.Services = append(l.Services, gmailPowers(st.Scopes)...)
	if len(l.Services) == 0 {
		l.Services = []string{"none"} // known, and none of calendar, chat or Gmail
	}
	if !st.HasRefreshToken {
		l.Problem = "gws has no refresh token for it, so it is signed out once the current token expires"
	}
	return l, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
