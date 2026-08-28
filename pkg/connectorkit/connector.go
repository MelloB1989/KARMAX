// Package connectorkit is the SDK for integrations: Gmail, GitHub, Slack,
// Notion, anything with an API.
//
// A connector is a KARMAX concept with a Go interface. MCP is one of several
// ways to implement one — used when a good server already exists and writing
// the integration is not worth it — and never the required path. The reasons
// are in §4.1 of the orchestration plan: auth belongs in KARMAX's credential
// store, inbound events are the point and MCP has no story for a server pushing
// into you, rate limits need provider-specific semantics, and a subprocess per
// integration is real memory on a small machine.
package connectorkit

import (
	"context"
	"encoding/json"
	"time"
)

// Connector is one integration.
type Connector interface {
	// Manifest describes it, including the capabilities it needs — which the
	// operator sees before it is enabled and the Broker enforces after.
	Manifest() Manifest

	// Auth says how credentials are obtained. KARMAX owns storage and refresh;
	// a connector never persists a token itself.
	Auth() AuthMethod

	// Tools are what the agent can call.
	Tools() []Tool

	// Sources turn things happening elsewhere into durable KARMAX events.
	Sources() []EventSource

	// Health reports whether the integration currently works, so a broken
	// credential is visible before someone depends on it.
	Health(ctx context.Context, c Credentials) error
}

// Manifest is what an operator reads before enabling a connector.
type Manifest struct {
	ID          string // stable, lowercase: "github"
	Name        string // "GitHub"
	Description string
	// Capabilities the connector needs, in Broker terms — "http:api.github.com",
	// "memory:write". Enabling it grants exactly these and nothing else.
	Capabilities []string
	// Config declares what the operator must supply, so setup can be checked
	// before anything runs rather than failing on the first call.
	Config []ConfigField

	// PerUser says this connector authenticates as an individual employee, not
	// as the install. The org supplies one OAuth app; each person authorises
	// their own account against it, and KARMAX picks the right one from who is
	// being acted for. Without this the whole company would read one inbox.
	PerUser bool
}

// ConfigField is one setting a connector needs.
type ConfigField struct {
	Key         string
	Description string
	Required    bool
	Secret      bool // never logged, never echoed back
	Default     string

	// Method scopes this field to one way of authenticating. Empty means it
	// applies whichever is chosen.
	//
	// Without this a connector's setup form is the UNION of every method's
	// fields — GitHub asking for a token AND an app id AND a private key AND
	// an installation id, of which any given operator needs about half, with
	// nothing saying which half.
	Method string

	// Help is where to find this value, in the provider's own words. A field
	// labelled "App ID" with no help is a question the operator has to take to
	// a search engine.
	Help string
}

// AuthOption is one way to authenticate a connector.
//
// Connectors usually support several, and they are not interchangeable: a
// personal access token acts as a human everywhere that human can reach, while
// a GitHub App acts only on the repositories it was installed on. Making the
// choice explicit is what lets the form ask for the right four fields instead
// of all nine.
type AuthOption struct {
	// ID is stored alongside the credentials, so the connector knows which way
	// it was set up rather than inferring it from which fields are non-empty.
	ID   string
	Name string
	// Summary is the one line that helps somebody choose.
	Summary string
	// Recommended marks the option most installs should take.
	Recommended bool
	// Steps are the instructions for this method specifically.
	Steps []SetupStep
}

// AuthChoices is implemented by connectors that support more than one way in.
//
// Optional: a connector with a single method needs no chooser, and gets the
// plain field list.
type AuthChoices interface {
	AuthOptions() []AuthOption
}

// AuthKind enumerates how credentials are obtained.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthAPIKey AuthKind = "apikey"
	AuthOAuth2 AuthKind = "oauth2"
	// AuthCLI delegates to a host binary that holds its own session — the wacli
	// and gws pattern. Legitimate and already proven.
	AuthCLI AuthKind = "cli"
)

// AuthMethod describes how a connector authenticates.
type AuthMethod struct {
	Kind AuthKind
	// APIKeyField names the config field holding the key, for AuthAPIKey.
	APIKeyField string
	// OAuth2 is set for AuthOAuth2.
	OAuth2 *OAuth2Config
	// CLIBinary is the host tool to resolve, for AuthCLI.
	CLIBinary string
}

// OAuth2Config is what KARMAX needs to run the flow and refresh afterwards.
type OAuth2Config struct {
	AuthURL     string
	TokenURL    string
	Scopes      []string
	ClientIDKey string // config field holding the client id
	SecretKey   string // config field holding the client secret
	// DeviceAuthURL enables the device flow, which is the only one that works
	// on a headless box with no browser and no public callback URL.
	DeviceAuthURL string
}

