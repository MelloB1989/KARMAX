package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const example = `
home: /home/op
image: karmax-fleet:2.1.294
hosts:
  kali: {exec: [docker]}
  pc2:  {exec: [docker, --context, pc2], fleetd_url: "http://192.168.1.10:7879"}
orchestrator: {host: kali, token_env: CLAUDE_TOKEN_KARMAX}
agents:
  agent-01: {host: kali, token_env: CLAUDE_TOKEN_AGENT_01, models: [sonnet, opus]}
  agent-04: {host: pc2,  token_env: CLAUDE_TOKEN_AGENT_04}
thresholds:
  standby_idle: 6h
`

func TestParseFillsDefaults(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if c.Code != "/home/op/code" || c.StateDir != "/home/op/.karmax/fleet" || c.EnvFile != "/home/op/.karmax/.env" {
		t.Errorf("paths: code=%q state=%q env=%q", c.Code, c.StateDir, c.EnvFile)
	}
	if c.Orchestrator.Name != "karmax" || c.Orchestrator.Container != "karmax-brain" {
		t.Errorf("orchestrator = %+v", c.Orchestrator)
	}
	th := c.Thresholds
	if th.Tick != 30*time.Second || th.StandbyIdle != 6*time.Hour || th.WaitingNudge != 2*time.Hour ||
		th.WaitingArchive != 24*time.Hour || th.Low5h != 80 || th.Reserved7d != 90 ||
		th.RestartFailures != 3 || th.RestartWindow != 10*time.Minute || th.CoolingDefault != time.Hour {
		t.Errorf("thresholds = %+v", th)
	}
	if c.Listen != "127.0.0.1:7878" || c.TokenEnv != "FLEET_TOKEN" {
		t.Errorf("listen=%q token_env=%q", c.Listen, c.TokenEnv)
	}
	if a := c.Agents["agent-01"]; a.Container != "agent-01" || !slices.Equal(a.Models, []string{"sonnet", "opus"}) {
		t.Errorf("agent-01 = %+v", a)
	}
	if a := c.Agents["agent-04"]; !slices.Equal(a.Models, []string{"sonnet"}) {
		t.Errorf("agent-04 models default = %v", a.Models)
	}
}

func TestAgentsOrderAndPeers(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.AgentNames(); !slices.Equal(got, []string{"agent-01", "agent-04"}) {
		t.Errorf("AgentNames = %v", got)
	}
	if got := c.AgentsOn("pc2"); !slices.Equal(got, []string{"agent-04"}) {
		t.Errorf("AgentsOn(pc2) = %v", got)
	}
	if got := c.Hosts["pc2"].Exec; !slices.Equal(got, []string{"docker", "--context", "pc2"}) {
		t.Errorf("pc2 exec = %v", got)
	}
}

func TestParseRejectsWhatWouldGoWrongLater(t *testing.T) {
	cases := map[string]string{
		"unknown host":        "agents: {agent-01: {host: nope, token_env: T}}",
		"no token env":        "agents: {agent-01: {host: kali}}",
		"bad name":            "agents: {'Agent 1': {host: kali, token_env: T}}",
		"token inline":        "agents: {agent-01: {host: kali, token_env: sk-ant-oat01-abc}}",
		"shared token":        "agents: {agent-01: {host: kali, token_env: T}, agent-02: {host: kali, token_env: T}}",
		"orchestrator host":   "orchestrator: {host: nope, token_env: O}\nagents: {agent-01: {host: kali, token_env: T}}",
		"name clash":          "orchestrator: {host: kali, token_env: O, name: agent-01}\nagents: {agent-01: {host: kali, token_env: T}}",
		"no exec":             "hosts: {kali: {}}\norchestrator: {host: kali, token_env: O}\nagents: {agent-01: {host: kali, token_env: T}}",
		"low above reserved?": "thresholds: {low_5h: 120}\nagents: {agent-01: {host: kali, token_env: T}}",
		"bad ssh alias":       "github: {ssh_aliases: ['gh; rm -rf /']}\nagents: {agent-01: {host: kali, token_env: T}}",
		"bad owner":           "github: {owners: {'acme; rm': G}}\nagents: {agent-01: {host: kali, token_env: T}}",
		"owner twice":         "github: {owners: {acme-org: G, Acme-Org: H}}\nagents: {agent-01: {host: kali, token_env: T}}",
		"owner token inline":  "github: {owners: {acme-org: gho_abc}}\nagents: {agent-01: {host: kali, token_env: T}}",
		"owner no token":      "github: {owners: {acme-org: ''}}\nagents: {agent-01: {host: kali, token_env: T}}",
		"packages inline":     "github: {packages_token_env: gho_abc}\nagents: {agent-01: {host: kali, token_env: T}}",
		"console host":        "console: {host: nowhere, fleetd_url: 'http://f:7879', token_env: C}\nagents: {agent-01: {host: kali, token_env: T}}",
		"console token":       "console: {host: kali, fleetd_url: 'http://f:7879', token_env: 'sk-abc'}\nagents: {agent-01: {host: kali, token_env: T}}",
		"console is full":     "console: {host: kali, fleetd_url: 'http://f:7879', token_env: FLEET_TOKEN}\nagents: {agent-01: {host: kali, token_env: T}}",
		"console is relay":    "console: {host: kali, fleetd_url: 'http://f:7879', token_env: FLEET_RELAY_TOKEN}\nagents: {agent-01: {host: kali, token_env: T}}",
		"console url":         "console: {host: kali, fleetd_url: 'f:7879', token_env: C}\nagents: {agent-01: {host: kali, token_env: T}}",
		"tailnet no port":     "tailnet: {listen: 100.64.0.1}\nagents: {agent-01: {host: kali, token_env: T}}",
		"tailnet bare *":      "tailnet: {listen: '100.64.0.1:7878', members: ['*']}\nagents: {agent-01: {host: kali, token_env: T}}",
		"tailnet any domain":  "tailnet: {listen: '100.64.0.1:7878', members: ['*@*']}\nagents: {agent-01: {host: kali, token_env: T}}",
		"tailnet not a login": "tailnet: {listen: '100.64.0.1:7878', operators: [op]}\nagents: {agent-01: {host: kali, token_env: T}}",
		"hide absolute":       "hide_from_agents: [/etc]\nagents: {agent-01: {host: kali, token_env: T}}",
		"hide outside code":   "hide_from_agents: [../.ssh]\nagents: {agent-01: {host: kali, token_env: T}}",
		"hide all of code":    "hide_from_agents: [.]\nagents: {agent-01: {host: kali, token_env: T}}",
		"orchestrator @":      "orchestrator: {host: kali, token_env: O, name: 'a@b.c'}\nagents: {agent-01: {host: kali, token_env: T}}",
	}
	for name, body := range cases {
		src := "home: /h\nhosts: {kali: {exec: [docker]}}\norchestrator: {host: kali, token_env: O}\n" + body
		if name == "no exec" || name == "orchestrator host" || name == "name clash" || name == "orchestrator @" {
			src = "home: /h\n" + body
			if name != "no exec" {
				src = "home: /h\nhosts: {kali: {exec: [docker]}}\n" + body
			}
		}
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: want an error for\n%s", name, src)
		} else if strings.Contains(err.Error(), "sk-ant") {
			t.Errorf("%s: error echoes a secret: %v", name, err)
		}
	}
}

