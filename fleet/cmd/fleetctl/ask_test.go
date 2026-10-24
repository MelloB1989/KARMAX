package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestSessionNameIsTheFirstOfSessionContainerClaudeOperator(t *testing.T) {
	set := func(session, name, agent, cc string) {
		t.Setenv("FLEET_SESSION", session)
		t.Setenv("FLEET_NAME", name)
		t.Setenv("FLEET_AGENT", agent)
		t.Setenv("CLAUDE_CODE_SESSION_ID", cc)
	}
	for _, c := range []struct {
		session, name, agent, cc, want string
	}{
		{"review-1", "agent-03", "agent-03", "CE458EAB-ffe8-41fd", "review-1"},
		{"", "agent-03", "agent-03", "ce458eab-ffe8-41fd", "agent-03"},
		{"", "", "agent-05", "ce458eab-ffe8-41fd", "agent-05"},
		{"", "", "", "ce458eab-ffe8-41fd-a20b-d6499e5d90ea", "cc-ce458eab"},
		{"", "", "", "", "operator"},
	} {
		set(c.session, c.name, c.agent, c.cc)
		if got := sessionName(); got != c.want {
			t.Errorf("%+v: sessionName = %q, want %q", c, got, c.want)
		}
	}
}

func TestRunAndFollowupAndAnswerSpeakAsTheSessionAndFromWins(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/run": runReply, "/v1/tasks/T1/followup": `{"ok":true}`, "/v1/tasks/T1/answer": `{"ok":true,"answered":true}`}}
	ctlEnv(t, s)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ce458eab-ffe8-41fd")
	if _, err := ctlRun("run", "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := ctlRun("followup", "T1", "more"); err != nil {
		t.Fatal(err)
	}
	if _, err := ctlRun("answer", "T1", "--from", "review-2", "the", "first"); err != nil {
		t.Fatal(err)
	}
	var froms []string
	for _, b := range s.body {
		var in map[string]string
		_ = json.Unmarshal([]byte(b), &in)
		froms = append(froms, in["from"])
	}
	if strings.Join(froms, ",") != "cc-ce458eab,cc-ce458eab,review-2" {
		t.Fatalf("senders = %v", froms)
	}
}

func TestAskPrintsTheAnswerAndSendsTheQuestionAsTheAgent(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/ask": `{"task":"T1","answer":"use the second one"}`}}
	ctlEnv(t, s)
	t.Setenv("FLEET_AGENT", "agent-03")
	out, err := ctlRun("ask", "which", "one?", "--timeout", "5m")
	if err != nil || strings.TrimSpace(out) != "use the second one" {
		t.Fatalf("out = %q, %v", out, err)
	}
	var in map[string]string
	_ = json.Unmarshal([]byte(s.body[0]), &in)
	if in["from"] != "agent-03" || in["text"] != "which one?" || in["timeout"] != "5m0s" {
		t.Fatalf("sent %v", in)
	}
}

func TestAskThatTimesOutSaysSoAndExits4(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/ask": `{"task":"T1","timed_out":true}`}}
	ctlEnv(t, s)
	t.Setenv("FLEET_AGENT", "agent-03")
	out, err := ctlRun("ask", "anyone?", "--timeout", "30m")
	if exitOf(err) != 4 || !strings.Contains(out, "no answer to your question in 30m0s") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestAnswerSaysWhetherItReachedABlockedAsk(t *testing.T) {
	for reply, want := range map[string]string{
		`{"ok":true,"answered":true}`:                                           "answered",
		`{"ok":true,"answered":false}`:                                          "sent as a follow-up",
		`{"ok":true,"queued":true,"why":"queued: agent-01 cannot take it now"}`: "queued: agent-01",
	} {
		s := &serve{out: map[string]string{"/v1/tasks/T1/answer": reply}}
		ctlEnv(t, s)
		out, err := ctlRun("answer", "T1", "yes")
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: out = %q, %v", reply, out, err)
		}
	}
}

func TestTellCarriesItsKind(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/tell": `{"ok":true}`}}
	ctlEnv(t, s)
	t.Setenv("FLEET_AGENT", "agent-03")
	if _, err := ctlRun("tell", "operator", "halfway", "--kind", "progress"); err != nil {
		t.Fatal(err)
	}
	var in map[string]string
	_ = json.Unmarshal([]byte(s.body[0]), &in)
	if in["kind"] != "progress" || in["from"] != "agent-03" {
		t.Fatalf("sent %v", in)
	}
}

func TestWaitPrintsTheKindAndTheNextCommand(t *testing.T) {
	msg := `{"reason":"message","message":{"id":7,"from":"agent-02","kind":"question","text":"which?"}}`
	s := &serve{out: map[string]string{"/v1/wait": msg}}
	ctlEnv(t, s)
	out, err := ctlRun("wait", "--task", "T1")
	if err != nil || !strings.Contains(out, "agent-02 [question]: which?") || !strings.Contains(out, "next: fleetctl wait --task T1 --after 7") {
		t.Fatalf("out = %q, %v", out, err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ce458eab-ffe8")
	out, err = ctlRun("wait", "--task", "T1", "--new")
	if err != nil || !strings.Contains(out, "next: fleetctl wait --task T1 --new --as cc-ce458eab") {
		t.Fatalf("out = %q, %v", out, err)
	}
	u, _ := url.Parse(s.reqs[len(s.reqs)-1][len("GET "):])
	if u.Query().Get("new") != "cc-ce458eab" {
		t.Fatalf("wait --new asked %v", s.reqs[len(s.reqs)-1])
	}
}

func TestWaitNewNeedsATask(t *testing.T) {
	ctlEnv(t, &serve{})
	if _, err := ctlRun("wait", "agent-02", "--new"); err == nil {
		t.Fatal("wait --new without a task")
	}
}

func TestInboxReadsTheSessionsOwnInbox(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/inbox": `[{"id":4,"from":"agent-02","kind":"progress","text":"half way"}]`}}
	ctlEnv(t, s)
	out, err := ctlRun("inbox", "--as", "review-1")
	if err != nil || !strings.Contains(out, "agent-02 [progress]: half way") || !strings.Contains(s.reqs[0], "to=review-1") {
		t.Fatalf("out = %q, %v, reqs %v", out, err, s.reqs)
	}
	if _, err := ctlRun("inbox"); err != nil || strings.Contains(s.reqs[1], "to=") {
		t.Fatalf("the operator's inbox asked %v", s.reqs[1])
	}
}