// Credentials are what KARMAX hands a connector at call time. A connector reads
// them and never writes them.
type Credentials struct {
	Config      map[string]string
	AccessToken string
	ExpiresAt   time.Time

	// Member is the org member these credentials belong to, for a PerUser
	// connector. Empty for install-wide credentials.
	Member string
	// Account is the external account they name, e.g. an email address, so a
	// tool can say whose mailbox it read rather than just reading it.
	Account string
}

// Get returns a config value.
func (c Credentials) Get(key string) string { return c.Config[key] }

// Tool is a callable the agent gains when a connector is enabled.
//
// Deliberately the same shape as KARMAX's internal tool interface, so a
// connector's tools and the built-in ones are indistinguishable to the agent —
// which is what stops "connector tools" becoming a second-class tier.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object.
	Parameters json.RawMessage
	Call       func(ctx context.Context, c Credentials, input map[string]any) (any, error)
}

// SourceKind says how a source learns that something happened.
type SourceKind string

const (
	// SourceWebhook receives a push. Preferred: no polling interval to tune and
	// no delay to explain.
	SourceWebhook SourceKind = "webhook"
	// SourcePoll asks on a schedule, for providers with no push.
	SourcePoll SourceKind = "poll"
)

// EventSource turns something happening elsewhere into a KARMAX event.
type EventSource struct {
	// ID is unique within the connector.
	ID   string
	Kind SourceKind
	// EventKind is the KARMAX event published, e.g. "github.pull_request".
	EventKind string

	// Path is the webhook route, for SourceWebhook. Mounted under the webhook
	// server, so it inherits the existing HMAC verification.
	Path string
	// Verify authenticates an inbound delivery. Returning false refuses it
	// before anything is parsed.
	Verify func(c Credentials, headers map[string]string, body []byte) bool
	// Decode turns a delivery into zero or more event payloads. Zero is normal:
	// most webhook deliveries are not interesting.
	Decode func(headers map[string]string, body []byte) ([]map[string]any, error)

	// Interval is the polling period, for SourcePoll.
	Interval time.Duration
	// Poll returns payloads for anything new since the cursor, and the cursor to
	// store for next time. KARMAX persists the cursor, so a restart does not
	// re-deliver or skip.
	Poll func(ctx context.Context, c Credentials, cursor string) (events []map[string]any, next string, err error)
}

// RateLimit is what a connector reports so KARMAX can back off correctly rather
// than guessing. Provider semantics differ enough that guessing is wrong.
type RateLimit struct {
	Remaining int
	ResetAt   time.Time
	// RetryAfter is set when the provider said so explicitly, which always wins
	// over a computed backoff.
	RetryAfter time.Duration
}

// SetupStep is one instruction in a connector's setup guide.
type SetupStep struct {
	Title string
	Body  string
	// Value is something to copy — a callback URL, a webhook secret.
	Value string
	// URL is a link to open. Some steps cannot be written as static text
	// because the address depends on what the operator just created: a GitHub
	// App's install page is /apps/<slug>/installations/new, and the slug does
	// not exist until the app does.
	URL string
	// Done reports whether this step already looks complete, so a half-finished
	// setup says which half. Nil means "cannot tell", which renders as neutral
	// rather than as a green tick nobody earned.
	Done *bool
}

// SetupGuide is implemented by connectors whose setup needs more than a list of
// config fields.
//
// The config fields say WHAT to paste. They cannot say that a GitHub App has to
// be installed on the repositories after it is created — a step with no field
// attached, which is exactly why people miss it and then wonder why a valid
// token sees no repos.
//
// Implementing this is optional; a connector that does not gets a generic guide
// built from its manifest.
type SetupGuide interface {
	// SetupSteps returns ordered instructions. It is given the credentials
	// saved so far so it can compute links and mark steps done, and the
	// callback URL this server is reachable at. It must not make network calls
	// that block: a setup screen has to render before anything is configured.
	SetupSteps(c Credentials, callbackURL string) []SetupStep
}

// SelfConfigured is implemented by connectors that can be set up somewhere
// other than KARMAX's credential store.
//
// The console decides "configured" by asking the store, which is right for
// almost everything. Slack is the exception on an install whose bot token has
// always lived in the daemon's .env: the store holds nothing, so the page said
// "not configured" beside a bot that was visibly answering in Slack. A status
// display that contradicts observable reality trains people to ignore it.
//
// Implementing this is optional and it must not make network calls — it
// answers "do I have what I need", not "does it work", which is the health
// check's job.
type SelfConfigured interface {
	Configured(c Credentials) bool
}
