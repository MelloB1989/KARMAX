package render

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/fleet/internal/identity"

	"gopkg.in/yaml.v3"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/console"
)

const fleetYAML = `home: /home/op
uid: 1000
gid: 1000
image: karmax-fleet:2.1.294
otlp_listen: 0.0.0.0:4318
relay_listen: 0.0.0.0:7879
hosts:
  kali: {exec: [docker], fleetd_url: "http://host.docker.internal:7879", otlp_url: "http://host.docker.internal:4318", karmax_url: "http://host.docker.internal:9091"}
  pc2:  {exec: [docker, --context, pc2], fleetd_url: "http://192.168.1.10:7879", otlp_url: "http://192.168.1.10:4318"}
orchestrator: {host: kali, token_env: CLAUDE_TOKEN_KARMAX}
agents:
  agent-01: {host: kali, token_env: CLAUDE_TOKEN_AGENT_01, models: [sonnet, opus], cpus: "2", memory: 4g}
  agent-02: {host: kali, token_env: CLAUDE_TOKEN_AGENT_02}
  agent-04: {host: pc2,  token_env: CLAUDE_TOKEN_AGENT_04}
`

var env = map[string]string{
	"CLAUDE_TOKEN_KARMAX": "sk-ant-oat01-K", "CLAUDE_TOKEN_AGENT_01": "sk-ant-oat01-A1",
	"CLAUDE_TOKEN_AGENT_02": "sk-ant-oat01-A2", "CLAUDE_TOKEN_AGENT_04": "sk-ant-oat01-A4",
	"FLEET_TOKEN": "full", "FLEET_RELAY_TOKEN": "relay", "KARMAX_API_TOKEN": "api",
}

func cfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(fleetYAML))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readEnv(t *testing.T, p string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestRenderWritesPrivateEnvFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(cfg(t), env, dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"agents/agent-01.env", "agents/agent-04.env", "agents/karmax-brain.env"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, st.Mode().Perm())
		}
	}
	a1 := readEnv(t, filepath.Join(dir, "agents/agent-01.env"))
	if a1["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-A1" || a1["FLEET_TOKEN"] != "relay" ||
		a1["OTEL_EXPORTER_OTLP_HEADERS"] != "Authorization=Bearer%20relay" {
		t.Errorf("agent-01 env = %v: an agent gets its own token and only the relay-scoped fleet token", a1)
	}
	if strings.Contains(strings.Join(mapValues(a1), " "), "sk-ant-oat01-A2") {
		t.Error("agent-01 can see agent-02's token")
	}
	brain := readEnv(t, filepath.Join(dir, "agents/karmax-brain.env"))
	if brain["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-K" || brain["FLEET_TOKEN"] != "full" || brain["KARMAX_API_TOKEN"] != "api" {
		t.Errorf("brain env = %v", brain)
	}
}

// Each agent gets a key of its own, which only the full token makes: fleetd
// tells agents apart with it, and none can make another's.
func TestEachAgentGetsItsOwnKeyAndNeverTheFullToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(cfg(t), env, dir); err != nil {
		t.Fatal(err)
	}
	a1 := readEnv(t, filepath.Join(dir, "agents/agent-01.env"))
	a4 := readEnv(t, filepath.Join(dir, "agents/agent-04.env"))
	if a1["FLEET_AGENT_KEY"] != identity.Key("full", "agent-01") || a4["FLEET_AGENT_KEY"] != identity.Key("full", "agent-04") {
		t.Fatalf("agent keys = %q, %q", a1["FLEET_AGENT_KEY"], a4["FLEET_AGENT_KEY"])
	}
	for _, e := range []map[string]string{a1, a4} {
		if slices.Contains(mapValues(e), "full") {
			t.Fatalf("an agent's env holds the full token: %v", e)
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestRenderRefusesASharedDirectory(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	if err := Render(cfg(t), env, dir); err == nil {
		t.Fatal("rendered tokens into a world-readable directory")
	}
}

func TestRenderNeedsEveryToken(t *testing.T) {
	short := map[string]string{"CLAUDE_TOKEN_KARMAX": "x"}
	err := Render(cfg(t), short, filepath.Join(t.TempDir(), "r"))
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_TOKEN_AGENT_01") || strings.Contains(err.Error(), "sk-ant") {
		t.Fatalf("err = %v", err)
	}
}

type compose struct {
	Services map[string]struct {
		Image         string            `yaml:"image"`
		ContainerName string            `yaml:"container_name"`
		PID           string            `yaml:"pid"`
		EnvFile       []string          `yaml:"env_file"`
		Environment   map[string]string `yaml:"environment"`
		Volumes       []string          `yaml:"volumes"`
		Tmpfs         []string          `yaml:"tmpfs"`
		CPUs          string            `yaml:"cpus"`
		MemLimit      string            `yaml:"mem_limit"`
		Restart       string            `yaml:"restart"`
	} `yaml:"services"`
	Volumes map[string]any `yaml:"volumes"`
}

func loadCompose(t *testing.T, p string) compose {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var c compose
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	if strings.Contains(string(b), "sk-ant") {
		t.Fatalf("%s carries a token", p)
	}
	return c
}

func TestComposePerHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(cfg(t), env, dir); err != nil {
		t.Fatal(err)
	}
	kali := loadCompose(t, filepath.Join(dir, "compose.kali.yaml"))
	var names []string
	for n := range kali.Services {
		names = append(names, n)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"agent-01", "agent-02", "fleet-anchor", "karmax-brain"}) {
		t.Fatalf("kali services = %v", names)
	}
	a := kali.Services["agent-01"]
	if a.PID != "service:fleet-anchor" || a.Image != "karmax-fleet:2.1.294" || a.Restart != "unless-stopped" || a.CPUs != "2" || a.MemLimit != "4g" {
		t.Errorf("agent-01 = %+v", a)
	}
	for _, v := range []string{"/home/op/code:/home/op/code", "fleet-peers:/home/op/.claude/sessions",
		"fleet-socks:/run/fleet", "agent-01-home:/home/op/.claude", "agent-01-work:/work"} {
		if !slices.Contains(a.Volumes, v) {
			t.Errorf("agent-01 lacks volume %s: %v", v, a.Volumes)
		}
	}
	e := a.Environment
	if e["FLEET_AGENT"] != "agent-01" || e["FLEET_NAME"] != "agent-01" || e["FLEET_HOST"] != "kali" || e["XDG_RUNTIME_DIR"] != "/run/fleet" || e["FLEET_CODE"] != "/home/op/code" ||
		e["FLEETD_URL"] != "http://host.docker.internal:7879" ||
		e["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://host.docker.internal:4318" ||
		!strings.Contains(e["OTEL_RESOURCE_ATTRIBUTES"], "fleet.agent=agent-01") {
		t.Errorf("agent-01 environment = %v", e)
	}
	brief, err := base64.StdEncoding.DecodeString(e["FLEET_CLAUDE_MD"])
	if err != nil || !strings.Contains(string(brief), "agent-01") || !strings.Contains(string(brief), "agent-02") ||
		!strings.Contains(string(brief), "agent-04") || !strings.Contains(string(brief), "fleetctl tell") {
		t.Errorf("CLAUDE.md = %s", brief)
	}
	b := kali.Services["karmax-brain"]
	if !slices.Contains(b.Volumes, "/home/op/.karmax/sessions:/home/op/.karmax/sessions") ||
		!slices.Contains(b.Volumes, "/home/op/.karmax/CLAUDE.md:/home/op/.karmax/CLAUDE.md:ro") ||
		b.Environment["FLEET_AGENT"] != "" || b.Environment["FLEET_NAME"] != "karmax" ||
		b.Environment["KARMAX_API_URL"] != "http://host.docker.internal:9091" {
		t.Errorf("karmax-brain = %+v", b)
	}

	pc2 := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml"))
	if _, ok := pc2.Services["karmax-brain"]; ok || len(pc2.Services) != 2 {
		t.Errorf("pc2 services = %v", pc2.Services)
	}
	if pc2.Services["agent-04"].Environment["FLEETD_URL"] != "http://192.168.1.10:7879" {
		t.Errorf("agent-04 fleetd url")
	}
}

// Credentials kept under the code root are covered in every agent
// container; the orchestrator, the operator's own session, still sees them.
func TestHiddenPathsAreCoveredInAgentsOnly(t *testing.T) {
	c := cfg(t)
	c.HideFromAgents = []string{"secrets", "KARMAX/.env.d"}
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(c, env, dir); err != nil {
		t.Fatal(err)
	}
	kali := loadCompose(t, filepath.Join(dir, "compose.kali.yaml"))
	want := []string{"/home/op/code/secrets:ro,size=4k,mode=755", "/home/op/code/KARMAX/.env.d:ro,size=4k,mode=755"}
	for _, n := range []string{"agent-01", "agent-02"} {
		if got := kali.Services[n].Tmpfs; !slices.Equal(got, want) {
			t.Errorf("%s tmpfs = %v", n, got)
		}
	}
	if got := kali.Services["karmax-brain"].Tmpfs; len(got) != 0 {
		t.Errorf("karmax-brain tmpfs = %v", got)
	}
	if got := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml")).Services["agent-04"].Tmpfs; !slices.Equal(got, want) {
		t.Errorf("agent-04 tmpfs = %v", got)
	}
}

