// Package instagram connects KARMAX to an Instagram account.
//
// READ THIS BEFORE ENABLING IT.
//
// Instagram has no API for a personal account. This works by impersonating the
// mobile app against Instagram's private endpoints, which means:
//
//   - It is against Instagram's terms of use. Accounts get restricted, and
//     sometimes disabled, for automated access.
//   - It breaks when Instagram changes anything, with no notice and no
//     deprecation period.
//   - Signing in costs a real password (and a real 2FA seed), not a scoped
//     token that can be revoked without changing the account's own credentials
//     — unless you use the session route below, which costs neither.
//
// So this connector is deliberately the most conservative in KARMAX: disabled
// unless explicitly enabled, read-only, and it says all of the above at
// `karmax login instagram` rather than burying it in a comment nobody reads.
//
// # Why there is a Python process behind this
//
// The client that actually keeps up with Instagram's private API is instagrapi,
// and it is Python. Rather than reimplement years of other people's
// reverse-engineering in Go and fall behind it immediately, KARMAX runs it as a
// child process and talks to it over a pipe — see helper.go. The environment
// for it is fetched on first use, not shipped, so an install that never enables
// this connector never pays for it.
//
// # The session route
//
// `sessionid` is the cookie from a browser the operator is already signed into.
// It is the preferred way in: no password is stored, no login flow runs, and
// the account is not asked to authenticate a second time — repeated logins are
// the strongest automation signal Instagram has. It is also the only route that
// matches how somebody actually connects Instagram in the LYZN desktop app,
// which signs them in through the shared browser and never collects a password.
package instagram

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Connector is one Instagram account.
type Connector struct {
	mu sync.Mutex
	h  *helper

	// ledger records who has already been contacted. Nil means sending is
	// unavailable, which is the default.
	ledger Ledger

	// allowed is the passthrough's read allowlist, fetched from the helper once
	// and kept. Cached so that refusing a write costs nothing and — more to the
	// point — can be refused before asking anybody to sign in: "that is
	// read-only" teaches the boundary, where "you are not signed in" sends the
	// caller off to fix the wrong thing.
	allowedOnce sync.Once
	allowedSet  map[string]bool
}

func New() *Connector { return &Connector{h: &helper{}} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   "instagram",
		Name: "Instagram",
		Description: "Read direct messages and your feed. UNOFFICIAL: this impersonates the mobile app " +
			"against Instagram's private API, which is against their terms and can get an account restricted.",
		Capabilities: []string{"http:i.instagram.com", "http:b.i.instagram.com"},
		Config: []connectorkit.ConfigField{
			{Key: "username", Description: "The account's username", Required: true},
			{Key: "sessionid", Description: "The sessionid cookie from a browser you are already " +
				"signed into. Preferred: no password is stored and no second login happens.", Secret: true},
			{Key: "password", Description: "The account's password — only needed without a sessionid, " +
				"and stored by KARMAX rather than being a revocable token", Secret: true},
			{Key: "totp_seed", Description: "The 2FA seed, if the account has two-factor enabled " +
				"and you are signing in with a password", Secret: true},
		},
	}
}

// Auth is a password, which is the whole problem with this integration and is
// stated rather than dressed up as something safer. A sessionid is no better in
// kind — it is a bearer credential for the whole account — but it is at least
// one the operator can revoke by logging out, without changing their password.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "password"}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	if !Enabled() {
		return fmt.Errorf("instagram is off — it uses an unofficial API that can get the account " +
			"restricted, so it stays off until KARMAX_ENABLE_INSTAGRAM=true is set")
	}
	who, err := c.ensure(ctx, cr)
	if err != nil {
		return err
	}
	if who == "" {
		return fmt.Errorf("instagram: signed in but the account did not come back")
	}
	return nil
}

// Enabled reports whether the operator has explicitly turned this on.
//
// Opt-in rather than opt-out because the cost of it running unnoticed is
// somebody's personal account being restricted, which is not recoverable by
// changing a setting afterwards.
func Enabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("KARMAX_ENABLE_INSTAGRAM")), "true")
}

