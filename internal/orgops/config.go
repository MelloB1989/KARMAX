// Package orgops is KARMAX's engineering office: an agent that watches an
// organisation's repositories and production the way a staff engineer would —
// reviewing pull requests against the org's own rules, keeping repositories to
// a standard, keeping a knowledge store of every codebase, and answering
// production incidents by alerting the owner, fixing the fault and shipping it.
//
// Nothing about the organisation is compiled in. Who owns what, which chat a
// team lives in, what the rules are and how much the agent may do on its own
// are all configuration (karmax.yaml `org_ops:`) plus rule files written in
// plain Markdown (an org-wide file and each repository's own CLAUDE.md /
// AGENTS.md). Two installs of the same binary can run one org fully hands-off
// and another strictly approval-gated.
package orgops

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// Config is the `org_ops:` block of karmax.yaml.
type Config struct {
	// Enabled ships the office dark until an operator turns it on.
	Enabled bool `yaml:"enabled"`
	// Org is the GitHub organisation (or user) this office looks after.
	Org string `yaml:"org"`
	// Agent names the office in cases, audit rows and chat. Defaults to "orgops".
	Agent string `yaml:"agent"`

	GitHub GitHubConfig `yaml:"github"`

	// RulesFiles are org-wide rule documents (Markdown) every review and fix is
	// held to — the org's CLAUDE.md, in effect. Paths are local files.
	RulesFiles []string `yaml:"rules_files"`
	// RepoRulesFiles are read from each repository's default branch and added to
	// the org rules for work in that repository. Defaults to CLAUDE.md,
	// AGENTS.md, .karmax/rules.md and CONTRIBUTING.md.
	RepoRulesFiles []string `yaml:"repo_rules_files"`

	// Channels are chat destinations, written "<channel-id>:<target>" for a
	// named channel (e.g. "gchat:spaces/AAAA", "slack:#eng", "discord:123") or a
	// bare target for the default channel. Any channel KARMAX has registered
	// works — the office never names a chat platform.
	Channels ChannelsConfig `yaml:"channels"`

	// Policy is the default autonomy for every repository.
	Policy Policy `yaml:"policy"`
	// Repos override owners, channels and policy for repositories matching a
	// glob ("acme/payments-*"). The first match wins.
	Repos []RepoConfig `yaml:"repos"`

	Standards Standards       `yaml:"standards"`
	Threats   ThreatConfig    `yaml:"threats"`
	Probes    []ProbeConfig   `yaml:"probes"`
	Factory   FactoryConfig   `yaml:"factory"`
	Knowledge KnowledgeConfig `yaml:"knowledge"`
	Fixer     FixerConfig     `yaml:"fixer"`

	// AlertPath is where monitoring tools POST alerts on KARMAX's webhook
	// server (default /orgops/alert). Alertmanager, Grafana, Sentry and a plain
	// {"title","service","severity"} body are all understood.
	AlertPath string `yaml:"alert_path"`
	// AlertToken, when set, must arrive as the X-Karmax-Token header or a
	// ?token= query parameter. AlertTokenEnv names an env var to read it from.
	AlertToken    string `yaml:"alert_token"`
	AlertTokenEnv string `yaml:"alert_token_env"`
	// ListenChannels are the KARMAX channel ids whose messages are read for
	// office commands ("approve K7F2Q", "fix INC-…", "status"). Empty means
	// every channel except WhatsApp; "*" means every channel.
	ListenChannels []string `yaml:"listen_channels"`
	// ProbeEvery is how often probes run and queued merges are re-checked (default 30s).
	ProbeEvery string `yaml:"probe_every"`
}

// GitHubConfig points the office at a GitHub API. APIURL is configurable so a
// GitHub Enterprise install — or the bundled fake server — works unchanged.
type GitHubConfig struct {
	APIURL string `yaml:"api_url"`
	// Token is a token with repo/admin scope. TokenEnv names an environment
	// variable to read it from instead; GITHUB_TOKEN is the fallback.
	Token    string `yaml:"token"`
	TokenEnv string `yaml:"token_env"`
	// WebhookSecret verifies deliveries to the office's own GitHub webhook
	// (POST /orgops/github). WebhookSecretEnv names an env var holding it.
	// Point the org webhook here when the GitHub connector is not set up; when
	// it is, use the connector's /hooks/connectors/github instead — not both.
	WebhookSecret    string `yaml:"webhook_secret"`
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
	// BotLogin is the account the office acts as, so it never reviews its own PRs.
	BotLogin string `yaml:"bot_login"`
}

// ChannelsConfig routes each kind of message. Empty entries fall back to Default.
type ChannelsConfig struct {
	Default   string `yaml:"default"`
	Reviews   string `yaml:"reviews"`
	Incidents string `yaml:"incidents"`
	Security  string `yaml:"security"`
	Approvals string `yaml:"approvals"`
}