func TestBriefNamesLocalAndRemotePeers(t *testing.T) {
	b, err := Brief(cfg(t), "agent-04")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b, "agent-04") || !strings.Contains(b, "fleet/agent-04/") {
		t.Fatalf("brief = %s", b)
	}
	local := section(b, "Local peers")
	remote := section(b, "Remote peers")
	if strings.Contains(local, "agent-01") || !strings.Contains(remote, "agent-01") || !strings.Contains(remote, "karmax") {
		t.Fatalf("pc2 agent: local=%q remote=%q", local, remote)
	}
}

func section(s, heading string) string {
	i := strings.Index(s, heading)
	if i < 0 {
		return ""
	}
	s = s[i:]
	if j := strings.Index(s[1:], "\n## "); j >= 0 {
		return s[:j+1]
	}
	return s
}

func TestOrchestratorBrief(t *testing.T) {
	b, err := OrchestratorBrief(cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent-01", "agent-02", "agent-04", "sonnet, opus", "fleetctl status --json",
		"fleetctl give", "fleetctl done", "fleetctl tell agent-04", "SendMessage", "low", "reserved", "fleet.agent."} {
		if !strings.Contains(b, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
	// KARMAX's task.create starts a worker session of its own, which then
	// watches the agent too; fleet work is tracked by fleetd alone.
	if strings.Contains(b, "task.open") || strings.Contains(b, "task.update") || !strings.Contains(b, "Do not open a KARMAX task") {
		t.Errorf("orchestrator brief must keep fleet work out of KARMAX tasks:\n%s", section(b, "Assigning"))
	}
}

// The persona lands only where the operator pointed it, beside — never over —
// the CLAUDE.md the orchestrator's session owns.
func TestRenderWritesTheOrchestratorBriefOnlyWhenAsked(t *testing.T) {
	c := cfg(t)
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(c, env, dir); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(c.Home, ".karmax", "sessions")
	if _, err := os.Stat(sessions); err == nil {
		t.Fatal("wrote into KARMAX's sessions without being asked")
	}
	c.Home = t.TempDir()
	c.Orchestrator.KarmaxAgent = "karmax-agent"
	own := filepath.Join(c.Home, ".karmax", "sessions", "agent_karmax_agent", "CLAUDE.md")
	_ = os.MkdirAll(filepath.Dir(own), 0o755)
	_ = os.WriteFile(own, []byte("the session's own notes"), 0o644)
	if err := Render(c, env, filepath.Join(t.TempDir(), "r2")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(own), ".claude", "CLAUDE.md"))
	if err != nil || !strings.Contains(string(b), "fleetctl give") {
		t.Fatalf("orchestrator brief: %v", err)
	}
	if mine, _ := os.ReadFile(own); string(mine) != "the session's own notes" {
		t.Fatal("overwrote the session's own CLAUDE.md")
	}
}

const nativeYAML = `home: /home/op
image: karmax-fleet:2.1.294
hosts:
  kali: {exec: [docker]}
  pc2:  {exec: [docker, --context, pc2], fleetd_url: "http://192.168.1.10:7879"}
orchestrator: {host: kali, native: true}
agents:
  agent-01: {host: pc2, token_env: CLAUDE_TOKEN_AGENT_01}
`

// A native orchestrator runs on the host outside Docker: no karmax-brain, no
// env file for it, and a host left with nothing to run gets no compose file,
// so `fleetctl up` starts nothing there.
func TestRenderWithANativeOrchestrator(t *testing.T) {
	c, err := config.Parse([]byte(nativeYAML))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(c, map[string]string{"CLAUDE_TOKEN_AGENT_01": "sk-ant-oat01-A1", "FLEET_TOKEN": "full", "FLEET_RELAY_TOKEN": "relay"}, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "karmax-brain.env")); !os.IsNotExist(err) {
		t.Errorf("karmax-brain.env written for a native orchestrator: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "compose.kali.yaml")); !os.IsNotExist(err) {
		t.Errorf("compose.kali.yaml written for a host with nothing to run: %v", err)
	}
	pc2 := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml"))
	if _, ok := pc2.Services["agent-01"]; !ok || len(pc2.Services) != 2 {
		t.Errorf("pc2 services = %v", pc2.Services)
	}
}

// With a build host, every agent's docker commands go to it over SSH: the
// address is plain config in the compose file, the key only in the 0600 env.
func TestRenderPointsAgentsAtTheBuildHost(t *testing.T) {
	c, err := config.Parse([]byte(nativeYAML + "build: {docker_host: \"ssh://fleetbuild@192.168.1.10\", key_env: FLEET_BUILD_KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "render")
	e := map[string]string{"CLAUDE_TOKEN_AGENT_01": "sk-ant-oat01-A1", "FLEET_TOKEN": "full", "FLEET_RELAY_TOKEN": "relay"}
	if err := Render(c, e, dir); err == nil || !strings.Contains(err.Error(), "FLEET_BUILD_KEY") {
		t.Fatalf("render without the build key: %v", err)
	}
	e["FLEET_BUILD_KEY"] = "a2V5"
	if err := Render(c, e, dir); err != nil {
		t.Fatal(err)
	}
	if got := readEnv(t, filepath.Join(dir, "agents", "agent-01.env"))["FLEET_BUILD_KEY"]; got != "a2V5" {
		t.Errorf("agent-01.env FLEET_BUILD_KEY = %q", got)
	}
	env := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml")).Services["agent-01"].Environment
	if env["DOCKER_HOST"] != "ssh://fleetbuild@192.168.1.10" || env["FLEET_BUILD_IMAGE"] != "karmax-fleet:2.1.294" {
		t.Errorf("agent-01 environment = %v", env)
	}
}

// An agent learns about kbuild only when there is a build host to send it to.
func TestBriefTellsAgentsAboutTheBuildHost(t *testing.T) {
	if b, _ := Brief(cfg(t), "agent-04"); strings.Contains(b, "kbuild") {
		t.Errorf("brief mentions kbuild with no build host:\n%s", b)
	}
	c, err := config.Parse([]byte(nativeYAML + "build: {docker_host: \"ssh://fleetbuild@192.168.1.10\", key_env: FLEET_BUILD_KEY}\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Brief(c, "agent-01")
	if err != nil {
		t.Fatal(err)
	}
	heavy := section(b, "Heavy work")
	if !strings.Contains(heavy, "kbuild") || !strings.Contains(heavy, "192.168.1.10") || !strings.Contains(heavy, "/home/op/code") {
		t.Errorf("Heavy work section = %q", heavy)
	}
}

// A turn an agent's report starts has no one to answer, so the orchestrator
// is told how to reach the operator — when fleet.yaml says how.
func TestOrchestratorBriefSaysHowToReportToTheOperator(t *testing.T) {
	if b, _ := OrchestratorBrief(cfg(t)); strings.Contains(b, "reaches nobody") {
		t.Errorf("report instruction without orchestrator.report")
	}
	c, err := config.Parse([]byte(strings.Replace(nativeYAML, "native: true}",
		`native: true, report: 'send it on WhatsApp: karmax tool call comms.send target=91000 content="<text>"'}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := OrchestratorBrief(c)
	if err != nil {
		t.Fatal(err)
	}
	if s := section(b, "When an agent reports back"); !strings.Contains(s, "comms.send target=91000") || !strings.Contains(s, "reaches nobody") {
		t.Errorf("report section = %q", s)
	}
}

// Agents commit under the fleet's git identity, named per agent, and push
// with the fleet's GitHub token — which, being a secret, is only in the env file.
func TestRenderGivesAgentsAGitIdentityAndGitHubToken(t *testing.T) {
	c, err := config.Parse([]byte(nativeYAML + "github: {token_env: GH_TOKEN_FLEET}\ngit: {name: \"Example coding agent\", email: build@example.com}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "render")
	e := map[string]string{"CLAUDE_TOKEN_AGENT_01": "sk-ant-oat01-A1", "FLEET_TOKEN": "full", "FLEET_RELAY_TOKEN": "relay"}
	if err := Render(c, e, dir); err == nil || !strings.Contains(err.Error(), "GH_TOKEN_FLEET") {
		t.Fatalf("render without the GitHub token: %v", err)
	}
	e["GH_TOKEN_FLEET"] = "gho_secret"
	if err := Render(c, e, dir); err != nil {
		t.Fatal(err)
	}
	if got := readEnv(t, filepath.Join(dir, "agents", "agent-01.env"))["GH_TOKEN"]; got != "gho_secret" {
		t.Errorf("agent-01.env GH_TOKEN = %q", got)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.pc2.yaml"))
	if strings.Contains(string(b), "gho_secret") {
		t.Fatal("the GitHub token reached the compose file")
	}
	env := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml")).Services["agent-01"].Environment
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		if env[k] != "Example coding agent (agent-01)" {
			t.Errorf("%s = %q", k, env[k])
		}
	}
	if env["GIT_AUTHOR_EMAIL"] != "build@example.com" || env["GIT_COMMITTER_EMAIL"] != "build@example.com" {
		t.Errorf("git email: %v", env)
	}
}

// The operator's own SSH host aliases for GitHub (~/.ssh/config Host entries)
// are config, not image: the image rewrites only github.com's own forms.
func TestRenderPassesGitHubSSHAliasesToAgents(t *testing.T) {
	c, err := config.Parse([]byte(nativeYAML + "github: {token_env: GH_TOKEN_FLEET, ssh_aliases: [gh-work, gh-personal]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "render")
	e := map[string]string{"CLAUDE_TOKEN_AGENT_01": "sk-ant-oat01-A1", "FLEET_TOKEN": "full", "FLEET_RELAY_TOKEN": "relay", "GH_TOKEN_FLEET": "gho_secret"}
	if err := Render(c, e, dir); err != nil {
		t.Fatal(err)
	}
	env := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml")).Services["agent-01"].Environment
	if got := env["FLEET_GITHUB_SSH_ALIASES"]; got != "gh-work gh-personal" {
		t.Errorf("FLEET_GITHUB_SSH_ALIASES = %q", got)
	}
}

func TestBriefSaysReportToTheAssigner(t *testing.T) {
	c := cfg(t)
	b, err := Brief(c, c.AgentNames()[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"report to whoever assigned it", `fleetctl tell operator`, "`Task <id> from <name>.`"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	o, _ := OrchestratorBrief(c)
	if !strings.Contains(o, "--from "+c.Orchestrator.Name) {
		t.Errorf("orchestrator brief does not pass --from:\n%s", o)
	}
}

const consoleYAML = `console:
  host: pc2
  fleetd_url: http://192.168.1.2:7879
  token_env: FLEET_CONSOLE_TOKEN
  repos: [~/code/KARMAX]
  dir: ~/.config/fleet-console
`

func consoleCfg(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(fleetYAML + consoleYAML))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func withConsoleToken() map[string]string {
	e := map[string]string{"FLEET_CONSOLE_TOKEN": "console-secret"}
	for k, v := range env {
		e[k] = v
	}
	return e
}

func TestRenderWritesConsoleConfigForItsHost(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(consoleCfg(t), withConsoleToken(), dir); err != nil {
		t.Fatal(err)
	}
	cc, err := console.LoadConfig(filepath.Join(dir, "pc2", "console", "console.yaml"))
	if err != nil {
		t.Fatalf("the console cannot load what render wrote: %v", err)
	}
	if cc.Host != "pc2" || cc.Listen != "127.0.0.1:7880" || cc.FleetdURL != "http://192.168.1.2:7879" {
		t.Errorf("config = %+v", cc)
	}
	if len(cc.Agents) != 3 {
		t.Fatalf("agents = %+v, want the whole roster", cc.Agents)
	}
	byName := map[string]console.AgentConf{}
	for _, a := range cc.Agents {
		byName[a.Name] = a
	}
	if a := byName["agent-04"]; !a.Live || !slices.Equal(a.Exec, []string{"docker"}) || a.Container == "" {
		t.Errorf("agent-04 (on the console's host) = %+v, want live with [docker]", a)
	}
	for _, n := range []string{"agent-01", "agent-02"} {
		if a := byName[n]; a.Live || a.Host != "kali" || len(a.Exec) != 0 {
			t.Errorf("%s (another host) = %+v, want recorded-only", n, a)
		}
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".config/fleet-console/fleetd.token"); cc.FleetdTokenFile != want {
		t.Errorf("fleetd_token_file = %s, want %s", cc.FleetdTokenFile, want)
	}
	if filepath.Base(cc.ClientTokenFile) != "client.token" || filepath.Dir(cc.ClientTokenFile) != filepath.Dir(cc.FleetdTokenFile) {
		t.Errorf("client_token_file = %s", cc.ClientTokenFile)
	}
	if !slices.Equal(cc.Repos, []string{filepath.Join(home, "code/KARMAX")}) {
		t.Errorf("repos = %v", cc.Repos)
	}
}

func TestRenderSkipsConsoleWhenNotConfigured(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(cfg(t), env, dir); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"kali", "pc2"} {
		if _, err := os.Stat(filepath.Join(dir, h, "console")); !os.IsNotExist(err) {
			t.Errorf("%s/console exists without a console: section (%v)", h, err)
		}
	}
	// And a console section without its token is refused, naming the variable.
	err := Render(consoleCfg(t), env, filepath.Join(t.TempDir(), "r2"))
	if err == nil || !strings.Contains(err.Error(), "FLEET_CONSOLE_TOKEN") {
		t.Errorf("render without the console token: %v", err)
	}
}

func TestConsoleTokenFileIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "render")
	if err := Render(consoleCfg(t), withConsoleToken(), dir); err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(dir, "pc2", "console")
	st, err := os.Stat(filepath.Join(cdir, "fleetd.token"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("fleetd.token mode %v, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(filepath.Join(cdir, "fleetd.token")); strings.TrimSpace(string(b)) != "console-secret" {
		t.Error("fleetd.token does not hold the console token")
	}
	if b, _ := os.ReadFile(filepath.Join(cdir, "console.yaml")); strings.Contains(string(b), "console-secret") {
		t.Error("the token is in console.yaml")
	}
	if st, _ := os.Stat(cdir); st.Mode().Perm()&0o077 != 0 {
		t.Errorf("console dir mode %v is readable by others", st.Mode().Perm())
	}
	// A shared directory is refused, as for the env files.
	_ = os.Chmod(cdir, 0o755)
	if err := Render(consoleCfg(t), withConsoleToken(), dir); err == nil {
		t.Error("re-render into a world-readable console dir succeeded")
	}
}

func TestOrchestratorBriefUsesTheJobCommands(t *testing.T) {
	b, err := OrchestratorBrief(cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fleetctl run", "fleetctl result", "fleetctl followup", "fleetctl cancel", "fleetctl done --task", "fleetctl guide"} {
		if !strings.Contains(b, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}

func TestAgentBriefSaysReportsFileUnderTheTask(t *testing.T) {
	b, err := Brief(cfg(t), "agent-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"filed under", "follow-up", "cancel"} {
		if !strings.Contains(b, want) {
			t.Errorf("agent brief lacks %q", want)
		}
	}
}

// github.owners: each owner's token goes to the env file under its own name,
// the list of owners (no secret) to the compose file; packages_token_env is
// GITHUB_TOKEN. Every named token must be in the env file.
func TestRenderGivesAgentsOwnerTokens(t *testing.T) {
	c, err := config.Parse([]byte(nativeYAML + "github: {token_env: GH_TOKEN_FLEET, owners: {acme-org: GH_TOKEN_ACME, beta: GH_TOKEN_BETA}, packages_token_env: GH_TOKEN_ACME}\n"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "render")
	e := map[string]string{"CLAUDE_TOKEN_AGENT_01": "sk-ant-oat01-A1", "FLEET_TOKEN": "full", "FLEET_RELAY_TOKEN": "relay",
		"GH_TOKEN_FLEET": "gho_fleet", "GH_TOKEN_ACME": "gho_acme"}
	if err := Render(c, e, dir); err == nil || !strings.Contains(err.Error(), "GH_TOKEN_BETA") {
		t.Fatalf("render without an owner's token: %v", err)
	}
	e["GH_TOKEN_BETA"] = "gho_beta"
	if err := Render(c, e, dir); err != nil {
		t.Fatal(err)
	}
	got := readEnv(t, filepath.Join(dir, "agents", "agent-01.env"))
	want := map[string]string{"GH_TOKEN": "gho_fleet", "GH_TOKEN_OWNER_ACME_ORG": "gho_acme", "GH_TOKEN_OWNER_BETA": "gho_beta", "GITHUB_TOKEN": "gho_acme"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("agent-01.env %s = %q, want %q", k, got[k], v)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "compose.pc2.yaml"))
	for _, secret := range []string{"gho_acme", "gho_beta"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("%s reached the compose file", secret)
		}
	}
	env := loadCompose(t, filepath.Join(dir, "compose.pc2.yaml")).Services["agent-01"].Environment
	if env["FLEET_GITHUB_OWNERS"] != "acme-org beta" {
		t.Errorf("FLEET_GITHUB_OWNERS = %q", env["FLEET_GITHUB_OWNERS"])
	}
	brief, _ := Brief(c, "agent-01")
	gh := section(brief, "## GitHub accounts")
	if !strings.Contains(gh, "`acme-org`, `beta`") || !strings.Contains(gh, "GITHUB_TOKEN") {
		t.Errorf("brief's GitHub section:\n%s", gh)
	}
}

// Without owners or a packages token the brief says nothing about accounts.
func TestBriefOmitsGitHubAccountsByDefault(t *testing.T) {
	if b, _ := Brief(cfg(t), "agent-04"); strings.Contains(b, "GitHub accounts") {
		t.Errorf("brief has a GitHub accounts section:\n%s", b)
	}
}

func TestTheOrchestratorBriefSaysHowToDecideOnTheGatewaysWrites(t *testing.T) {
	b, err := OrchestratorBrief(cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fleetctl approve <id>", "fleetctl deny <id> --reason", "fleetctl approvals",
		"on WhatsApp first", "you alone write memory", "anything that runs code or starts"} {
		if !strings.Contains(b, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}

func TestAgentBriefSaysHowToUseTheOperatorsIntegrations(t *testing.T) {
	b, err := Brief(cfg(t), "agent-01")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fleetctl karmax tools", "fleetctl karmax call", "--reason", "never\nhold KARMAX's token", "fleetd refuses"} {
		if !strings.Contains(b, want) {
			t.Errorf("agent brief lacks %q", want)
		}
	}
}

// The one section that says what an agent has and the rules: each tool and
// each rule the operator decided, and nothing a public repo cannot hold.
func TestAgentBriefStatesWhatTheAgentHasAndTheRules(t *testing.T) {
	b, err := Brief(cfg(t), "agent-01")
	if err != nil {
		t.Fatal(err)
	}
	have := section(b, "## What you have, and the rules")
	for _, want := range []string{
		"### Talking", "fleetctl tell", "--kind progress", "--kind question", "fleetctl ask", "exits 4",
		"### Files", "/work/inbox/<task>/",
		"### The operator's accounts", "fleetctl karmax tools", "fleetctl karmax call", "--reason", "Every read is free",
		"Every write", "waits for\n  approval", "WhatsApp", "run code", "denied outright", "any other route",
		"### Memory", "fleetctl memory recall", "fleetctl memory suggest", "only the orchestrator saves",
		"### Secrets", "fleetctl secret", "Never print one, commit one",
		"### Public repos", "pre-commit hook", "--no-verify",
		"`fleet-agent` skill", "`fleet-memory` skill",
	} {
		if !strings.Contains(have, want) {
			t.Errorf("What you have, and the rules lacks %q", want)
		}
	}
	for _, old := range []string{"## The operator's integrations", "\n## Heavy work", "\n## GitHub accounts"} {
		if strings.Contains(b, old) {
			t.Errorf("brief still has the scattered section %q", old)
		}
	}
}

// The brief goes into a public repo's tests and is read by every agent: it
// carries only fake values.
func TestBriefsNameNoRealPersonLoginPhoneOrAddress(t *testing.T) {
	c, err := config.Parse([]byte(nativeYAML + "build: {docker_host: \"ssh://fleetbuild@192.0.2.10\", key_env: FLEET_BUILD_KEY}\ngithub: {token_env: GH_TOKEN_FLEET, owners: {acme-org: GH_TOKEN_ACME}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := Brief(c, "agent-01")
	orch, _ := OrchestratorBrief(c)
	phone := regexp.MustCompile(`\+\d[\d ()-]{7,}|\b\d{10,}\b`)
	ip := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	email := regexp.MustCompile(`[\w.+-]+@[\w.-]+`)
	home := regexp.MustCompile(`/(home|Users)/[\w.-]+`)
	for name, text := range map[string]string{"agent brief": agent, "orchestrator brief": orch} {
		if m := phone.FindString(text); m != "" {
			t.Errorf("%s has a phone number: %q", name, m)
		}
		for _, m := range ip.FindAllString(text, -1) {
			if !strings.HasPrefix(m, "192.0.2.") && !strings.HasPrefix(m, "100.64.0.") {
				t.Errorf("%s has an address that is not documentation-range: %q", name, m)
			}
		}
		for _, m := range email.FindAllString(text, -1) {
			if !strings.HasSuffix(m, "@example.com") && !strings.HasPrefix(m, "fleetbuild@") {
				t.Errorf("%s has a login that is not @example.com: %q", name, m)
			}
		}
		for _, m := range home.FindAllString(text, -1) {
			if m != "/home/op" {
				t.Errorf("%s has a home path that is not /home/op: %q", name, m)
			}
		}
	}
}

func TestOrchestratorBriefSaysWhatReachesHerFromTheAgentsToolkit(t *testing.T) {
	b, err := OrchestratorBrief(cfg(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## What your agents have, and what you decide", "`fleet-agent`", "`fleet-memory`", "**Approvals.**",
		"Memory suggestion from <agent>", "You alone", "**Questions.**", "fleetctl approve <id>"} {
		if !strings.Contains(b, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
}
