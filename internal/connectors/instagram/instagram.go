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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	if seed := strings.TrimSpace(cr.Get("totp_seed")); seed != "" {
		insta = goinsta.New(user, pass, seed)
	} else {
		insta = goinsta.New(user, pass)
	}
	if err := insta.Login(); err != nil {
		return nil, fmt.Errorf("instagram: sign-in failed (a challenge here usually means the account "+
			"was flagged — open the app and confirm it is you): %w", err)
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