// RepoConfig narrows the defaults for some repositories.
type RepoConfig struct {
	Match   string   `yaml:"match"`
	Owners  []string `yaml:"owners"`
	Channel string   `yaml:"channel"`
	Policy  Policy   `yaml:"policy"`
	// Service names the production service this repository deploys, so a probe
	// or alert on that service can be traced back to the code.
	Service string `yaml:"service"`
}

// Standards are the deterministic checks every PR passes before a model reads it.
type Standards struct {
	// TitlePattern is a regexp every PR title must match (e.g. conventional commits).
	TitlePattern string `yaml:"title_pattern"`
	// MaxChangedLines flags a PR larger than a reviewer can hold in their head.
	MaxChangedLines int `yaml:"max_changed_lines"`
	// RequireDescription flags a PR with an empty body.
	RequireDescription bool `yaml:"require_description"`
	// RequiredFiles must exist in every repository (README.md, CODEOWNERS, ...).
	RequiredFiles []string `yaml:"required_files"`
	// ProtectedPaths are globs whose change always needs a human, whatever the policy.
	ProtectedPaths []string `yaml:"protected_paths"`
	// TestPaths are globs that count as tests; a code change with none is flagged.
	TestPaths []string `yaml:"test_paths"`
	// RequireTests flags source changes that come with no test changes.
	RequireTests bool `yaml:"require_tests"`
}

// ThreatConfig tunes the security scan run on every diff.
type ThreatConfig struct {
	// Disabled turns the built-in secret and risky-pattern scan off.
	Disabled bool `yaml:"disabled"`
	// ExtraPatterns are additional regexps, each a named rule.
	ExtraPatterns []PatternRule `yaml:"extra_patterns"`
	// Allow are regexps whose match is never reported (test fixtures, examples).
	Allow []string `yaml:"allow"`
}

// PatternRule is one named regexp with a severity.
type PatternRule struct {
	Name     string `yaml:"name"`
	Pattern  string `yaml:"pattern"`
	Severity string `yaml:"severity"`
}

// ProbeConfig is one production check the office keeps running.
type ProbeConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	// Method defaults to GET.
	Method string `yaml:"method"`
	// Expect is the healthy status code. Zero accepts any 2xx/3xx.
	Expect int `yaml:"expect"`
	// Timeout per request. Defaults to 5s.
	Timeout string `yaml:"timeout"`
	// Window is how many recent results the error rate is computed over (default 10).
	Window int `yaml:"window"`
	// MaxErrorRate opens an incident above this fraction of failures (default 0.5).
	MaxErrorRate float64 `yaml:"max_error_rate"`
	// MaxLatency opens an incident when the median latency exceeds it ("800ms").
	MaxLatency string `yaml:"max_latency"`
	// Repo is the repository that serves this endpoint, for the fix.
	Repo string `yaml:"repo"`
	// Owners are told first; they override the repo's owners.
	Owners []string `yaml:"owners"`
	// Headers sent with the request (auth for a health endpoint, say).
	Headers map[string]string `yaml:"headers"`
}

// FactoryConfig is how new repositories are made.
type FactoryConfig struct {
	// Templates maps a short name ("go-service") to a template repository
	// ("acme/template-go"). A request naming no template uses DefaultTemplate.
	Templates       map[string]string `yaml:"templates"`
	DefaultTemplate string            `yaml:"default_template"`
	// Private makes new repositories private (the default).
	Public bool `yaml:"public"`
	// Bootstrap files are written into every new repository, path -> content.
	// Content may use {{repo}}, {{org}}, {{owners}} and {{description}}.
	Bootstrap map[string]string `yaml:"bootstrap"`
	// ProtectDefaultBranch requires reviews and passing checks on the default branch.
	ProtectDefaultBranch bool `yaml:"protect_default_branch"`
	// RequiredChecks are status checks branch protection demands.
	RequiredChecks []string `yaml:"required_checks"`
}

// KnowledgeConfig is the org-wide knowledge store.
type KnowledgeConfig struct {
	// Files are read from each repository and summarised into memory.
	// Defaults to README.md, CLAUDE.md, AGENTS.md, docs/ARCHITECTURE.md.
	Files []string `yaml:"files"`
	// Every re-indexes on a schedule ("24h"). Empty means only on demand,
	// on repository creation, and when a PR merges.
	Every string `yaml:"every"`
	// MaxBytesPerFile caps what is read from one file (default 16KB).
	MaxBytesPerFile int `yaml:"max_bytes_per_file"`
}

// FixerConfig chooses who does the coding when the office decides to fix something.
type FixerConfig struct {
	// Dispatcher is "harness" (KARMAX's own Claude Code / Codex harness — the
	// default), "fleet" (fleetctl run on the standby fleet) or "sandbox" (a
	// container from the sandbox driver).
	Dispatcher string `yaml:"dispatcher"`
	// FleetctlPath defaults to "fleetctl" on PATH.
	FleetctlPath string `yaml:"fleetctl_path"`
	// Image is the sandbox image for Dispatcher "sandbox".
	Image string `yaml:"image"`
	// Timeout bounds one fix (default 45m).
	Timeout string `yaml:"timeout"`
	// WorkDir is the harness working directory, relative to the shared work root.
	WorkDir string `yaml:"workdir"`
}