// The shipped example must always load.
func TestTheShippedExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../../packaging/fleet/fleet.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AgentsOn("kali")) != 3 || len(c.AgentsOn("pc2")) != 5 {
		t.Errorf("the example is the plan's 3/5 split: %v / %v", c.AgentsOn("kali"), c.AgentsOn("pc2"))
	}
}

// An orchestrator that runs natively on fleetd's host uses that host's own
// Claude Code login, so it needs no token of its own.
func TestANativeOrchestratorNeedsNoToken(t *testing.T) {
	c, err := Parse([]byte("home: /h\nhosts: {kali: {exec: [docker]}, pc2: {exec: [docker, --context, pc2]}}\n" +
		"orchestrator: {host: kali, native: true}\nagents: {agent-01: {host: pc2, token_env: T}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Orchestrator.Native {
		t.Fatal("native not read")
	}
	if got := c.AgentsOn("kali"); len(got) != 0 {
		t.Errorf("kali agents = %v", got)
	}
}

func TestConsoleSectionFillsDefaults(t *testing.T) {
	c, err := Parse([]byte("home: /h\nhosts: {pc2: {exec: [docker]}}\norchestrator: {host: pc2, token_env: O}\n" +
		"console: {host: pc2, fleetd_url: 'http://f:7879', token_env: C, repos: [/h/code/r]}\n" +
		"agents: {agent-01: {host: pc2, token_env: T}}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Console.Listen != "127.0.0.1:7880" || c.Console.Dir != "~/.config/fleet-console" {
		t.Fatalf("console defaults = %+v", c.Console)
	}
}

func TestNoConsoleSectionIsNil(t *testing.T) {
	c, err := Parse([]byte("home: /h\nhosts: {pc2: {exec: [docker]}}\norchestrator: {host: pc2, token_env: O}\nagents: {agent-01: {host: pc2, token_env: T}}"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Console != nil {
		t.Fatalf("console = %+v, want nil", c.Console)
	}
}

// An owner's token reaches agents under a name the image's fleet-gh-token
// derives the same way (upper case, '-' as '_').
func TestGitHubOwners(t *testing.T) {
	c, err := Parse([]byte("home: /h\nhosts: {kali: {exec: [docker]}}\norchestrator: {host: kali, token_env: O}\n" +
		"github: {token_env: G, owners: {zeta: Z, acme-org: A}, packages_token_env: A}\nagents: {agent-01: {host: kali, token_env: T}}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.GitHub.OwnerNames(), " "); got != "acme-org zeta" {
		t.Errorf("OwnerNames = %q", got)
	}
	if got := OwnerTokenVar("acme-org"); got != "GH_TOKEN_OWNER_ACME_ORG" {
		t.Errorf("OwnerTokenVar = %q", got)
	}
}

// A tailnet login is an operator, a member, or nobody; *@domain is every
// login of that domain and no other.
func TestTailnetRoles(t *testing.T) {
	c, err := Parse([]byte(example + `
tailnet:
  listen: 100.64.0.1:7878
  operators: [Op@Example.com]
  members: ["*@example.com", guest@other.org]
`))
	if err != nil {
		t.Fatal(err)
	}
	tn := c.Tailnet
	if tn.MemberOpen != 2 || tn.Socket != "/var/run/tailscale/tailscaled.sock" || tn.Dist != "/home/op/.karmax/fleet/dist" {
		t.Errorf("defaults: %+v", tn)
	}
	for login, want := range map[string]string{
		"op@example.com":    "operator",
		"ram@example.com":   "member",
		"guest@other.org":   "member",
		"someone@other.org": "",
		"x@evilexample.com": "",
		"tagged-devices":    "",
		"":                  "",
	} {
		if got := tn.Role(login); got != want {
			t.Errorf("Role(%q) = %q, want %q", login, got, want)
		}
	}
	var none *Tailnet
	if none.Role("op@example.com") != "" {
		t.Error("no tailnet section: nobody")
	}
}
