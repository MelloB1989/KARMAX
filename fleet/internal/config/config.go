// Package config reads fleet.yaml: which agents exist, where they run, whose
// subscription each one uses, and every threshold the reconciler acts on.
//
// It never holds a secret. An agent names the env var its token is in
// (token_env); the value lives only in the env file and the 0600 per-agent
// file rendered from it.
package config

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	Code  string `yaml:"code"`
	Image string `yaml:"image"`
	UID   int    `yaml:"uid"`
	GID   int    `yaml:"gid"`
	// User is the account name inside the image (`fleetctl image build`);
	// empty is the operator's own login name.
	User string `yaml:"user"`
	// ClaudeVersion pins Claude Code in the image; empty keeps the
	// Dockerfile's own pin.
	ClaudeVersion string `yaml:"claude_version"`
	EnvFile       string `yaml:"env_file"`
	StateDir      string `yaml:"state_dir"`

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

	// Slots is how many sessions each agent runs at once, each a worker of
	// its own (Workers); agents.<name>.slots overrides it. Default 2.
	Slots int `yaml:"slots"`

	Hosts        map[string]Host  `yaml:"hosts"`
	Orchestrator Orchestrator     `yaml:"orchestrator"`
	Agents       map[string]Agent `yaml:"agents"`
	Karmax       Karmax           `yaml:"karmax"`
	Build        Build            `yaml:"build"`
	GitHub       GitHub           `yaml:"github"`
	Git          Git              `yaml:"git"`
	Thresholds   Thresholds       `yaml:"thresholds"`
	Files        Files            `yaml:"files"`
	// Console is the fleet console on one host; nil means none.
	Console *Console `yaml:"console"`
	// Tailnet opens the API to people on the Tailscale network; nil means
	// it is not.
	Tailnet *Tailnet `yaml:"tailnet"`
	// HideFromAgents are paths under Code the agents do not see: each is
	// covered by an empty read-only tmpfs in their containers.
	HideFromAgents []string `yaml:"hide_from_agents"`
	// Memory is the operator's long-term memory that fleetd reads for its
	// callers; nil means none.
	Memory *Memory `yaml:"memory"`
	// SandboxNet is the network sandbox agents run on.
	SandboxNet SandboxNet `yaml:"sandbox"`
}

// SandboxNet is fleet-sandbox, the Docker bridge sandbox agents run on: it
// reaches the internet and fleetd's relay and telemetry ports, and nothing
// else (the rules fleetctl render writes beside each host's compose file).
type SandboxNet struct {
	// Subnet is the bridge's fixed subnet, which the rules match; default
	// 172.31.250.0/24.
	Subnet string `yaml:"subnet"`
}

// Memory is the gitloom namespace the orchestrator keeps. fleetd holds its
// key and only reads with it: agents and sessions recall, and suggest what
// to save to the orchestrator, which alone writes.
type Memory struct {
	// URL is gitloom on fleetd's host, e.g. http://127.0.0.1:8787.
	URL string `yaml:"url"`
	// KeyEnv names gitloom's key in the env file.
	KeyEnv    string `yaml:"key_env"`
	Namespace string `yaml:"namespace"`
}

// Files is fleetctl cp: files to and from an agent's container.
type Files struct {
	// MaxBytes caps one transfer, as the tar stream's size; 0 is 2 GiB.
	MaxBytes int64 `yaml:"max_bytes"`
}

// Tailnet is fleetd's API for people on the Tailscale network. Who calls is
// who Tailscale says (tailscaled's WhoIs on the caller's address), so there
// is no token to hand out: a login on Operators has the full token's scope,
// one on Members may run tasks as itself and see and close its own. A
// tagged device is nobody, and so is anyone on neither list.
type Tailnet struct {
	// Listen is the API's address on the tailnet, e.g. 100.64.0.1:7878.
	// It binds before tailscale0 has the address (IP_FREEBIND).
	Listen string `yaml:"listen"`
	// Operators and Members are Tailscale logins (you@example.com), or
	// "*@example.com" for every login of that domain.
	Operators []string `yaml:"operators"`
	Members   []string `yaml:"members"`
	// MemberOpen is how many open tasks one member may hold; 0 is 2.
	MemberOpen int `yaml:"member_open"`
	// Socket is tailscaled's LocalAPI socket.
	Socket string `yaml:"socket"`
	// Dist holds the fleetctl builds served at /dl/ (fleet/scripts/dist.sh).
	Dist string `yaml:"dist"`
}