// WithDefaults returns a copy with every unset field filled in.
func (c Config) WithDefaults() Config {
	if c.Agent == "" {
		c.Agent = "orgops"
	}
	if c.GitHub.APIURL == "" {
		c.GitHub.APIURL = "https://api.github.com"
	}
	if len(c.RepoRulesFiles) == 0 {
		c.RepoRulesFiles = []string{"CLAUDE.md", "AGENTS.md", ".karmax/rules.md", "CONTRIBUTING.md"}
	}
	if len(c.Knowledge.Files) == 0 {
		c.Knowledge.Files = []string{"README.md", "CLAUDE.md", "AGENTS.md", "docs/ARCHITECTURE.md"}
	}
	if c.Knowledge.MaxBytesPerFile <= 0 {
		c.Knowledge.MaxBytesPerFile = 16 << 10
	}
	if c.AlertPath == "" {
		c.AlertPath = "/orgops/alert"
	}
	if c.ProbeEvery == "" {
		c.ProbeEvery = "30s"
	}
	if c.Fixer.Dispatcher == "" {
		c.Fixer.Dispatcher = "harness"
	}
	if c.Fixer.FleetctlPath == "" {
		c.Fixer.FleetctlPath = "fleetctl"
	}
	c.Policy = c.Policy.withDefaults()
	return c
}

// Validate reports configuration that would fail at the worst moment instead.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Org) == "" {
		return fmt.Errorf("org_ops.org is required")
	}
	for _, p := range c.Probes {
		if p.Name == "" || p.URL == "" {
			return fmt.Errorf("org_ops.probes: every probe needs a name and a url")
		}
	}
	for _, mode := range c.Policy.modes() {
		if !mode.valid() {
			return fmt.Errorf("org_ops.policy: unknown mode %q (want off, notify, approve or auto)", mode)
		}
	}
	switch c.Fixer.Dispatcher {
	case "", "harness", "fleet", "sandbox":
	default:
		return fmt.Errorf("org_ops.fixer.dispatcher: unknown %q (want harness, fleet or sandbox)", c.Fixer.Dispatcher)
	}
	if _, err := compileStandards(c.Standards); err != nil {
		return err
	}
	if _, err := newScanner(c.Threats); err != nil {
		return err
	}
	return nil
}

// FullName qualifies a bare repository name with the org.
func (c Config) FullName(repo string) string {
	if strings.Contains(repo, "/") {
		return repo
	}
	return c.Org + "/" + repo
}

// repoFor returns the override block for a repository, or an empty one.
func (c Config) repoFor(fullName string) RepoConfig {
	for _, r := range c.Repos {
		if globMatch(r.Match, fullName) || globMatch(r.Match, shortName(fullName)) {
			return r
		}
	}
	return RepoConfig{}
}

// PolicyFor is the effective policy for a repository: its override over the default.
func (c Config) PolicyFor(fullName string) Policy {
	return c.repoFor(fullName).Policy.over(c.Policy)
}

// OwnersFor names who is told about a repository.
func (c Config) OwnersFor(fullName string) []string {
	return c.repoFor(fullName).Owners
}

// ChannelFor picks the chat destination for a kind of message about a repository.
func (c Config) ChannelFor(kind, fullName string) string {
	if r := c.repoFor(fullName); r.Channel != "" {
		return r.Channel
	}
	var ch string
	switch kind {
	case "review":
		ch = c.Channels.Reviews
	case "incident":
		ch = c.Channels.Incidents
	case "security":
		ch = c.Channels.Security
	case "approval":
		ch = c.Channels.Approvals
	}
	if ch == "" {
		ch = c.Channels.Default
	}
	return ch
}

// RepoForService maps a production service name back to its repository.
func (c Config) RepoForService(service string) string {
	for _, r := range c.Repos {
		if r.Service != "" && strings.EqualFold(r.Service, service) && !strings.ContainsAny(r.Match, "*?[") {
			return c.FullName(r.Match)
		}
	}
	return ""
}

func (p ProbeConfig) timeout() time.Duration {
	if d, err := time.ParseDuration(p.Timeout); err == nil && d > 0 {
		return d
	}
	return 5 * time.Second
}

func (f FixerConfig) timeout() time.Duration {
	if d, err := time.ParseDuration(f.Timeout); err == nil && d > 0 {
		return d
	}
	return 45 * time.Minute
}

func globMatch(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func shortName(fullName string) string {
	if i := strings.LastIndex(fullName, "/"); i >= 0 {
		return fullName[i+1:]
	}
	return fullName
}
