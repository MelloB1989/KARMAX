// Package config reads fleet.yaml: which agents exist, where they run, whose
// subscription each one uses, and every threshold the reconciler acts on.
//
// It never holds a secret. An agent names the env var its token is in
// (token_env); the value lives only in the env file and the 0600 per-agent
// file rendered from it.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is fleet.yaml.
type Config struct {
	// Home is the HOME every container runs with — the operator's own, so
	// that Code and every absolute path under it is the same everywhere.
	Home string `yaml:"home"`
	// Code is the shared checkout root, mounted at this same path in every
	// container on every host.
	Code     string `yaml:"code"`
	Image    string `yaml:"image"`
	UID      int    `yaml:"uid"`
	GID      int    `yaml:"gid"`
	EnvFile  string `yaml:"env_file"`
	StateDir string `yaml:"state_dir"`

	// Listen is fleetd's local API. RelayListen, when set, is the second
	// listener PC2's agents reach for `fleetctl tell`; it requires the bearer
	// token in TokenEnv. OTLPListen receives the agents' telemetry.
	Listen      string `yaml:"listen"`
	RelayListen string `yaml:"relay_listen"`
	OTLPListen  string `yaml:"otlp_listen"`
	// TokenEnv names the full-scope API token (you, the orchestrator);
	// RelayTokenEnv the one agents get, good for tell and status only.
	TokenEnv      string `yaml:"token_env"`
	RelayTokenEnv string `yaml:"relay_token_env"`
	// SockDir is where Claude Code puts its messaging sockets inside the
	// containers (XDG_RUNTIME_DIR); one volume shares it per host. Spike S1
	// confirms the path.
	SockDir string `yaml:"sock_dir"`

	Hosts        map[string]Host  `yaml:"hosts"`
	Orchestrator Orchestrator     `yaml:"orchestrator"`
	Agents       map[string]Agent `yaml:"agents"`
	Karmax       Karmax           `yaml:"karmax"`
	Build        Build            `yaml:"build"`
	GitHub       GitHub           `yaml:"github"`
	Git          Git              `yaml:"git"`
	Thresholds   Thresholds       `yaml:"thresholds"`
	// Console is the fleet console on one host; nil means none.
	Console *Console `yaml:"console"`
}

// Console is fleet-console: the live panel on one fleet host. fleetctl
// render writes its console.yaml and its fleetd token from this.
type Console struct {
	// Host is the fleet host that runs it; its agents get live views.
	Host string `yaml:"host"`
	// Listen is its address on that host, loopback only.
	Listen string `yaml:"listen"`
	// FleetdURL is how it reaches fleetd: the relay listener, from there.
	FleetdURL string `yaml:"fleetd_url"`
	// TokenEnv names its console-scope token for fleetd, in env_file.
	TokenEnv string `yaml:"token_env"`
	// Repos are the git checkouts it watches, as paths on its host.
	Repos []string `yaml:"repos"`
	// Dir is its config directory on its host.
	Dir string `yaml:"dir"`
}

// Host is one machine running agents, reached through an exec prefix.
type Host struct {
	// Exec is the docker CLI prefix that reaches this host's containers:
	// [docker] locally, [docker, --context, pc2] for a remote engine.
	Exec []string `yaml:"exec"`
	// FleetdURL is how agents on this host reach fleetd, for `fleetctl tell`.
	// Empty on the host fleetd runs on.
	FleetdURL string `yaml:"fleetd_url"`
	// OTLPURL is where this host's agents send telemetry. Empty disables it.
	OTLPURL string `yaml:"otlp_url"`
	// KarmaxURL is how this host's containers reach the KARMAX API.
	KarmaxURL string `yaml:"karmax_url"`
}

