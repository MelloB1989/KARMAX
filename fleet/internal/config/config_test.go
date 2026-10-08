package config

import (
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
	}
	for name, body := range cases {
		src := "home: /h\nhosts: {kali: {exec: [docker]}}\norchestrator: {host: kali, token_env: O}\n" + body
		if name == "no exec" || name == "orchestrator host" || name == "name clash" {
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