func (c *Connector) Tools() []connectorkit.Tool {
	out := []connectorkit.Tool{
		{
			Name: "instagram.inbox",
			Description: "Read recent Instagram direct message threads. Read-only: KARMAX does not send " +
				"on Instagram, because automated sending is what gets accounts banned.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{"limit":{"type":"integer","description":"Maximum threads (default 10, max 30)."}}
			}`),
			Call: c.inbox,
		},
		{
			Name: "instagram.reads",
			Description: "List every Instagram read instagram.call will accept, with each one's " +
				"arguments. Call this first rather than guessing a method name.",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
			Call:       c.reads,
		},
		{
			Name: "instagram.call",
			Description: "Call one Instagram read directly — any method listed by instagram.reads, " +
				"with its own arguments. Use this for anything the other tools do not cover: " +
				"a post's comments, who someone follows, a user's recent media. " +
				"READ-ONLY: sending, commenting, liking and following are not reachable here, " +
				"because automated writing is what gets accounts restricted.",
			Parameters: json.RawMessage(`{
				"type":"object",
				"properties":{
					"method":{"type":"string","description":"An instagrapi read, e.g. \"media_comments\". instagram.reads lists them."},
					"args":{"type":"object","description":"Named arguments for that method, e.g. {\"media_id\":\"123\",\"amount\":0}."}
				},
				"required":["method"]
			}`),
			Call: c.passthrough,
		},
	}

	// Sending is a separate decision from reading, and stays off even when the
	// connector itself is on. Reading someone's inbox and messaging their
	// followers have different consequences; this connector has always refused
	// the second, and that refusal remains the default.
	if SendingEnabled() && c.ledger != nil {
		out = append(out,
			connectorkit.Tool{
				Name: "instagram.send_dm",
				Description: "Send ONE Instagram direct message. Pacing, a per-campaign cap and a " +
					"ledger that refuses to contact the same person twice are enforced inside this " +
					"tool — call it once per recipient rather than building a loop around something " +
					"lower-level. The first time Instagram objects, the whole campaign stops and " +
					"does not resume.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"campaign":{"type":"string","description":"Names this run, e.g. the post you are following up on. The cap and the ledger are counted per campaign."},
						"user_id":{"type":"string","description":"The recipient's numeric Instagram id."},
						"text":{"type":"string","description":"The message. Write a different one for each person — the same sentence sent to a batch is what reads as spam."}
					},
					"required":["campaign","user_id","text"]
				}`),
				Call: c.sendDM,
			},
			connectorkit.Tool{
				Name: "instagram.reply_comment",
				Description: "Post ONE reply to an Instagram comment. Same pacing, cap and ledger as " +
					"instagram.send_dm, keyed by comment rather than by person — so somebody who " +
					"commented three times can be answered three times.",
				Parameters: json.RawMessage(`{
					"type":"object",
					"properties":{
						"campaign":{"type":"string","description":"Names this run. Use a different campaign from the DM run: the two ledgers answer different questions."},
						"media_id":{"type":"string","description":"The post being commented on."},
						"comment_id":{"type":"string","description":"The comment being replied to. Omit to comment on the post itself."},
						"text":{"type":"string","description":"The reply. It is public — say something that reads well under somebody else's comment."}
					},
					"required":["campaign","media_id","text"]
				}`),
				Call: c.replyComment,
			},
		)
	}
	return out
}

// Sources is empty. Polling Instagram on a schedule is exactly the behaviour
// that gets an account flagged, so events are not offered at all.
func (c *Connector) Sources() []connectorkit.EventSource { return nil }

// Close stops the helper. The signed-in session goes with it.
func (c *Connector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.h.stop()
}

