package render

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/MelloB1989/karmax/fleet/internal/config"
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
	if e["FLEET_AGENT"] != "agent-01" || e["FLEET_HOST"] != "kali" || e["XDG_RUNTIME_DIR"] != "/run/fleet" ||
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
		b.Environment["FLEET_AGENT"] != "" || b.Environment["KARMAX_API_URL"] != "http://host.docker.internal:9091" {
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
