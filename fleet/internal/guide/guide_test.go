package guide

import (
	"os"
	"strings"
	"testing"
)

func TestSkillIsFrontmatterThenTheGuide(t *testing.T) {
	s := Skill()
	if !strings.HasPrefix(s, "---\nname: fleet\ndescription: ") {
		t.Fatalf("skill starts %q", s[:min(len(s), 60)])
	}
	head, body, ok := strings.Cut(strings.TrimPrefix(s, "---\n"), "\n---\n")
	if !ok || strings.Count(head, "\n") != 1 {
		t.Fatalf("frontmatter is not name + one-line description:\n%s", head)
	}
	for _, trigger := range []string{"fleet", "agents", "fan-out", "fleetctl"} {
		if !strings.Contains(head, trigger) {
			t.Errorf("description lacks %q", trigger)
		}
	}
	if !strings.Contains(body, Text()) {
		t.Error("skill body does not contain the guide")
	}
}

func TestTextMentionsEveryCommand(t *testing.T) {
	for _, cmd := range []string{"run", "pick", "result", "wait", "followup", "cancel", "done", "guide", "skill install",
		"ls", "peek", "tasks --open", "quota", "--queue", "--wait", "--repo", "--task"} {
		if !strings.Contains(Text(), "fleetctl "+cmd) && !strings.Contains(Text(), cmd) {
			t.Errorf("guide never mentions %q", cmd)
		}
	}
	for _, cmd := range []string{"run", "pick", "result", "wait", "followup", "cancel", "done", "guide", "skill install"} {
		if !strings.Contains(Text(), "fleetctl "+cmd) {
			t.Errorf("guide has no `fleetctl %s` example", cmd)
		}
	}
	for _, route := range []string{"POST /v1/run", "GET /v1/pick", "GET /v1/tasks/", "GET /v1/wait",
		"/v1/tasks/{id}/followup", "/v1/tasks/{id}/cancel", "POST /v1/done", "GET /v1/guide"} {
		if !strings.Contains(Text(), route) {
			t.Errorf("guide lacks route %q", route)
		}
	}
}

func TestTextStatesTheRulesAndExitCodes(t *testing.T) {
	for _, want := range []string{"untrusted", "&&", "secret", "rebase", "exit 5", "409"} {
		if !strings.Contains(strings.ToLower(Text()), strings.ToLower(want)) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if n := strings.Count(Text(), "\n"); n > 250 {
		t.Errorf("guide is %d lines, want <= 250", n)
	}
}

func TestTextNamesNoOperator(t *testing.T) {
	for _, bad := range []string{"/home/", "192.168.", "sk-ant"} {
		if strings.Contains(Text(), bad) {
			t.Errorf("guide contains %q; the repo is public", bad)
		}
	}
}

func TestTheMemorySkillSaysWhenToRecallHowToSuggestAndNoSecrets(t *testing.T) {
	s := MemorySkill()
	if !strings.HasPrefix(s, "---\nname: fleet-memory\ndescription: ") {
		t.Fatalf("skill starts %q", s[:min(len(s), 60)])
	}
	head, _, ok := strings.Cut(strings.TrimPrefix(s, "---\n"), "\n---\n")
	if !ok || strings.Count(head, "\n") != 1 {
		t.Fatalf("frontmatter is not name + one-line description:\n%s", head)
	}
	for _, want := range []string{"fleetctl memory recall", "fleetctl memory suggest", "--why", "memory_recall", "memory_suggest",
		"fleetctl mcp", "claude mcp add --scope user fleet -- fleetctl mcp", "secret", "orchestrator", "before"} {
		if !strings.Contains(s, want) {
			t.Errorf("memory skill lacks %q", want)
		}
	}
	for _, bad := range []string{"/home/", "192.168.", "sk-ant"} {
		if strings.Contains(s, bad) {
			t.Errorf("memory skill contains %q; the repo is public", bad)
		}
	}
}

func TestTextSaysHowToUseTheKARMAXGateway(t *testing.T) {
	for _, want := range []string{"fleetctl karmax tools", "fleetctl karmax call", "--reason", "fleetctl approvals",
		"fleetctl approve", "fleetctl deny", "POST /v1/karmax/call", "/v1/approvals/{id}/approve", "never sees the token",
		"karmax_tools", "karmax_call", "runs code or starts an agent"} {
		if !strings.Contains(Text(), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}

// The image ships the fleet-memory skill as a file; it must be the very text
// `fleetctl skill install` writes, so the two cannot drift. If this fails,
// copy `fleetctl skill install --print`'s fleet-memory part over the file.
func TestTheShippedMemorySkillIsWhatSkillInstallWrites(t *testing.T) {
	got, err := os.ReadFile("../../../packaging/fleet/skills/fleet-memory/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != MemorySkill() {
		t.Error("packaging/fleet/skills/fleet-memory/SKILL.md differs from guide.MemorySkill()")
	}
}

func TestTheShippedAgentSkillCoversEveryToolAndFailure(t *testing.T) {
	b, err := os.ReadFile("../../../packaging/fleet/skills/fleet-agent/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\nname: fleet-agent\ndescription: ") {
		t.Fatalf("skill starts %q", s[:min(len(s), 60)])
	}
	for _, want := range []string{"fleetctl tell", "--kind progress", "fleetctl ask", "exit 4", "/work/inbox/<task>/",
		"fleetctl karmax tools", "fleetctl karmax call", "--reason", "k:=5", "--json-args", "Reads are free", "Denied outright",
		"fleetctl memory recall", "fleetctl secret", "fleetctl whoami", "fleetctl model", "kbuild", "--no-verify", "403", "queued",
		"v2-files", "v2-secrets"} {
		if !strings.Contains(s, want) {
			t.Errorf("fleet-agent skill lacks %q", want)
		}
	}
	for _, bad := range []string{"192.168.", "sk-ant", "gho_"} {
		if strings.Contains(s, bad) {
			t.Errorf("fleet-agent skill contains %q; the repo is public", bad)
		}
	}
}
