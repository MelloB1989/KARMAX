package integration

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Where a secret comes from, and in what order.
//
// Both ways have to work. An operator running KARMAX from a config file in a
// container wants the key in karmax.yaml; one running it on a laptop wants to
// type `karmax login slack` and never see a token. Supporting only the first
// makes the laptop case a chore, and only the second makes the container case
// impossible.
//
// So: the store wins, then the file, then the environment. The store is first
// because it is the one an operator changed most recently and deliberately —
// `karmax login` is an action, a yaml key is a setting, and an action should
// not be silently overridden by a setting somebody wrote months ago.

// Source names where a credential came from, for the status view.
const (
	SourceStore  = "karmax login"
	SourceConfig = "karmax.yaml"
	SourceEnv    = "environment"
	SourceNone   = "not configured"
)

// ConfigLookup returns what karmax.yaml holds for an integration, already
// ${ENV}-interpolated. Injected so this package does not depend on the config
// package's shape.
type ConfigLookup func(id string) map[string]string

// Resolver finds the credentials for an integration.
type Resolver struct {
	db     *store.Store
	config ConfigLookup

	// fields records each integration's declared config, so the environment
	// fallback knows which names to look for rather than guessing.
	mu     sync.RWMutex
	fields map[string][]connectorkit.ConfigField
	envFor map[string]string
}

// NewResolver builds a resolver over the credential store and the config file.
func NewResolver(db *store.Store, config ConfigLookup) *Resolver {
	if config == nil {
		config = func(string) map[string]string { return nil }
	}
	return &Resolver{
		db: db, config: config,
		fields: map[string][]connectorkit.ConfigField{},
		envFor: map[string]string{},
	}
}

// Declare records what an integration needs, so Resolve knows what to look for.
func (r *Resolver) Declare(id string, fields []connectorkit.ConfigField) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fields[id] = fields
}

// Resolve returns an integration's credentials and where they came from.
//
// Merged rather than first-wins-wholesale: an operator can have the client id
// in karmax.yaml and the token from a login without one erasing the other. Only
// each individual VALUE is first-wins.
func (r *Resolver) Resolve(id string) (connectorkit.Credentials, string, error) {
	out := connectorkit.Credentials{Config: map[string]string{}}
	source := SourceNone

	// Lowest precedence first, so later writes overwrite.
	for key, val := range r.fromEnv(id) {
		if val != "" {
			out.Config[key] = val
			source = SourceEnv
		}
	}
	for key, val := range r.config(id) {
		if val != "" {
			out.Config[key] = val
			source = SourceConfig
		}
	}

	if r.db != nil {
		cred, err := r.db.Credential(id)
		if err != nil {
			return out, source, fmt.Errorf("integration: could not read stored credentials for %s: %w", id, err)
		}
		if cred != nil {
			for key, val := range cred.Config {
				if val != "" {
					out.Config[key] = val
					source = SourceStore
				}
			}
			if cred.AccessToken != "" {
				out.AccessToken = cred.AccessToken
				source = SourceStore
			}
			if cred.ExpiresAt != nil {
				out.ExpiresAt = *cred.ExpiresAt
			}
		}
	}
	return out, source, nil
}

// fromEnv reads the declared fields out of the process environment.
//
// The name is KARMAX_<ID>_<FIELD>, upper-cased with punctuation flattened, so
// slack's bot_token is KARMAX_SLACK_BOT_TOKEN. A bare <FIELD> name is also
// accepted for the variables that predate this — DISCORD_BOT_TOKEN and friends
// are already in people's .env files and breaking them to be tidy is not worth
// it.
func (r *Resolver) fromEnv(id string) map[string]string {
	r.mu.RLock()
	fields := r.fields[id]
	r.mu.RUnlock()

	out := map[string]string{}
	for _, f := range fields {
		if v := os.Getenv(EnvName(id, f.Key)); v != "" {
			out[f.Key] = v
			continue
		}
		if v := os.Getenv(flatten(f.Key)); v != "" {
			out[f.Key] = v
		}
	}
	return out
}

// EnvName is the environment variable an integration's field reads from.
func EnvName(id, field string) string {
	return "KARMAX_" + flatten(id) + "_" + flatten(field)
}

func flatten(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	return strings.NewReplacer(".", "_", "-", "_", ":", "_", " ", "_").Replace(s)
}

// Save stores what an interactive login obtained.
func (r *Resolver) Save(id string, config map[string]string, access, refresh string, expiresAt *time.Time) error {
	if r.db == nil {
		return fmt.Errorf("integration: no store to save credentials into")
	}
	existing, err := r.db.Credential(id)
	if err != nil {
		return err
	}
	merged := map[string]string{}
	if existing != nil {
		// Kept, so logging in again to refresh one field does not wipe the
		// others somebody set earlier.
		for k, v := range existing.Config {
			merged[k] = v
		}
	}
	for k, v := range config {
		if v != "" {
			merged[k] = v
		}
	}
	if access == "" && existing != nil {
		access = existing.AccessToken
	}
	if refresh == "" && existing != nil {
		refresh = existing.RefreshToken
	}
	return r.db.SaveCredential(store.Credential{
		Connector: id, Config: merged, AccessToken: access,
		RefreshToken: refresh, ExpiresAt: expiresAt, Enabled: true,
	})
}

// Forget removes a stored credential, which is how `karmax login --forget`
// hands control back to the config file.
func (r *Resolver) Forget(id string) error {
	if r.db == nil {
		return nil
	}
	return r.db.DeleteCredential(id)
}
