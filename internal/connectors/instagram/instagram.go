// Package instagram connects KARMAX to an Instagram account.
//
// READ THIS BEFORE ENABLING IT.
//
// Instagram has no API for a personal account. goinsta works by impersonating
// the mobile app against Instagram's private endpoints, which means:
//
//   - It is against Instagram's terms of use. Accounts get restricted, and
//     sometimes disabled, for automated access.
//   - It breaks when Instagram changes anything, with no notice and no
//     deprecation period.
//   - The login is a real password (and a real 2FA seed), not a scoped token
//     that can be revoked without changing the account's own credentials.
//
// So this connector is deliberately the most conservative in KARMAX: disabled
// unless explicitly enabled, read-only by default, and it says all of the above
// at `karmax login instagram` rather than burying it in a comment nobody reads.
// The session is cached so that enabling it costs one login rather than one per
// call — repeated logins are what gets an account flagged fastest.
package instagram

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Davincible/goinsta/v3"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Connector is one Instagram account.
type Connector struct {
	mu      sync.Mutex
	client  *goinsta.Instagram
	loginAt time.Time
}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   "instagram",
		Name: "Instagram",
		Description: "Read direct messages and your feed. UNOFFICIAL: this impersonates the mobile app " +
			"against Instagram's private API, which is against their terms and can get an account restricted.",
		Capabilities: []string{"http:i.instagram.com", "http:b.i.instagram.com"},
		Config: []connectorkit.ConfigField{
			{Key: "username", Description: "The account's username", Required: true},
			{Key: "password", Description: "The account's password — stored by KARMAX, not a revocable token", Required: true, Secret: true},
			{Key: "totp_seed", Description: "The 2FA seed, if the account has two-factor enabled", Secret: true},
		},
	}
}

// Auth is a password, which is the whole problem with this integration and is
// stated rather than dressed up as something safer.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: "password"}
}

func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	if !Enabled() {
		return fmt.Errorf("instagram is off — it uses an unofficial API that can get the account " +
			"restricted, so it stays off until KARMAX_ENABLE_INSTAGRAM=true is set")
	}
	client, err := c.connect(cr)
	if err != nil {
		return err
	}
	if client.Account == nil {
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
	return []connectorkit.Tool{
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
	}
}

// Sources is empty. Polling Instagram on a schedule is exactly the behaviour
// that gets an account flagged, so events are not offered at all.
func (c *Connector) Sources() []connectorkit.EventSource { return nil }

// connect signs in, reusing the session.
//
// Cached deliberately: repeated logins from one account are the strongest
// signal of automation Instagram has, so a session is kept for as long as it
// lasts rather than logging in per call.
func (c *Connector) connect(cr connectorkit.Credentials) (*goinsta.Instagram, error) {
	if !Enabled() {
		return nil, fmt.Errorf("instagram is off; set KARMAX_ENABLE_INSTAGRAM=true to turn it on")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil && time.Since(c.loginAt) < 12*time.Hour {
		return c.client, nil
	}

	user := strings.TrimSpace(cr.Get("username"))
	pass := strings.TrimSpace(cr.Get("password"))
	if user == "" || pass == "" {
		return nil, fmt.Errorf("instagram: a username and password are required")
	}

	// A cached session on disk survives restarts, which is one fewer login.
	if path := sessionPath(user); path != "" {
		if insta, err := goinsta.Import(path); err == nil && insta != nil {
			c.client, c.loginAt = insta, time.Now()
			return insta, nil
		}
	}

	var insta *goinsta.Instagram
	seed := strings.TrimSpace(cr.Get("totp_seed"))
	if seed != "" {
		normalized, err := normalizeTOTPSeed(seed)
		if err != nil {
			return nil, fmt.Errorf("instagram: the 2FA seed is not usable — %w. "+
				"Paste the base32 secret from the authenticator setup screen "+
				"(behind \"can't scan the QR code\"); spaces and case do not matter", err)
		}
		insta = goinsta.New(user, pass, normalized)
	} else {
		insta = goinsta.New(user, pass)
	}
	if err := insta.Login(); err != nil {
		return nil, signInError(err, seed != "")
	}
	if path := sessionPath(user); path != "" {
		_ = insta.Export(path)
	}
	c.client, c.loginAt = insta, time.Now()
	return insta, nil
}

func (c *Connector) inbox(ctx context.Context, cr connectorkit.Credentials, in map[string]any) (any, error) {
	client, err := c.connect(cr)
	if err != nil {
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

	inbox := client.Inbox
	if err := inbox.Sync(); err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, limit)
	for i, conv := range inbox.Conversations {
		if i >= limit {
			break
		}
		last := ""
		if len(conv.Items) > 0 {
			last = conv.Items[0].Text
		}
		items = append(items, map[string]any{
			"thread_id":   conv.ID,
			"title":       conv.Title,
			"last":        last,
			"last_active": time.UnixMicro(conv.LastActivityAt).Format(time.RFC3339),
		})
	}
	return map[string]any{"count": len(items), "threads": items}, nil
}

// sessionPath is where the cached login lives, 0600 like any other credential.
func sessionPath(user string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".karmax", "instagram")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return filepath.Join(dir, safeName(user)+".session")
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "account"
	}
	return b.String()
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
// goinsta hands the seed to base32.StdEncoding.DecodeString, which rejects
// spaces and REQUIRES padding to a multiple of eight. Instagram presents the
// secret lowercase in space-separated groups of four and never pads it — so
// copying it exactly as displayed fails, and the error names a byte offset
// rather than the space or the missing padding that caused it.
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