// Role is what a Tailscale login may do on the tailnet listener:
// "operator", "member", or "" (nothing).
func (t *Tailnet) Role(login string) string {
	switch {
	case t == nil || login == "":
		return ""
	case matchLogin(t.Operators, login):
		return "operator"
	case matchLogin(t.Members, login):
		return "member"
	}
	return ""
}

func matchLogin(list []string, login string) bool {
	login = strings.ToLower(login)
	for _, p := range list {
		p = strings.ToLower(p)
		if p == login || strings.HasPrefix(p, "*@") && strings.HasSuffix(login, p[1:]) {
			return true
		}
	}
	return false
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
	// Slots is how many sessions the agent runs at once; 0 is the top-level
	// slots. Each needs about 1.25 GB of the container's memory.
	Slots int `yaml:"slots"`
	// Sandbox makes the agent one that can be lent to a guest device: none
	// of the operator's code, accounts or credentials in its container
	// (docs/superpowers/specs/2026-10-25-fleetctl-tui-access-design.md, 2).
	Sandbox bool `yaml:"sandbox"`
}

// maxSlots bounds an agent's slots: past it one container is a host.
const maxSlots = 8

// Worker is one session slot of an agent: what fleetd observes, decides on
// and hands tasks to. Slot 1 is the agent itself and keeps its name, so its
// state, ledger rows and archives from before slots stay its own; slot k ≥ 2
// is "<agent>:k". Every worker of an agent shares its container and its
// subscription.
type Worker struct {
	Name      string
	Agent     string // the agent it is a slot of
	Slot      int    // 1-based
	Host      string
	Container string
	Account   string
	Models    []string
	Model     string
	Sandbox   bool
}

// WorkerName is the name of an agent's slot.
func WorkerName(agent string, slot int) string {
	if slot <= 1 {
		return agent
	}
	return agent + ":" + strconv.Itoa(slot)
}

