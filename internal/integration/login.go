package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Logging in, for each way a provider makes you do it.
//
// The point of putting all four here is that an operator learns ONE command.
// Slack wants two tokens pasted, Google wants a browser round trip, wacli holds
// its own session and just needs pairing — and none of that should be something
// you have to know before you can connect something.

// Prompter asks the operator for a value. Injected so the flows can be tested
// without a terminal.
type Prompter interface {
	// Ask reads a value. secret means it must not be echoed.
	Ask(field connectorkit.ConfigField) (string, error)
	// Say tells the operator something.
	Say(format string, args ...any)
	// Open launches a URL in a browser, or explains how to reach it.
	Open(url string) error
}

// LoginResult is what a flow obtained.
type LoginResult struct {
	Config    map[string]string
	Access    string
	Refresh   string
	ExpiresAt *time.Time
	// Message is what to tell the operator on success.
	Message string
}

// Login runs the right flow for an integration and stores what it gets.
//
// Health is called BEFORE anything is saved: a key that does not work should
// fail at the moment it is typed, while the operator still has the page open,
// not hours later inside a loop.
func (r *Registry) Login(ctx context.Context, id string, p Prompter) error {
	in, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("no integration called %q — `karmax integrations` lists them", id)
	}
	m, auth := in.Manifest(), in.Auth()

	p.Say("%s — %s", m.Name, m.Description)
	if m.SetupURL != "" {
		p.Say("Get the credentials here: %s", m.SetupURL)
	}

	var (
		res LoginResult
		err error
	)
	switch auth.Kind {
	case connectorkit.AuthNone:
		p.Say("%s needs no credentials.", m.Name)
		return nil
	case connectorkit.AuthAPIKey:
		res, err = loginAPIKey(m, p)
	case connectorkit.AuthOAuth2:
		res, err = loginOAuth2(ctx, m, auth, r, p)
	case connectorkit.AuthCLI:
		res, err = loginCLI(ctx, m, auth, in, r, p)
	default:
		return fmt.Errorf("%s uses an authentication method KARMAX does not know (%q)", m.Name, auth.Kind)
	}
	if err != nil {
		return err
	}

	// Verified before it is stored.
	if auth.Kind != connectorkit.AuthCLI {
		check := connectorkit.Credentials{Config: res.Config, AccessToken: res.Access}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		herr := in.Health(cctx, check)
		cancel()
		if herr != nil {
			return fmt.Errorf("those credentials did not work, so nothing was saved: %w", herr)
		}
	}

	if err := r.creds.Save(id, res.Config, res.Access, res.Refresh, res.ExpiresAt); err != nil {
		return err
	}
	if res.Message != "" {
		p.Say("%s", res.Message)
	}
	p.Say("Connected. `karmax integrations` will show it.")
	return nil
}

// loginAPIKey prompts for each declared field.
func loginAPIKey(m Manifest, p Prompter) (LoginResult, error) {
	out := LoginResult{Config: map[string]string{}}
	for _, f := range m.Config {
		v, err := p.Ask(f)
		if err != nil {
			return out, err
		}
		v = strings.TrimSpace(v)
		if v == "" {
			if f.Required {
				return out, fmt.Errorf("%s is required", f.Key)
			}
			v = f.Default
		}
		if v != "" {
			out.Config[f.Key] = v
		}
	}
	return out, nil
}

// loginCLI reports on a session the host binary holds itself.
//
// KARMAX cannot log in for wacli or gws — the session is theirs, in their own
// store. What it can do is check, and say the exact command rather than leaving
// somebody to work out which of the binary's subcommands does it.
func loginCLI(ctx context.Context, m Manifest, auth connectorkit.AuthMethod,
	in Integration, r *Registry, p Prompter) (LoginResult, error) {

	creds, _, _ := r.creds.Resolve(m.ID)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := in.Health(cctx, creds)
	cancel()
	if err == nil {
		return LoginResult{Message: fmt.Sprintf("%s is already signed in.", m.Name)}, nil
	}

	cmd := auth.CLIBinary
	if cmd == "" {
		cmd = m.ID
	}
	return LoginResult{}, fmt.Errorf(
		"%s holds its own session and is not signed in: %v\n"+
			"  sign in with: %s\n"+
			"  then run `karmax integrations` to confirm",
		m.Name, err, loginHint(cmd))
}