// Orchestrator is KARMAX's own harness session, run inside karmax-brain —
// or, with Native, on fleetd's host itself under that host's Claude login.
type Orchestrator struct {
	Name      string `yaml:"name"`
	Host      string `yaml:"host"`
	Container string `yaml:"container"`
	TokenEnv  string `yaml:"token_env"`
	// Native runs the orchestrator outside Docker on its host, which must be
	// fleetd's own: no karmax-brain, no token, and relays to it run there.
	Native bool `yaml:"native"`
	// Report is how the orchestrator tells the operator what came of a task,
	// e.g. a WhatsApp send command. A turn an agent's report starts has no
	// caller, so its reply text reaches nobody unless it is sent somewhere.
	Report string `yaml:"report"`
	// KarmaxAgent is the KARMAX agent id whose harness session is the
	// orchestrator (karmax.yaml agents[].id). When set, fleetctl render
	// writes the fleet's paragraph into that session's .claude/CLAUDE.md.
	KarmaxAgent string `yaml:"karmax_agent"`
}

// Agent is one standby coding agent, bound to one subscription.
type Agent struct {
	Host      string   `yaml:"host"`
	TokenEnv  string   `yaml:"token_env"`
	Models    []string `yaml:"models"`
	Model     string   `yaml:"model"` // the model its session starts on; empty = the CLI default
	Account   string   `yaml:"account"`
	Container string   `yaml:"container"`
	CPUs      string   `yaml:"cpus"`
	Memory    string   `yaml:"memory"`
}

// Build is a Docker engine agents send heavy builds to, over SSH. Docker
// access there is root on that machine; give it a user of its own.
type Build struct {
	// DockerHost is DOCKER_HOST in every agent, e.g. ssh://fleetbuild@kali.
	DockerHost string `yaml:"docker_host"`
	// KeyEnv names the env var holding the base64 private key for it.
	KeyEnv string `yaml:"key_env"`
}

// GitHub is how agents reach GitHub: one token for every agent, as GH_TOKEN
// (gh, and git through gh's credential helper in the image).
type GitHub struct {
	TokenEnv string `yaml:"token_env"`
	// SSHAliases are the operator's ~/.ssh/config Host names for github.com,
	// so a remote cloned as git@<alias>:owner/repo is rewritten to HTTPS in
	// the agents, which hold no SSH keys.
	SSHAliases []string `yaml:"ssh_aliases"`
}

// Git is the identity agents commit under; each agent's name is appended,
// e.g. "Example coding agent (agent-01)".
type Git struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Karmax is where fleetd tells KARMAX things.
type Karmax struct {
	// WebhookURL receives state changes as a POST; a webhooks.routes entry
	// in karmax.yaml turns it into a bus event. Empty disables the push.
	WebhookURL string `yaml:"webhook_url"`
	// WebhookSecretEnv names the env var holding the route's HMAC secret.
	WebhookSecretEnv string `yaml:"webhook_secret_env"`
	// Dashboard is the id of the Fleet dashboard whose data files fleetd
	// rewrites each tick. Empty disables it.
	Dashboard string `yaml:"dashboard"`
	// DataDir is KARMAX's data directory, where dashboards live.
	DataDir string `yaml:"data_dir"`
	// Notify pushes the few alerts worth a phone notification through
	// `karmax notify`. On unless set false.
	Notify *bool `yaml:"notify"`
	// Bin is the karmax CLI. Default "karmax".
	Bin string `yaml:"bin"`
	// APIURL is KARMAX's API, read for the orchestrator's own quota.
	// Default http://127.0.0.1:9091.
	APIURL string `yaml:"api_url"`
}