// SplitWorker is the agent and slot a worker name stands for: "agent-03:2"
// is agent-03's slot 2, any name without a slot is slot 1. It does not say
// whether the worker exists (Worker does).
func SplitWorker(name string) (agent string, slot int) {
	a, k, ok := strings.Cut(name, ":")
	if !ok {
		return name, 1
	}
	n, err := strconv.Atoi(k)
	if err != nil || n < 2 || strconv.Itoa(n) != k {
		return name, 0
	}
	return a, n
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
// (gh, and git through the image's credential helper).
type GitHub struct {
	TokenEnv string `yaml:"token_env"`
	// SSHAliases are the operator's ~/.ssh/config Host names for github.com,
	// so a remote cloned as git@<alias>:owner/repo is rewritten to HTTPS in
	// the agents, which hold no SSH keys.
	SSHAliases []string `yaml:"ssh_aliases"`
	// Owners gives some owners' repositories their own account — an org the
	// fleet's account cannot see — as owner → the env file key of its token
	// (gh auth token --user <account>). git and gh in the image pick it by the
	// repository's owner; every other repository keeps token_env.
	Owners map[string]string `yaml:"owners"`
	// PackagesTokenEnv is handed to agents as GITHUB_TOKEN, which an .npmrc for
	// GitHub Packages reads (//npm.pkg.github.com/:_authToken=${GITHUB_TOKEN}).
	PackagesTokenEnv string `yaml:"packages_token_env"`
}

// OwnerNames are the github.owners keys, sorted.
func (g GitHub) OwnerNames() []string {
	out := make([]string, 0, len(g.Owners))
	for o := range g.Owners {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

// OwnerTokenVar is the agent env var holding an owner's token; the image's
// fleet-gh-token derives the same name (upper case, '-' as '_').
func OwnerTokenVar(owner string) string {
	return "GH_TOKEN_OWNER_" + strings.ToUpper(strings.ReplaceAll(owner, "-", "_"))
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
	// APIURL is KARMAX's API, read for the orchestrator's own quota and
	// called by the gateway (fleetctl karmax) with KARMAX_API_TOKEN, which
	// only fleetd holds. Default http://127.0.0.1:9091.
	APIURL string `yaml:"api_url"`
	// Tools is the gateway's policy, tool name (or prefix*) → read, write or
	// deny. A read runs at once, a write waits for the orchestrator's
	// approval, a deny is refused. Unlisted tools are classed by name
	// (ToolClass); a few are always denied, whatever this says.
	Tools map[string]string `yaml:"tools"`
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
	// QuestionEscalate is how long a question asked of the operator or a
	// session may go unanswered before the orchestrator is asked, and how
	// much longer before you get an alert.
	QuestionEscalate time.Duration `yaml:"question_escalate"`
	// HeldAlertAfter is how long a message may be held for a receiver that
	// cannot take it before you are told, once. Default 1h.
	HeldAlertAfter time.Duration `yaml:"held_alert_after"`
	// HostSettle is how long after a host returns from out of reach, or
	// boots, a session that will not start is not counted as crashing.
	// Default 10m.
	HostSettle time.Duration `yaml:"host_settle"`
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
	if c.User == "" {
		if u, err := user.Current(); err == nil {
			c.User = u.Username
		}
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7878"
	}
	if t := c.Tailnet; t != nil {
		if t.MemberOpen <= 0 {
			t.MemberOpen = 2
		}
		if t.Socket == "" {
			t.Socket = "/var/run/tailscale/tailscaled.sock"
		}
		or(&t.Dist, filepath.Join(c.StateDir, "dist"))
	}
	if c.Files.MaxBytes <= 0 {
		c.Files.MaxBytes = 2 << 30
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
	if c.Slots == 0 {
		c.Slots = 2
	}
	if c.SandboxNet.Subnet == "" {
		c.SandboxNet.Subnet = "172.31.250.0/24"
	}
	for name, a := range c.Agents {
		if a.Sandbox && a.Slots == 0 {
			a.Slots = 1 // lent whole to one guest's work at a time
		}
		if a.Sandbox && a.Memory == "" {
			a.Memory = "1536m"
		}
		if a.Slots == 0 {
			a.Slots = c.Slots
		}
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
	dur(&th.QuestionEscalate, 10*time.Minute)
	dur(&th.HeldAlertAfter, time.Hour)
	dur(&th.HostSettle, 10*time.Minute)
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
	if c.Slots < 1 || c.Slots > maxSlots {
		return fmt.Errorf("slots: %d: want 1 to %d", c.Slots, maxSlots)
	}
	if _, slot := SplitWorker(c.Orchestrator.Name); slot != 1 {
		return fmt.Errorf("orchestrator: name %q: a name with \":\" in it is an agent's slot", c.Orchestrator.Name)
	}
	if _, _, err := net.ParseCIDR(c.SandboxNet.Subnet); err != nil {
		return fmt.Errorf("sandbox: subnet %q: want a CIDR like 172.31.250.0/24", c.SandboxNet.Subnet)
	}
	for _, name := range c.AgentNames() {
		a := c.Agents[name]
		// A worker of slot k is <agent>:k: an agent named like one would be
		// two workers with one name.
		if _, slot := SplitWorker(name); slot != 1 {
			return fmt.Errorf("agent %q: names with \":\" in them are taken by agents' slots (agent-01:2)", name)
		}
		if !namePattern.MatchString(name) {
			return fmt.Errorf("agent %q: names are lowercase letters, digits and dashes", name)
		}
		if name == c.Orchestrator.Name {
			return fmt.Errorf("agent %q: same name as the orchestrator", name)
		}
		if _, ok := c.Hosts[a.Host]; !ok {
			return fmt.Errorf("agent %q: unknown host %q", name, a.Host)
		}
		if a.Slots < 1 || a.Slots > maxSlots {
			return fmt.Errorf("agent %q: slots: %d: want 1 to %d", name, a.Slots, maxSlots)
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
	seenOwner := map[string]string{}
	for _, o := range c.GitHub.OwnerNames() {
		if !ghOwner.MatchString(o) {
			return fmt.Errorf("github: owners: %q is not a GitHub user or org name", o)
		}
		// GitHub names are case-insensitive, and the env var is upper case.
		if prev, dup := seenOwner[strings.ToLower(o)]; dup {
			return fmt.Errorf("github: owners: %q and %q are the same owner", prev, o)
		}
		seenOwner[strings.ToLower(o)] = o
		if err := checkTokenEnv("github: owners: "+o, c.GitHub.Owners[o]); err != nil {
			return err
		}
	}
	if c.GitHub.PackagesTokenEnv != "" && !envName.MatchString(c.GitHub.PackagesTokenEnv) {
		return fmt.Errorf("github: packages_token_env must be an env var NAME, not a token")
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
		// One token per scope: a console token that is the full or relay one
		// would hand the console (or whoever reads its env) a wider scope.
		if k.TokenEnv == c.TokenEnv || k.TokenEnv == c.RelayTokenEnv {
			return fmt.Errorf("console: token_env %s is also the fleet API's token_env or relay_token_env — the console needs a token of its own", k.TokenEnv)
		}
		if !strings.HasPrefix(k.FleetdURL, "http://") && !strings.HasPrefix(k.FleetdURL, "https://") {
			return fmt.Errorf("console: fleetd_url %q is not an http(s) URL", k.FleetdURL)
		}
	}
	if t := c.Tailnet; t != nil {
		if _, port, err := net.SplitHostPort(t.Listen); err != nil || port == "" {
			return fmt.Errorf("tailnet: listen %q: want the tailnet address and a port, e.g. 100.64.0.1:7878", t.Listen)
		}
		for _, l := range append(append([]string{}, t.Operators...), t.Members...) {
			if at := strings.Index(l, "@"); at < 1 || at == len(l)-1 || strings.Contains(l[at+1:], "*") || strings.Contains(l[:at], "*") && l[:at] != "*" {
				return fmt.Errorf("tailnet: %q is not a Tailscale login (you@example.com) or *@example.com", l)
			}
		}
	}
	if m := c.Memory; m != nil {
		if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
			return fmt.Errorf("memory: url %q is not an http(s) URL", m.URL)
		}
		if m.KeyEnv == "" || !envName.MatchString(m.KeyEnv) {
			return fmt.Errorf("memory: key_env must be an env var NAME (e.g. GITLOOM_API_KEY), not a key")
		}
		// The key reaches nobody: one that is a fleet token's env var would.
		if m.KeyEnv == c.TokenEnv || m.KeyEnv == c.RelayTokenEnv {
			return fmt.Errorf("memory: key_env %s is also a fleet token's env var — gitloom's key needs one of its own", m.KeyEnv)
		}
		if strings.TrimSpace(m.Namespace) == "" {
			return fmt.Errorf("memory: namespace is required (the orchestrator's gitloom namespace)")
		}
	}
	for _, p := range c.HideFromAgents {
		if p == "" || filepath.IsAbs(p) || filepath.Clean(p) == "." || strings.HasPrefix(filepath.Clean(p), "..") {
			return fmt.Errorf("hide_from_agents: %q: want a path under code, like secrets", p)
		}
	}
	if err := c.Karmax.validateTools(); err != nil {
		return err
	}
	// A name with @ in it is a person on the tailnet (a task's requester).
	if strings.Contains(c.Orchestrator.Name, "@") {
		return fmt.Errorf("orchestrator: name %q: no @ in it — that is a person's login", c.Orchestrator.Name)
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

// ghOwner is a GitHub user or org name; it becomes part of an env var name.
var ghOwner = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`)

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

// Workers is every agent's slots, sorted by agent and then slot.
func (c *Config) Workers() []Worker {
	var out []Worker
	for _, n := range c.AgentNames() {
		a := c.Agents[n]
		for k := 1; k <= max(a.Slots, 1); k++ {
			out = append(out, Worker{Name: WorkerName(n, k), Agent: n, Slot: k, Host: a.Host,
				Container: a.Container, Account: a.Account, Models: a.Models, Model: a.Model, Sandbox: a.Sandbox})
		}
	}
	return out
}

// WorkerNames is every worker's name, in Workers' order.
func (c *Config) WorkerNames() []string {
	ws := c.Workers()
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}

// Worker is the worker of that name.
func (c *Config) Worker(name string) (Worker, bool) {
	agent, slot := SplitWorker(name)
	a, ok := c.Agents[agent]
	if !ok || slot < 1 || slot > max(a.Slots, 1) {
		return Worker{}, false
	}
	return Worker{Name: WorkerName(agent, slot), Agent: agent, Slot: slot, Host: a.Host,
		Container: a.Container, Account: a.Account, Models: a.Models, Model: a.Model, Sandbox: a.Sandbox}, true
}

// Siblings is the other workers of a worker's agent, in slot order.
func (c *Config) Siblings(name string) []string {
	w, ok := c.Worker(name)
	if !ok {
		return nil
	}
	var out []string
	for k := 1; k <= max(c.Agents[w.Agent].Slots, 1); k++ {
		if k != w.Slot {
			out = append(out, WorkerName(w.Agent, k))
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
