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
		"hide absolute":       "hide_from_agents: [/etc]\nagents: {agent-01: {host: kali, token_env: T}}",
		"hide outside code":   "hide_from_agents: [../.ssh]\nagents: {agent-01: {host: kali, token_env: T}}",
		"hide all of code":    "hide_from_agents: [.]\nagents: {agent-01: {host: kali, token_env: T}}",
		"orchestrator @":      "orchestrator: {host: kali, token_env: O, name: 'a@b.c'}\nagents: {agent-01: {host: kali, token_env: T}}",
		"memory no url":       "memory: {key_env: GITLOOM_KEY, namespace: mira}\nagents: {agent-01: {host: kali, token_env: T}}",
		"memory bad url":      "memory: {url: '127.0.0.1:8787', key_env: GITLOOM_KEY, namespace: mira}\nagents: {agent-01: {host: kali, token_env: T}}",
		"memory key inline":   "memory: {url: 'http://127.0.0.1:8787', key_env: 'gl_abc-123', namespace: mira}\nagents: {agent-01: {host: kali, token_env: T}}",
		"memory no namespace": "memory: {url: 'http://127.0.0.1:8787', key_env: GITLOOM_KEY}\nagents: {agent-01: {host: kali, token_env: T}}",
		"memory is full":      "memory: {url: 'http://127.0.0.1:8787', key_env: FLEET_TOKEN, namespace: mira}\nagents: {agent-01: {host: kali, token_env: T}}",
		"named like a slot":   "agents: {'agent-01:2': {host: kali, token_env: T}}",
		"orchestrator slot":   "orchestrator: {host: kali, token_env: O, name: 'agent-01:2'}\nagents: {agent-01: {host: kali, token_env: T}}",
		"no slots":            "slots: -1\nagents: {agent-01: {host: kali, token_env: T}}",
		"too many slots":      "agents: {agent-01: {host: kali, token_env: T, slots: 9}}",
	}
	for name, body := range cases {
		src := "home: /h\nhosts: {kali: {exec: [docker]}}\norchestrator: {host: kali, token_env: O}\n" + body
		if name == "no exec" || name == "orchestrator host" || name == "name clash" || name == "orchestrator @" || name == "orchestrator slot" {
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

func TestAgentsExpandIntoOneWorkerPerSlot(t *testing.T) {
	c, err := Parse([]byte(example + "\nslots: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Parse([]byte(strings.Replace(example, "token_env: CLAUDE_TOKEN_AGENT_04}", "token_env: CLAUDE_TOKEN_AGENT_04, slots: 1}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.WorkerNames(); !slices.Equal(got, []string{"agent-01", "agent-01:2", "agent-01:3", "agent-04", "agent-04:2", "agent-04:3"}) {
		t.Errorf("slots: 3 → workers %v", got)
	}
	// The default is two; an agent's own slots overrides the fleet's.
	if got := c2.WorkerNames(); !slices.Equal(got, []string{"agent-01", "agent-01:2", "agent-04"}) {
		t.Errorf("default slots with agent-04 at 1 → workers %v", got)
	}
	w, ok := c.Worker("agent-04:2")
	if !ok || w.Agent != "agent-04" || w.Slot != 2 || w.Host != "pc2" || w.Container != "agent-04" || w.Account != "agent-04" ||
		!slices.Equal(w.Models, []string{"sonnet"}) {
		t.Errorf("Worker(agent-04:2) = %+v, %v", w, ok)
	}
	if w, ok := c.Worker("agent-01"); !ok || w.Slot != 1 || w.Agent != "agent-01" {
		t.Errorf("slot 1 keeps the agent's name: %+v, %v", w, ok)
	}
	for _, bad := range []string{"agent-01:1", "agent-01:4", "agent-01:02", "agent-09:2", "agent-01:x", "nobody"} {
		if _, ok := c.Worker(bad); ok {
			t.Errorf("Worker(%q) exists", bad)
		}
	}
	if got := c.Siblings("agent-01:2"); !slices.Equal(got, []string{"agent-01", "agent-01:3"}) {
		t.Errorf("Siblings(agent-01:2) = %v", got)
	}
	if got := c2.Siblings("agent-04"); len(got) != 0 {
		t.Errorf("a one-slot agent has siblings %v", got)
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

func TestTheMemorySectionIsOffWhenAbsent(t *testing.T) {
	base := "home: /h\nhosts: {pc2: {exec: [docker]}}\norchestrator: {host: pc2, token_env: O}\nagents: {agent-01: {host: pc2, token_env: T}}\n"
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.Memory != nil {
		t.Fatalf("memory = %+v, want nil", c.Memory)
	}
	c, err = Parse([]byte(base + "memory: {url: 'http://127.0.0.1:8787', key_env: GITLOOM_KEY, namespace: mira}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m := c.Memory; m == nil || m.URL != "http://127.0.0.1:8787" || m.KeyEnv != "GITLOOM_KEY" || m.Namespace != "mira" {
		t.Fatalf("memory = %+v", c.Memory)
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

func TestTailnetDefaults(t *testing.T) {
	c, err := Parse([]byte(example + `
tailnet:
  listen: 100.64.0.1:7878
`))
	if err != nil {
		t.Fatal(err)
	}
	tn := c.Tailnet
	if tn.GuestOpen != 2 || tn.Socket != "/var/run/tailscale/tailscaled.sock" || tn.Dist != "/home/op/.karmax/fleet/dist" {
		t.Errorf("defaults: %+v", tn)
	}
}

// Who may use the fleet is a device's tag now: a fleet.yaml that still
// lists logins fails to load, and says what replaced them.
func TestTheOldTailnetKeysFailToLoad(t *testing.T) {
	for _, k := range []string{"operators: [op@example.com]", `members: ["*@example.com"]`, "member_open: 3", "operators: []"} {
		_, err := Parse([]byte(example + "\ntailnet:\n  listen: 100.64.0.1:7878\n  " + k + "\n"))
		if err == nil || !strings.Contains(err.Error(), "tailnet.operators/members are gone: tag devices with `fleetctl access`") {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestAToolIsClassedByItsEntryElseByItsName(t *testing.T) {
	k := Karmax{Tools: map[string]string{"slack.channels": "read", "google.*": "write", "google.drive.*": "read",
		"whatsapp.read": "deny"}}
	for name, want := range map[string]string{
		"google.mail.search":    "write", // google.* outranks the name
		"google.drive.search":   "read",  // the longer prefix wins
		"slack.channels":        "read",
		"slack.post":            "write",
		"whatsapp.read":         "deny",
		"whatsapp.monitored":    "read",
		"notion.query_database": "read",
		"memory.retrieve":       "read",
		"comms.send":            "write",
		"github.issues":         "write",
		"shell.exec":            "deny",
		"file.read":             "deny",
		"http.request":          "deny",
		"harness.list":          "deny",
		"task.list":             "deny",
		"scheduler.add":         "deny",
		"proposal.decide":       "deny",
		"orgops.list":           "deny",
		"memory.ingest":         "deny",
		"memory.forget":         "deny",
		"vault.get_secret":      "deny",
		"aws.credentials_list":  "deny",
		"fleet.status":          "deny",
		"../shell.exec":         "deny",
		"x?agent=root":          "deny",
	} {
		if got := k.ToolClass(name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}

func TestNoToolsEntryOpensAnAlwaysDeniedTool(t *testing.T) {
	for _, entry := range []string{"shell.exec: read", "memory.ingest: write", "file.read: read", "google.mail.send: maybe", "'../x': read"} {
		if _, err := Parse([]byte(example + "karmax:\n  tools: {" + entry + "}\n")); err == nil {
			t.Errorf("tools %s was accepted", entry)
		}
	}
	c, err := Parse([]byte(example + "karmax:\n  tools: {shell.exec: deny, 'google.*': read}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Karmax.ToolClass("shell.exec") != "deny" || c.Karmax.ToolClass("google.mail.send") != "read" {
		t.Fatalf("tools = %v", c.Karmax.Tools)
	}
}

func TestToolsThatRunCodeOrStartAgentsAreAlwaysDenied(t *testing.T) {
	k := Karmax{Tools: map[string]string{"sandbox.*": "read", "saw.*": "write"}}
	for _, name := range []string{"claude_code.call", "codex.call", "subagent.spawn", "sandbox.start", "orchestrator.send",
		"saw.run", "job.run", "docker.exec", "worker.spawn"} {
		if got := k.ToolClass(name); got != ToolDeny {
			t.Errorf("%s = %s, want deny", name, got)
		}
	}
	// What only looks like them is not.
	for name, want := range map[string]string{"sandbox.status": "read", "google.calendar.list": "read", "instagram.call": "read",
		"run.report": "write", "spawn_report.send": "write"} {
		if got := k.ToolClass(name); got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	if _, err := Parse([]byte(example + "karmax:\n  tools: {codex.call: write}\n")); err == nil {
		t.Error("an entry opened codex.call")
	}
}

func TestKnownReadsWithoutAReadVerbAreFree(t *testing.T) {
	var k Karmax
	for _, name := range []string{"slack.channels", "slack.user", "instagram.inbox", "instagram.reads", "instagram.commenters",
		"google.chat.spaces", "keka.departments", "keka.employees", "keka.leave", "activity.recent", "cost.report",
		"karmax.capabilities", "tools.load"} {
		if got := k.ToolClass(name); got != ToolRead {
			t.Errorf("%s = %s, want read", name, got)
		}
	}
	// Their writes stay writes, and an entry still outranks the list.
	for _, name := range []string{"slack.post", "instagram.send_dm", "instagram.reply_comment", "google.chat.post", "keka.update"} {
		if got := k.ToolClass(name); got != ToolWrite {
			t.Errorf("%s = %s, want write", name, got)
		}
	}
	if got := (Karmax{Tools: map[string]string{"slack.*": "write"}}).ToolClass("slack.channels"); got != ToolWrite {
		t.Errorf("slack.channels under slack.*: write = %s", got)
	}
}

func TestWacliUnderscoreNamesAreClassedByTheirWords(t *testing.T) {
	var k Karmax
	reads := []string{
		"whatsapp_get_chat", "whatsapp_get_contact", "whatsapp_list_calls", "whatsapp_list_chats",
		"whatsapp_list_contacts", "whatsapp_list_groups", "whatsapp_list_triggers", "whatsapp_list_webhooks",
		"whatsapp_search_messages",
		// Nouns only, reads by their descriptions.
		"whatsapp_status", "whatsapp_logs", "whatsapp_message_receipts", "whatsapp_webhook_deliveries",
		"whatsapp_group_info", "whatsapp_call_status", "whatsapp_check_numbers", "whatsapp_resolve",
		"whatsapp_download_media",
	}
	writes := []string{
		"app.push", "calendar.add", "call.start", "comms.escalate", "comms.send", "contact.add", "contact.update",
		"google", "notify.send", "profile.update", "propose", "recipe.write", "reminder.add", "review.resolve",
		"self.remind", "whatsapp.monitor",
		"whatsapp_answer_call", "whatsapp_bulk_send", "whatsapp_create_group", "whatsapp_create_trigger",
		"whatsapp_create_webhook", "whatsapp_delete_message", "whatsapp_delete_trigger", "whatsapp_delete_webhook",
		"whatsapp_edit_message", "whatsapp_end_call", "whatsapp_join_group", "whatsapp_leave_group",
		"whatsapp_place_call", "whatsapp_reject_call", "whatsapp_rename_group", "whatsapp_replay_webhook_delivery",
		"whatsapp_send_message", "whatsapp_send_story", "whatsapp_set_chat_locked", "whatsapp_set_dnd",
		"whatsapp_set_presence", "whatsapp_set_trigger_enabled", "whatsapp_set_typing", "whatsapp_test_trigger",
		"whatsapp_test_webhook", "whatsapp_sync",
		// It adds and removes people, and can reset a link.
		"whatsapp_group_participants", "whatsapp_group_invite_link",
		// Made up: a read verb does not save a name with a write verb in it.
		"whatsapp_set_foo", "whatsapp_list_delete", "slack.history_delete", "_", "whatsapp_",
	}
	for _, name := range reads {
		if got := k.ToolClass(name); got != ToolRead {
			t.Errorf("%s = %s, want read", name, got)
		}
	}
	for _, name := range writes {
		if got := k.ToolClass(name); got != ToolWrite {
			t.Errorf("%s = %s, want write", name, got)
		}
	}
	// The always-denied list still wins over a read verb.
	for _, name := range []string{"whatsapp_list_secrets", "fleet_list", "job_list.run", "file.list_dir"} {
		if got := (Karmax{Tools: map[string]string{"whatsapp_*": "read"}}).ToolClass(name); got != ToolDeny {
			t.Errorf("%s = %s, want deny", name, got)
		}
	}
}

func TestFilesCapDefaultsToTwoGiBAndTakesYAML(t *testing.T) {
	c, err := Parse([]byte(example))
	if err != nil || c.Files.MaxBytes != 2<<30 {
		t.Fatalf("default = %d, %v", c.Files.MaxBytes, err)
	}
	c, err = Parse([]byte(example + "files: {max_bytes: 1048576}\n"))
	if err != nil || c.Files.MaxBytes != 1<<20 {
		t.Fatalf("set = %d, %v", c.Files.MaxBytes, err)
	}
}

func TestASandboxAgentsWorkersAreSandboxes(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(example, "models: [sonnet, opus]}", "models: [sonnet, opus], sandbox: true}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	w, ok := c.Worker("agent-01")
	if !ok || !w.Sandbox || !c.Workers()[0].Sandbox {
		t.Fatalf("worker = %+v (%v)", w, ok)
	}
}

// A sandbox runs one session in a small container unless fleet.yaml says
// otherwise, on the fixed subnet its network rules name.
func TestASandboxDefaultsToOneSlotAndASmallContainer(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(example, "models: [sonnet, opus]}", "models: [sonnet, opus], sandbox: true}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if a := c.Agents["agent-01"]; a.Slots != 1 || a.Memory != "1536m" {
		t.Errorf("sandbox = %+v, want slots 1 and memory 1536m", a)
	}
	if a := c.Agents["agent-04"]; a.Slots != 2 || a.Memory != "" {
		t.Errorf("ordinary agent = %+v: the sandbox defaults are the sandbox's only", a)
	}
	if c.SandboxNet.Subnet != "172.30.250.0/24" {
		t.Errorf("subnet = %q", c.SandboxNet.Subnet)
	}
	c, err = Parse([]byte(strings.Replace(example, "models: [sonnet, opus]}", "models: [sonnet, opus], sandbox: true, slots: 2, memory: 3g}", 1) +
		"sandbox: {subnet: 192.0.2.0/24}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a := c.Agents["agent-01"]; a.Slots != 2 || a.Memory != "3g" || c.SandboxNet.Subnet != "192.0.2.0/24" {
		t.Errorf("set = %+v, subnet %q", a, c.SandboxNet.Subnet)
	}
	if _, err := Parse([]byte(example + "sandbox: {subnet: 192.0.2.0}\n")); err == nil || !strings.Contains(err.Error(), "subnet") {
		t.Errorf("a subnet that is no CIDR: %v", err)
	}
}
