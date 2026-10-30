package aws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func call(t *testing.T, tool *ReadTool, in map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		return nil, res.Error
	}
	b, _ := json.Marshal(res.Output)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("output %s: %v", b, err)
	}
	return out, ""
}

// Handling an alarm, the agent sees the alarm, its recent history and the
// metric's recent datapoints in one call.
func TestTheToolDescribesAnAlarmWithItsMetric(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	at := time.Date(2026, 10, 31, 0, 30, 0, 0, time.UTC)
	acct.alarms = []Alarm{cpuAlarm("ALARM", at), {Name: "example-other", State: "OK"}}
	acct.history = []Transition{{Alarm: "example-api-cpu-high", At: at, From: "OK", To: "ALARM"}, {Alarm: "example-other", At: at, From: "ALARM", To: "OK"}}
	acct.points = []Datapoint{{At: at, Value: 93.2, Unit: "Percent"}}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	tool := NewReadTool(h.src)

	out, errText := call(t, tool, map[string]any{"op": "alarm", "profile": "ops-admin", "alarm": "example-api-cpu-high", "hours": "2"})
	if errText != "" {
		t.Fatal(errText)
	}
	if a, _ := out["alarm"].(map[string]any); a["Name"] != "example-api-cpu-high" || a["State"] != "ALARM" {
		t.Errorf("alarm = %v", out["alarm"])
	}
	if hist, _ := out["history"].([]any); len(hist) != 1 {
		t.Errorf("history = %v, want only this alarm's", out["history"])
	}
	if pts, _ := out["datapoints"].([]any); len(pts) != 1 {
		t.Errorf("datapoints = %v", out["datapoints"])
	}
	q := acct.metricQ[0]
	if q.Namespace != "AWS/EC2" || q.Metric != "CPUUtilization" || q.Statistic != "Average" || q.Period != 300 ||
		len(q.Dimensions) != 1 || q.To.Sub(q.From) != 2*time.Hour {
		t.Errorf("metric query = %+v", q)
	}
}

func TestTheToolReadsAnyMetric(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	_, errText := call(t, NewReadTool(h.src), map[string]any{"op": "metric", "profile": "ops-admin", "namespace": "AWS/RDS",
		"metric": "FreeStorageSpace", "dimensions": "DBInstanceIdentifier=example-db", "stat": "Minimum", "period": 60.0})
	if errText != "" {
		t.Fatal(errText)
	}
	q := acct.metricQ[0]
	if q.Namespace != "AWS/RDS" || q.Statistic != "Minimum" || q.Period != 60 || q.Dimensions[0] != (Dimension{"DBInstanceIdentifier", "example-db"}) {
		t.Errorf("query = %+v", q)
	}
}

func TestTheToolListsAlarmsInAState(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", alarms: []Alarm{cpuAlarm("ALARM", time.Time{}), {Name: "example-other", State: "OK"}}}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	out, errText := call(t, NewReadTool(h.src), map[string]any{"op": "alarms", "profile": "ops-admin", "state": "alarm"})
	if errText != "" {
		t.Fatal(errText)
	}
	if list, _ := out["alarms"].([]any); len(list) != 1 {
		t.Fatalf("alarms = %v", out["alarms"])
	}
}

// Cost by service is a billed call: asked twice in a row, it is read once.
func TestTheToolReadsCostByServiceOnce(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", services: []ServiceCost{{"Amazon Elastic Compute Cloud - Compute", 12.5, "USD"}}}
	h := newHarness(t, Config{Profiles: []string{"ops-eu"}}, map[string]*fakeAPI{"ops-eu": acct})
	tool := NewReadTool(h.src)
	in := map[string]any{"op": "cost", "profile": "ops-eu", "start": "2026-10-30", "end": "2026-10-31"}
	out, errText := call(t, tool, in)
	if errText != "" {
		t.Fatal(errText)
	}
	if svc, _ := out["services"].([]any); len(svc) != 1 {
		t.Errorf("services = %v", out["services"])
	}
	call(t, tool, in)
	if acct.count("CostByService") != 1 {
		t.Errorf("billed reads = %d", acct.count("CostByService"))
	}
	if h.dialed[len(h.dialed)-1] != "ops-eu@us-east-1" {
		t.Errorf("cost read through %v; billing answers in us-east-1", h.dialed)
	}
}

// An account can be named instead of a profile: by id or by alias.
func TestTheToolFindsTheProfileForAnAccount(t *testing.T) {
	acct := &fakeAPI{account: "444455556666", alarms: []Alarm{{Name: "example-staging-5xx", State: "OK"}}}
	h := newHarness(t, Config{Profiles: []string{"staging"}, Aliases: map[string]string{"444455556666": "example-staging"}},
		map[string]*fakeAPI{"staging": acct})
	out, errText := call(t, NewReadTool(h.src), map[string]any{"op": "alarms", "account": "example-staging"})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["profile"] != "staging" || out["region"] != "ap-south-1" {
		t.Errorf("read through %v in %v", out["profile"], out["region"])
	}
}

// Only profiles the source may read can be read through the tool.
func TestTheToolKeepsToTheAllowList(t *testing.T) {
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": {account: "111122223333"}, "staging": {account: "444455556666"}})
	_, errText := call(t, NewReadTool(h.src), map[string]any{"op": "alarms", "profile": "staging"})
	if !strings.Contains(errText, "staging") {
		t.Fatalf("error = %q, want the profile refused", errText)
	}
}

// The tool reports which profiles work and how to fix the ones that do not.
func TestTheToolReportsProfileHealth(t *testing.T) {
	h := newHarness(t, Config{Profiles: []string{"ops-admin", "staging"}}, map[string]*fakeAPI{
		"ops-admin": {account: "111122223333"}, "staging": {account: "444455556666", identityErr: &AuthError{Err: errContext("expired")}}})
	h.src.PollAlarms(context.Background())
	out, errText := call(t, NewReadTool(h.src), map[string]any{"op": "profiles"})
	if errText != "" {
		t.Fatal(errText)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), "aws sso login --sso-session other-sso") {
		t.Errorf("profiles = %s", b)
	}
}

// The tool can only read, and says the hard floor where the agent reads it.
func TestTheToolSaysItOnlyReads(t *testing.T) {
	m := NewReadTool(nil).Manifest()
	if m.Name != "aws.read" || !json.Valid(m.Parameters) {
		t.Fatalf("manifest = %+v", m)
	}
	if !strings.Contains(m.Description, "Read-only") || !strings.Contains(m.Description, "without asking") {
		t.Errorf("description %q does not state the hard floor", m.Description)
	}
}

type errContext string

func (e errContext) Error() string { return string(e) }