// ensure signs in if the helper is not already holding a session, and returns
// whose account it is.
//
// The helper is asked rather than remembered here: it may have been restarted
// underneath us, and a cached "yes, signed in" on this side would then send
// every call into a process that is not. Asking costs one local pipe round
// trip and no Instagram traffic at all.
func (c *Connector) ensure(ctx context.Context, cr connectorkit.Credentials) (string, error) {
	if !Enabled() {
		return "", fmt.Errorf("instagram is off; set KARMAX_ENABLE_INSTAGRAM=true to turn it on")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var ping struct {
		LoggedIn bool `json:"logged_in"`
	}
	if err := c.h.call(ctx, "ping", nil, &ping); err != nil {
		return "", err
	}
	if ping.LoggedIn {
		var acct struct {
			Username string `json:"username"`
		}
		if err := c.h.call(ctx, "account", nil, &acct); err == nil {
			return acct.Username, nil
		}
		// Falling through to a fresh login: the helper thinks it is signed in
		// but cannot say as whom, which is the shape of a session Instagram
		// has since invalidated.
	}

	user := strings.TrimSpace(cr.Get("username"))
	session := strings.TrimSpace(cr.Get("sessionid"))
	pass := strings.TrimSpace(cr.Get("password"))
	seed := strings.TrimSpace(cr.Get("totp_seed"))

	if session == "" && (user == "" || pass == "") {
		return "", fmt.Errorf("instagram: needs either a sessionid from a browser you are " +
			"signed into, or a username and password")
	}

	params := map[string]any{"username": user}
	if session != "" {
		params["sessionid"] = session
	} else {
		params["password"] = pass
		if seed != "" {
			normalized, err := normalizeTOTPSeed(seed)
			if err != nil {
				return "", fmt.Errorf("instagram: the 2FA seed is not usable — %w. "+
					"Paste the base32 secret from the authenticator setup screen "+
					"(behind \"can't scan the QR code\"); spaces and case do not matter", err)
			}
			params["totp_seed"] = normalized
		}
	}

	var out struct {
		Username string `json:"username"`
	}
	if err := c.h.call(ctx, "login", params, &out); err != nil {
		return "", loginFailed(err, seed != "", session != "")
	}
	return out.Username, nil
}

func (c *Connector) inbox(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	if _, err := c.ensure(ctx, cr); err != nil {
		return nil, err
	}
	limit := 10
	switch n := in["limit"].(type) {
	case float64:
		limit = int(n)
	case int:
		limit = n
	}
	if limit <= 0 || limit > 30 {
		limit = 10
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	var out any
	if err := c.h.call(ctx, "inbox", map[string]any{"limit": limit}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// reads lists what the passthrough accepts. No sign-in needed: it is a
// question about this build, not about anybody's account.
func (c *Connector) reads(ctx context.Context, _ connectorkit.Credentials, _ map[string]any) (any, error) {
	if !Enabled() {
		return nil, fmt.Errorf("instagram is off; set KARMAX_ENABLE_INSTAGRAM=true to turn it on")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out any
	if err := c.h.call(ctx, "reads", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// readable returns the passthrough's allowlist, asking the helper once.
//
// A failure here is not fatal: the helper enforces the allowlist regardless, so
// an empty answer costs a worse error message and nothing else. That is why the
// caller treats an error as "no opinion" rather than as a refusal.
func (c *Connector) readable(ctx context.Context) (map[string]bool, error) {
	var err error
	c.allowedOnce.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		var out struct {
			Reads []struct {
				Method string `json:"method"`
			} `json:"reads"`
		}
		if err = c.h.call(ctx, "reads", nil, &out); err != nil {
			return
		}
		set := make(map[string]bool, len(out.Reads))
		for _, r := range out.Reads {
			set[r.Method] = true
		}
		c.allowedSet = set
	})
	return c.allowedSet, err
}

// passthrough calls one allowlisted instagrapi read.
//
// The allowlist lives in the helper rather than here, deliberately: it has to
// be enforced next to the call it is protecting, so a future caller that
// reaches the helper by another path cannot route around it.
func (c *Connector) passthrough(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	method, _ := in["method"].(string)
	if strings.TrimSpace(method) == "" {
		return nil, fmt.Errorf("instagram: name a method — instagram.reads lists them")
	}
	args, _ := in["args"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}

	// Before sign-in, so a request to write is answered with what it actually
	// ran into. The helper checks this again next to the call itself; this copy
	// is for the error message, not for the enforcement.
	if known, err := c.readable(ctx); err == nil && len(known) > 0 && !known[method] {
		return nil, fmt.Errorf("instagram: %q is not an allowed read. This passthrough is "+
			"read-only by design — sending, commenting, liking and following are not "+
			"reachable through it. instagram.reads lists what is", method)
	}

	if _, err := c.ensure(ctx, cr); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	var out any
	if err := c.h.call(ctx, "call", map[string]any{"method": method, "args": args}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// loginFailed turns a helper failure into something the operator can act on.
//
// The helper reports instagrapi's own exception name, so the common cases are
// named rather than guessed at from message text — but signInError still does
// the text reading underneath, because the 2FA advice it carries is the part
// people actually need and it is worth keeping however the error arrived.
func loginFailed(err error, hadSeed, hadSession bool) error {
	var he *Error
	if errors.As(err, &he) {
		switch {
		case he.Type == "LoginRequired" && hadSession:
			return fmt.Errorf("instagram: that sessionid is no longer valid — it expires when the " +
				"account signs out anywhere. Sign in again in the browser and copy a fresh one")
		case he.HardStop:
			return fmt.Errorf("instagram: %s — Instagram is refusing this account for now, which "+
				"usually means it was flagged. Open the app, confirm it is you, and leave it alone "+
				"for a while before trying again: %s", he.Type, he.Message)
		}
	}
	return signInError(err, hadSeed)
}

// signInError says what actually went wrong, in terms the operator can act on.
//
// The one blanket hint this used to give — "a challenge usually means the
// account was flagged" — sent people to open the app and confirm their identity
// when the real answer was that two-factor is on and no seed was supplied. The
// underlying library reports that as "illegal base32 data at input byte 0",
// which names the symptom (it tried to decode an empty seed) and not the cause.
func signInError(err error, hadSeed bool) error {
	low := strings.ToLower(err.Error())
	twoFactor := strings.Contains(low, "2fa") ||
		strings.Contains(low, "otp") ||
		strings.Contains(low, "two-factor") ||
		strings.Contains(low, "base32")

	switch {
	case twoFactor && !hadSeed:
		return fmt.Errorf("instagram: this account has two-factor authentication enabled, "+
			"so signing in needs its TOTP seed — run `karmax login instagram` again and paste it "+
			"at the totp_seed prompt. That is the base32 SECRET shown when you set up the "+
			"authenticator app (often behind \"can't scan the QR code\"), not a six-digit code. "+
			"Underlying error: %w", err)

	case twoFactor:
		return fmt.Errorf("instagram: the TOTP seed was not usable — it must be the base32 secret "+
			"from the authenticator setup screen, with no spaces, not a six-digit code and not a "+
			"backup code. Underlying error: %w", err)

	case strings.Contains(low, "challenge"):
		return fmt.Errorf("instagram: sign-in hit a verification challenge, which usually means the "+
			"account was flagged — open the app and confirm it is you, then try again: %w", err)

	case strings.Contains(low, "password") || strings.Contains(low, "credential"):
		return fmt.Errorf("instagram: the username or password was rejected: %w", err)
	}
	return fmt.Errorf("instagram: sign-in failed: %w", err)
}

// normalizeTOTPSeed turns what Instagram shows you into what the decoder wants.
//
// The seed goes to a base32 decoder that rejects spaces and REQUIRES padding to
// a multiple of eight. Instagram presents the secret lowercase in
// space-separated groups of four and never pads it — so copying it exactly as
// displayed fails, and the error names a byte offset rather than the space or
// the missing padding that caused it.
//
// Case is already handled upstream; whitespace, separators and padding are not.
func normalizeTOTPSeed(seed string) (string, error) {
	var b strings.Builder
	for _, r := range seed {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '-' || r == '_':
			continue // grouping, not content
		case r == '=':
			continue // re-added below, so a partly-padded seed still works
		default:
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	cleaned := b.String()
	if cleaned == "" {
		return "", fmt.Errorf("the seed is empty once spaces are removed")
	}

	// Named before decoding, because "illegal base32 data at input byte 7" does
	// not tell anyone that their secret contains a 0 or a 1.
	for i, r := range cleaned {
		if (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7') {
			continue
		}
		return "", fmt.Errorf("character %q (position %d) is not valid base32 — a seed uses only "+
			"letters A-Z and digits 2-7, so 0, 1, 8 and 9 never appear in one", r, i+1)
	}

	// StdEncoding demands padding; Instagram never shows any.
	if pad := len(cleaned) % 8; pad != 0 {
		cleaned += strings.Repeat("=", 8-pad)
	}
	if _, err := base32.StdEncoding.DecodeString(cleaned); err != nil {
		return "", fmt.Errorf("the seed is not decodable base32: %w", err)
	}
	return cleaned, nil
}