// loginHint is the command that signs a host binary in.
func loginHint(binary string) string {
	base := binary
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "wacli":
		return binary + " login    (scan the QR with WhatsApp)"
	case "gws":
		return binary + " auth login"
	}
	return binary + " login"
}

// loginOAuth2 runs the browser round trip against a loopback callback.
//
// Loopback rather than a hosted redirect: KARMAX runs on somebody's laptop or a
// Pi behind their router, so there is no public URL to redirect to, and asking
// them to expose one to connect Notion would be absurd. The device flow is used
// instead when the provider offers it, since a headless box has no browser at
// all.
func loginOAuth2(ctx context.Context, m Manifest, auth connectorkit.AuthMethod,
	r *Registry, p Prompter) (LoginResult, error) {

	if auth.OAuth2 == nil {
		return LoginResult{}, fmt.Errorf("%s declares OAuth but carries no configuration for it", m.Name)
	}
	cfg := auth.OAuth2

	// The client id and secret are the app's, not the operator's session, so
	// they come from config like any other field.
	existing, _, _ := r.creds.Resolve(m.ID)
	clientID := existing.Get(cfg.ClientIDKey)
	clientSecret := existing.Get(cfg.SecretKey)
	out := LoginResult{Config: map[string]string{}}

	for key, val := range map[string]*string{cfg.ClientIDKey: &clientID, cfg.SecretKey: &clientSecret} {
		if key == "" || strings.TrimSpace(*val) != "" {
			continue
		}
		v, err := p.Ask(connectorkit.ConfigField{
			Key: key, Description: "from the app you registered with " + m.Name,
			Required: true, Secret: strings.Contains(key, "secret"),
		})
		if err != nil {
			return out, err
		}
		*val = strings.TrimSpace(v)
		out.Config[key] = *val
	}
	if clientID == "" {
		return out, fmt.Errorf("%s needs a client id before it can authorise", m.Name)
	}

	host, port := callbackAddress()
	listeners, err := listenLoopback(host, port)
	if err != nil {
		// Deliberately not falling back to another port. The whole point of a
		// fixed one is that it matches what the operator registered with the
		// provider; quietly moving would produce a redirect_uri mismatch whose
		// cause is invisible from the browser error.
		return out, fmt.Errorf("could not open the OAuth callback port %d: %w\n"+
			"Something else is using it — check for another `karmax login` still waiting. "+
			"Free it, or set karmax.oauth_callback_port to a port you have registered with %s",
			port, err, m.Name)
	}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	redirect := fmt.Sprintf("http://%s:%d/callback", host, port)

	// Said out loud before the browser opens, because a redirect_uri mismatch is
	// reported by the provider as an opaque error page and the operator has no
	// other way to learn the exact string to register.
	p.Say("Callback URL (must be registered in your %s app): %s", m.Name, redirect)

	state := fmt.Sprintf("karmax-%d", time.Now().UnixNano())
	authURL := cfg.AuthURL + "?" + url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {strings.Join(cfg.Scopes, " ")},
		"state":         {state},
		"access_type":   {"offline"},
	}.Encode()

	codes := make(chan string, 1)
	errs := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errs <- fmt.Errorf("the provider returned a different state than we sent, so the response is not ours")
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errs <- fmt.Errorf("%s refused: %s", m.Name, e)
			return
		}
		fmt.Fprintf(w, "<html><body style='font-family:system-ui;padding:3rem'>"+
			"<h2>%s is connected.</h2><p>You can close this tab.</p></body></html>", m.Name)
		codes <- q.Get("code")
	})}
	for _, l := range listeners {
		go srv.Serve(l)
	}
	defer srv.Close()

	p.Say("Opening %s to authorise…", m.Name)
	if err := p.Open(authURL); err != nil {
		p.Say("Open this in a browser:\n  %s", authURL)
	}

	var code string
	select {
	case code = <-codes:
	case err := <-errs:
		return out, err
	case <-ctx.Done():
		return out, fmt.Errorf("gave up waiting for the browser")
	case <-time.After(5 * time.Minute):
		return out, fmt.Errorf("nothing came back from the browser within five minutes")
	}

	tok, err := exchange(ctx, cfg.TokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if err != nil {
		return out, err
	}
	out.Access, out.Refresh, out.ExpiresAt = tok.access, tok.refresh, tok.expires
	return out, nil
}

type tokenResponse struct {
	access  string
	refresh string
	expires *time.Time
}