// Thresholds is everything the reconciler decides on.
type Thresholds struct {
	Tick            time.Duration `yaml:"tick"`
	RestartFailures int           `yaml:"restart_failures"`
	RestartWindow   time.Duration `yaml:"restart_window"`
	StandbyIdle     time.Duration `yaml:"standby_idle"`
	// StandbyMinTranscript is the size below which a standby session's
	// transcript is too trivial to bother archiving and rotating.
	StandbyMinTranscript int64         `yaml:"standby_min_transcript"`
	WaitingNudge         time.Duration `yaml:"waiting_nudge"`
	WaitingArchive       time.Duration `yaml:"waiting_archive"`
	CompactBytes         int64         `yaml:"compact_bytes"`
	Low5h                float64       `yaml:"low_5h"`
	Reserved7d           float64       `yaml:"reserved_7d"`
	CoolingDefault       time.Duration `yaml:"cooling_default"`
	// Retention.
	EventsRaw   time.Duration `yaml:"events_raw"`
	QuotaRaw    time.Duration `yaml:"quota_raw"`
	RelayText   time.Duration `yaml:"relay_text"`
	HostSamples time.Duration `yaml:"host_samples"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates a fleet.yaml.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse validates a fleet.yaml body and fills its defaults.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("fleet.yaml: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("fleet.yaml: %w", err)
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Home == "" {
		c.Home, _ = os.UserHomeDir()
	}
	c.Home = expand(c.Home, c.Home)
	or := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
		*p = expand(*p, c.Home)
	}
	if k := c.Console; k != nil {
		// Paths on the console's host: ~ stays, for that host to expand.
		if k.Listen == "" {
			k.Listen = "127.0.0.1:7880"
		}
		if k.Dir == "" {
			k.Dir = "~/.config/fleet-console"
		}
	}
	or(&c.Code, filepath.Join(c.Home, "code"))
	or(&c.EnvFile, filepath.Join(c.Home, ".karmax", ".env"))
	or(&c.StateDir, filepath.Join(c.Home, ".karmax", "fleet"))
	or(&c.Karmax.DataDir, filepath.Join(c.Home, ".karmax"))
	if c.Image == "" {
		c.Image = "karmax-fleet:latest"
	}
	if c.UID == 0 {
		c.UID = os.Getuid()
	}
	if c.GID == 0 {
		c.GID = os.Getgid()
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7878"
	}
	if c.TokenEnv == "" {
		c.TokenEnv = "FLEET_TOKEN"
	}
	if c.RelayTokenEnv == "" {
		c.RelayTokenEnv = "FLEET_RELAY_TOKEN"
	}
	if c.SockDir == "" {
		c.SockDir = "/run/fleet"
	}
	if c.Karmax.Bin == "" {
		c.Karmax.Bin = "karmax"
	}
	if c.Karmax.APIURL == "" {
		c.Karmax.APIURL = "http://127.0.0.1:9091"
	}
	if c.Orchestrator.Name == "" {
		c.Orchestrator.Name = "karmax"
	}
	if c.Orchestrator.Container == "" {
		c.Orchestrator.Container = "karmax-brain"
	}
	for name, a := range c.Agents {
		if a.Container == "" {
			a.Container = name
		}
		if len(a.Models) == 0 {
			a.Models = []string{"sonnet"}
		}
		if a.Account == "" {
			a.Account = name
		}
		c.Agents[name] = a
	}

	th := &c.Thresholds
	dur := func(p *time.Duration, v time.Duration) {
		if *p <= 0 {
			*p = v
		}
	}
	dur(&th.Tick, 30*time.Second)
	dur(&th.RestartWindow, 10*time.Minute)
	dur(&th.StandbyIdle, 12*time.Hour)
	dur(&th.WaitingNudge, 2*time.Hour)
	dur(&th.WaitingArchive, 24*time.Hour)
	dur(&th.CoolingDefault, time.Hour)
	dur(&th.EventsRaw, 90*24*time.Hour)
	dur(&th.QuotaRaw, 7*24*time.Hour)
	dur(&th.RelayText, 30*24*time.Hour)
	dur(&th.HostSamples, 7*24*time.Hour)
	if th.RestartFailures <= 0 {
		th.RestartFailures = 3
	}
	if th.StandbyMinTranscript <= 0 {
		th.StandbyMinTranscript = 32 << 10
	}
	if th.CompactBytes <= 0 {
		th.CompactBytes = 8 << 20
	}
	if th.Low5h == 0 {
		th.Low5h = 80
	}
	if th.Reserved7d == 0 {
		th.Reserved7d = 90
	}
}

func (c *Config) validate() error {
	if len(c.Hosts) == 0 {
		return fmt.Errorf("no hosts")
	}
	for name, h := range c.Hosts {
		if len(h.Exec) == 0 {
			return fmt.Errorf("host %q: exec is empty (want e.g. [docker] or [docker, --context, %s])", name, name)
		}
	}
	if _, ok := c.Hosts[c.Orchestrator.Host]; !ok {
		return fmt.Errorf("orchestrator: unknown host %q", c.Orchestrator.Host)
	}
	if !c.Orchestrator.Native {
		if err := checkTokenEnv("orchestrator", c.Orchestrator.TokenEnv); err != nil {
			return err
		}
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("no agents")
	}
	seenToken := map[string]string{}
	if !c.Orchestrator.Native {
		seenToken[c.Orchestrator.TokenEnv] = "orchestrator"
	}
	for _, name := range c.AgentNames() {
		a := c.Agents[name]
		if !namePattern.MatchString(name) {
			return fmt.Errorf("agent %q: names are lowercase letters, digits and dashes", name)
		}
		if name == c.Orchestrator.Name {
			return fmt.Errorf("agent %q: same name as the orchestrator", name)
		}
		if _, ok := c.Hosts[a.Host]; !ok {
			return fmt.Errorf("agent %q: unknown host %q", name, a.Host)
		}
		if err := checkTokenEnv("agent "+name, a.TokenEnv); err != nil {
			return err
		}
		if other, dup := seenToken[a.TokenEnv]; dup {
			return fmt.Errorf("agent %q: token_env %s is already %s's — one subscription per agent", name, a.TokenEnv, other)
		}
		seenToken[a.TokenEnv] = name
	}
	if c.GitHub.TokenEnv != "" {
		if err := checkTokenEnv("github", c.GitHub.TokenEnv); err != nil {
			return err
		}
	}
	for _, a := range c.GitHub.SSHAliases {
		if !hostAlias.MatchString(a) {
			return fmt.Errorf("github: ssh_aliases: %q is not a host name", a)
		}
	}
	if c.Build.DockerHost != "" {
		if err := checkTokenEnv("build", c.Build.KeyEnv); err != nil {
			return err
		}
	}
	if k := c.Console; k != nil {
		if _, ok := c.Hosts[k.Host]; !ok {
			return fmt.Errorf("console: unknown host %q", k.Host)
		}
		if err := checkTokenEnv("console", k.TokenEnv); err != nil {
			return err
		}
		if !strings.HasPrefix(k.FleetdURL, "http://") && !strings.HasPrefix(k.FleetdURL, "https://") {
			return fmt.Errorf("console: fleetd_url %q is not an http(s) URL", k.FleetdURL)
		}
	}
	th := c.Thresholds
	if th.Low5h < 0 || th.Low5h > 100 || th.Reserved7d < 0 || th.Reserved7d > 100 {
		return fmt.Errorf("thresholds: low_5h and reserved_7d are percentages")
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// hostAlias is an ssh_config Host name; agent-run writes it into git config.
var hostAlias = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// checkTokenEnv refuses a missing name and, without echoing it, anything that
// looks like a token pasted where its env var's name belongs.
func checkTokenEnv(who, v string) error {
	if v == "" {
		return fmt.Errorf("%s: token_env is required", who)
	}
	if !envName.MatchString(v) {
		return fmt.Errorf("%s: token_env must be an env var NAME (e.g. CLAUDE_TOKEN_AGENT_01), not a token", who)
	}
	return nil
}

// AgentNames is every agent, sorted.
func (c *Config) AgentNames() []string {
	out := make([]string, 0, len(c.Agents))
	for n := range c.Agents {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AgentsOn is the agents on one host, sorted.
func (c *Config) AgentsOn(host string) []string {
	var out []string
	for _, n := range c.AgentNames() {
		if c.Agents[n].Host == host {
			out = append(out, n)
		}
	}
	return out
}

// HostNames is every host, sorted.
func (c *Config) HostNames() []string {
	out := make([]string, 0, len(c.Hosts))
	for n := range c.Hosts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NotifyOn reports whether phone alerts are enabled.
func (k Karmax) NotifyOn() bool { return k.Notify == nil || *k.Notify }

func expand(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
