package guide

import (
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

func TestTextSaysHowToUseTheKARMAXGateway(t *testing.T) {
	for _, want := range []string{"fleetctl karmax tools", "fleetctl karmax call", "--reason", "fleetctl approvals",
		"fleetctl approve", "fleetctl deny", "POST /v1/karmax/call", "/v1/approvals/{id}/approve", "never sees the token"} {
		if !strings.Contains(Text(), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}
