package server

import (
	"strings"
	"testing"
)

func TestAskIsTheAgentsAloneAndWaitsForNothingElse(t *testing.T) {
	f, h := setup(t)
	for _, c := range []struct {
		token string
		want  int
	}{{"relay", 200}, {"full", 403}, {"console", 403}, {"", 401}} {
		if rec := do(t, h, "POST", "/v1/ask", c.token, `{"from":"agent-03","text":"which branch?","timeout":"5m"}`); rec.Code != c.want {
			t.Errorf("token %q: %d %s, want %d", c.token, rec.Code, rec.Body, c.want)
		}
	}
	rec := do(t, h, "POST", "/v1/ask", "relay", `{"from":"agent-03","text":"which branch?","timeout":"5m"}`)
	if !strings.Contains(rec.Body.String(), `"answer":"use the second"`) {
		t.Fatalf("reply = %s", rec.Body)
	}
	if got := strings.Join(f.calls, "|"); !strings.Contains(got, "ask agent-03 which branch? 5m0s") {
		t.Fatalf("calls = %s", got)
	}
	for _, bad := range []string{`{"from":"agent-03","text":" "}`, `{"text":"q"}`, `{"from":"agent-03","text":"q","timeout":"soon"}`, `{"from":"agent-03","text":"q","timeout":"-5m"}`} {
		if rec := do(t, h, "POST", "/v1/ask", "relay", bad); rec.Code != 400 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
}

func TestTellTakesAKindAndRefusesAnyOtherAnswerKind(t *testing.T) {
	f, h := setup(t)
	if rec := do(t, h, "POST", "/v1/tell", "relay", `{"from":"agent-03","to":"operator","text":"half way","kind":"progress"}`); rec.Code != 200 {
		t.Fatalf("progress: %d %s", rec.Code, rec.Body)
	}
	for _, kind := range []string{"answer", "gossip"} {
		if rec := do(t, h, "POST", "/v1/tell", "relay", `{"from":"agent-03","to":"operator","text":"x","kind":"`+kind+`"}`); rec.Code != 400 {
			t.Errorf("kind %s: %d", kind, rec.Code)
		}
	}
	if got := strings.Join(f.calls, "|"); !strings.Contains(got, "tell agent-03 operator half way [progress]") || strings.Contains(got, "gossip") {
		t.Fatalf("calls = %s", got)
	}
}

func TestAnswerIsForTheAssignerAnAgentMayOnlyAnswerItsOwnAssignments(t *testing.T) {
	f, h := setup(t)
	if rec := do(t, h, "POST", "/v1/tasks/T1/answer", "full", `{"from":"operator","text":"main"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"answered":true`) {
		t.Fatalf("full: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/tasks/T1/answer", "console", `{"text":"main"}`); rec.Code != 403 {
		t.Errorf("console: %d", rec.Code)
	}
	// T1's assigner is the operator: an agent cannot answer for it.
	if rec := do(t, h, "POST", "/v1/tasks/T1/answer", "relay", `{"from":"agent-05","text":"main"}`); rec.Code != 403 {
		t.Errorf("an agent answered the operator's task: %d", rec.Code)
	}
	// The operator's name is the task's assigner, but an agent may not speak as it.
	if rec := do(t, h, "POST", "/v1/tasks/T1/answer", "relay", `{"from":"operator","text":"main"}`); rec.Code != 403 {
		t.Errorf("an agent answered as the operator: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/v1/tasks/T1/answer", "relay", `{"text":"main"}`); rec.Code != 403 {
		t.Errorf("an agent answered as nobody: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/v1/tasks/T1/answer", "full", `{"text":" "}`); rec.Code != 400 {
		t.Errorf("empty: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/v1/tasks/T-missing/answer", "full", `{"text":"x"}`); rec.Code != 404 {
		t.Errorf("unknown task: %d", rec.Code)
	}
	if got := strings.Join(f.calls, "|"); !strings.Contains(got, "answer T1 operator main") {
		t.Fatalf("calls = %s", got)
	}
}

func TestInboxAndWaitNewTakeASessionsName(t *testing.T) {
	f, h := setup(t)
	do(t, h, "GET", "/v1/inbox?to=review-1", "full", "")
	do(t, h, "GET", "/v1/inbox", "full", "")
	if rec := do(t, h, "GET", "/v1/wait?task=T1&new=review-1&timeout=1s", "full", ""); rec.Code != 200 {
		t.Fatalf("wait new: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(f.calls, "|"); got != "inbox review-1|inbox operator|waitnew T1 review-1" {
		t.Fatalf("calls = %s", got)
	}
}

func TestAMemberNamesTheirOwnSessionsAndNeverSomeoneElses(t *testing.T) {
	f, h, _ := tailnetSetup(t)
	for _, from := range []string{"review", "operator", "", "ram@example.com/mine", "eve@example.com", "eve@example.com/theirs"} {
		if rec := tdo(t, h, "POST", "/v1/run", ramIP, "", `{"brief":"fix it","from":"`+from+`"}`); rec.Code != 200 {
			t.Fatalf("run as %q: %d %s", from, rec.Code, rec.Body)
		}
	}
	var froms []string
	for _, c := range f.calls {
		if i := strings.Index(c, ` from="`); i >= 0 {
			froms = append(froms, c[i+7:i+strings.Index(c[i+7:], `"`)+7])
		}
	}
	want := "ram@example.com/review,ram@example.com,ram@example.com,ram@example.com/mine,ram@example.com,ram@example.com"
	if strings.Join(froms, ",") != want {
		t.Fatalf("run as %v, want %s", froms, want)
	}
	// And a task one of their sessions started is theirs.
	if rec := tdo(t, h, "POST", "/v1/tasks/T-ram/answer", ramIP, "", `{"from":"review","text":"yes"}`); rec.Code != 200 {
		t.Fatalf("member answer: %d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(f.calls, "|"); !strings.Contains(got, "answer T-ram ram@example.com/review yes") {
		t.Fatalf("calls = %s", got)
	}
	if rec := tdo(t, h, "POST", "/v1/tasks/T1/answer", ramIP, "", `{"text":"yes"}`); rec.Code != 403 {
		t.Errorf("answer on the operator's task: %d", rec.Code)
	}
}

func TestAnAgentWithAVerifiedKeyAsksAndAnswersAsItselfOnly(t *testing.T) {
	f, h := setup(t)
	if resp := ado(t, h, "POST", "/v1/ask", "agent-05", `{"text":"which?"}`); resp.StatusCode != 200 {
		t.Fatalf("ask without from: %d", resp.StatusCode)
	}
	if resp := ado(t, h, "POST", "/v1/ask", "agent-05", `{"from":"agent-05","text":"which?"}`); resp.StatusCode != 200 {
		t.Fatalf("ask as itself: %d", resp.StatusCode)
	}
	if resp := ado(t, h, "POST", "/v1/ask", "agent-05", `{"from":"agent-03","text":"which?"}`); resp.StatusCode != 403 {
		t.Fatalf("ask as another agent: %d", resp.StatusCode)
	}
	// T1 was assigned by the operator: no agent answers it, verified or not.
	if resp := ado(t, h, "POST", "/v1/tasks/T1/answer", "agent-05", `{"text":"main"}`); resp.StatusCode != 403 {
		t.Fatalf("answer on a task it did not assign: %d", resp.StatusCode)
	}
	if resp := ado(t, h, "POST", "/v1/tasks/T1/answer", "agent-05", `{"from":"operator","text":"main"}`); resp.StatusCode != 403 {
		t.Fatalf("answer as the operator: %d", resp.StatusCode)
	}
	if resp := ado(t, h, "POST", "/v1/tasks/T1/answer", "agent-05", `{"from":"agent-03","text":"main"}`); resp.StatusCode != 403 {
		t.Fatalf("answer as another agent: %d", resp.StatusCode)
	}
	got := strings.Join(f.calls, "|")
	if got != "ask agent-05 which? 30m0s|ask agent-05 which? 30m0s" {
		t.Fatalf("calls = %s", got)
	}
}
