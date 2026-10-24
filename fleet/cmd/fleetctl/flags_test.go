package main

import (
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestFlagsComeBeforeBetweenOrAfterThePositionals(t *testing.T) {
	for _, args := range [][]string{
		{"--limit", "2", "--json", "fleet agents"},
		{"fleet agents", "--limit", "2", "--json"},
		{"fleet", "--limit=2", "agents", "--json"},
	} {
		var limit int
		var asJSON bool
		_, pos, err := flags("t", args, func(fs *flag.FlagSet) {
			fs.IntVar(&limit, "limit", 0, "")
			fs.BoolVar(&asJSON, "json", false, "")
		})
		if err != nil || limit != 2 || !asJSON || len(pos) == 0 {
			t.Errorf("%q: limit %d json %v pos %q, %v", args, limit, asJSON, pos, err)
		}
	}
}

func TestDashDashEndsTheFlagsAndWhatFollowsIsPositional(t *testing.T) {
	var asJSON bool
	_, pos, err := flags("t", []string{"--json", "T1", "--", "--not-a-flag", "-x", "--json"}, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "")
	})
	if err != nil || !asJSON {
		t.Fatalf("json %v, %v", asJSON, err)
	}
	if want := []string{"T1", "--not-a-flag", "-x", "--json"}; !reflect.DeepEqual(pos, want) {
		t.Errorf("pos = %q, want %q", pos, want)
	}
	// A lone dash is positional, and an unknown flag is still an error.
	if _, pos, err := flags("t", []string{"-", "--x"}, func(*flag.FlagSet) {}); err == nil {
		t.Errorf("unknown flag after a positional: pos %q, no error", pos)
	}
	if _, pos, _ := flags("t", []string{"-"}, func(*flag.FlagSet) {}); !reflect.DeepEqual(pos, []string{"-"}) {
		t.Errorf("lone dash: pos = %q", pos)
	}
}

func TestTellTakesItsFlagsAfterTheText(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "tell", "karmax", "all done", "--from", "agent-01", "--kind", "progress"); err != nil {
		t.Fatal(err)
	}
	in := reqBody(t, f.reqs[0])
	if in["to"] != "karmax" || in["text"] != "all done" || in["from"] != "agent-01" || in["kind"] != "progress" {
		t.Errorf("request = %s", f.reqs[0])
	}
}

func TestTellAfterDashDashSendsTextThatStartsWithADash(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "tell", "--from", "agent-01", "karmax", "--", "--kind is not a flag here"); err != nil {
		t.Fatal(err)
	}
	if in := reqBody(t, f.reqs[0]); in["text"] != "--kind is not a flag here" || in["kind"] != nil {
		t.Errorf("request = %s", f.reqs[0])
	}
}

func TestMemoryRecallTakesLimitAndJSONAfterTheQuery(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/memory/recall": recallReply}}
	out, err := fleetctl(t, f, "memory", "recall", "fleet agents", "--limit", "2", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if f.reqs[0] != "GET /v1/memory/recall?limit=2&q=fleet+agents " || !strings.Contains(out, `"path": "people/ram.md"`) {
		t.Errorf("request %q, out %s", f.reqs[0], out)
	}
}

func TestRunTakesFlagsAfterTheBriefAndKeepsAStdinBrief(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/run": runReply}}
	if _, err := ctl(t, s, "", "run", "fix", "the bug", "--repo", "KARMAX", "--from", "karmax"); err != nil {
		t.Fatal(err)
	}
	if in := sentBrief(t, s); in["brief"] != "fix the bug" || in["repo"] != "KARMAX" || in["from"] != "karmax" {
		t.Errorf("request = %v", in)
	}
	s = &serve{out: map[string]string{"/v1/run": runReply}}
	if _, err := ctl(t, s, "from stdin\n", "run", "-", "--repo", "KARMAX"); err != nil {
		t.Fatal(err)
	}
	if in := sentBrief(t, s); in["brief"] != "from stdin\n" || in["repo"] != "KARMAX" {
		t.Errorf("stdin brief = %v", in)
	}
}

func TestFollowupAndCancelTakeFlagsAfterThePositionals(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "followup", "T1", "use the other repo", "--from", "karmax"); err != nil {
		t.Fatal(err)
	}
	if in := reqBody(t, f.reqs[0]); in["text"] != "use the other repo" || in["from"] != "karmax" {
		t.Errorf("followup = %s", f.reqs[0])
	}
	if _, err := fleetctl(t, f, "cancel", "T1", "--reason", "wrong repo"); err != nil {
		t.Fatal(err)
	}
	if in := reqBody(t, f.reqs[1]); in["reason"] != "wrong repo" {
		t.Errorf("cancel = %s", f.reqs[1])
	}
}

func TestCommandsThatTookRawArgsTakeFlagsToo(t *testing.T) {
	f := &fakeFleetd{}
	if _, err := fleetctl(t, f, "enable", "agent-03"); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetctl(t, f, "prompt", "agent-03", "hello", "there"); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetctl(t, f, "secret", "rm", "TOKEN_A", "--"); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetctl(t, f, "archive", "show", "a1", "--"); err != nil {
		t.Fatal(err)
	}
	if in := reqBody(t, f.reqs[1]); in["agent"] != "agent-03" || in["text"] != "hello there" {
		t.Errorf("prompt = %s", f.reqs[1])
	}
	if f.reqs[2][:len("DELETE /v1/secrets/TOKEN_A")] != "DELETE /v1/secrets/TOKEN_A" || !strings.HasPrefix(f.reqs[3], "GET /v1/archives/a1 ") {
		t.Errorf("requests = %q", f.reqs)
	}
	if _, err := fleetctl(t, f, "enable", "--bogus", "agent-03"); err == nil {
		t.Error("an unknown flag must be refused")
	}
}