// exchange swaps a code (or a refresh token) for an access token.
func exchange(ctx context.Context, tokenURL string, form url.Values) (tokenResponse, error) {
	var out tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, fmt.Errorf("the token endpoint answered %d: %.200s", resp.StatusCode, body)
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return out, fmt.Errorf("the token endpoint returned something that is not JSON: %.200s", body)
	}
	if parsed.AccessToken == "" {
		return out, fmt.Errorf("the token endpoint returned no access token")
	}
	out.access, out.refresh = parsed.AccessToken, parsed.RefreshToken
	if parsed.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
		out.expires = &t
	}
	return out, nil
}

// Refresh renews an OAuth access token that is close to expiring.
//
// Called before use rather than on a timer: a token that expired while KARMAX
// was asleep is the common case, and a timer would have missed it.
func (r *Registry) Refresh(ctx context.Context, id string) error {
	in, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("no integration called %q", id)
	}
	auth := in.Auth()
	if auth.Kind != connectorkit.AuthOAuth2 || auth.OAuth2 == nil {
		return nil
	}
	cred, err := r.creds.db.Credential(id)
	if err != nil || cred == nil || cred.RefreshToken == "" {
		return err
	}
	// Five minutes of headroom, so a call started now does not expire mid-flight.
	if cred.ExpiresAt != nil && time.Until(*cred.ExpiresAt) > 5*time.Minute {
		return nil
	}

	creds, _, _ := r.creds.Resolve(id)
	tok, err := exchange(ctx, auth.OAuth2.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cred.RefreshToken},
		"client_id":     {creds.Get(auth.OAuth2.ClientIDKey)},
		"client_secret": {creds.Get(auth.OAuth2.SecretKey)},
	})
	if err != nil {
		return fmt.Errorf("could not refresh %s: %w", id, err)
	}
	return r.creds.db.SaveTokens(id, tok.access, tok.refresh, tok.expires)
}

// OpenBrowser launches a URL, best-effort.
func OpenBrowser(target string) error {
	for _, candidate := range [][]string{
		{"xdg-open", target}, {"open", target}, {"wslview", target},
	} {
		if _, err := exec.LookPath(candidate[0]); err == nil {
			return exec.Command(candidate[0], candidate[1:]...).Start()
		}
	}
	return fmt.Errorf("no browser opener on this machine")
}

// The OAuth callback the browser is redirected back to.
//
// Fixed rather than ephemeral. An OS-assigned port changes every run, and a
// provider that matches redirect URLs exactly — LinkedIn does — can never have
// it registered, so the flow could not complete at all. 9095 sits beside the
// webhook (9090) and API (9091) ports as one of KARMAX's own.
const (
	defaultCallbackPort = 9095
	defaultCallbackHost = "127.0.0.1"
)

// callbackAddress resolves the host and port to listen on and to advertise.
//
// Both are overridable: some providers reject the bare-IP loopback form and
// want "localhost", and an operator may already be using 9095 for something.
// Whatever comes out here is both bound and sent as redirect_uri, so the two
// can never disagree.
func callbackAddress() (string, int) {
	host := strings.TrimSpace(os.Getenv("KARMAX_OAUTH_CALLBACK_HOST"))
	if host == "" {
		host = defaultCallbackHost
	}
	port := defaultCallbackPort
	if v := strings.TrimSpace(os.Getenv("KARMAX_OAUTH_CALLBACK_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	return host, port
}

// CallbackURL is the redirect URI this instance will use, for showing the
// operator what to register before they start a login they cannot finish.
func CallbackURL() string {
	host, port := callbackAddress()
	return fmt.Sprintf("http://%s:%d/callback", host, port)
}

// listenLoopback opens the callback port on every loopback address the
// advertised host can resolve to.
//
// net.Listen("tcp", "localhost:9095") binds ONE address — 127.0.0.1 on this
// machine — while a browser resolving the same name may reach for ::1 and get
// connection refused. The operator sees a browser that redirected to a page
// that never loads and a login that hangs until it times out, with nothing
// naming the cause. A literal IP is taken at its word; a hostname listens on
// both families so whichever one the browser picks is answered.
func listenLoopback(host string, port int) ([]net.Listener, error) {
	if ip := net.ParseIP(host); ip != nil {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		return []net.Listener{l}, nil
	}

	var out []net.Listener
	var firstErr error
	for _, addr := range []string{"127.0.0.1", "::1"} {
		l, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err != nil {
			// A machine with IPv6 disabled is normal, and so is one where only
			// v6 loopback exists. Only failing on BOTH is a real failure.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}
