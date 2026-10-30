package runtime

import (
	"path/filepath"
	"strings"
	"testing"

	awssource "github.com/MelloB1989/karmax/internal/sources/aws"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Each profile's health and the source's land where connector health lives,
// and a login is told in the app once, when it becomes needed; a second one
// needed later is told too.
func TestAWSHealthIsSavedAndEachLoginToldOnce(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	h := awsHealth{s: s, agentID: "agent-1", log: zap.NewNop()}
	first := "aws sso login --sso-session example-sso"
	second := "aws login"
	hs := []awssource.Health{
		{Profile: "ops-admin", Status: awssource.StatusHealthy, Detail: "3 alarms, 0 in ALARM"},
		{Profile: "staging", Status: awssource.StatusFailed, Fix: first, Detail: "not logged in; run: " + first},
	}

	h.record(hs, []string{first}, true)
	h.record(hs, []string{first}, true)
	h.record(hs, nil, false) // a cost poll does not decide which logins are needed

	if got, _ := s.ConnectorHealthFor("aws:staging"); got == nil || got.Status != "failed" || !strings.Contains(got.Detail, first) {
		t.Errorf("profile health = %+v", got)
	}
	if got, _ := s.ConnectorHealthFor("aws"); got == nil || got.Status != "degraded" || !strings.Contains(got.Detail, first) {
		t.Errorf("source health = %+v", got)
	}
	notes, _ := s.ListNotifications(10)
	if len(notes) != 1 || !strings.Contains(notes[0].Title+notes[0].Body, first) {
		t.Fatalf("notifications = %+v, want the login told once", notes)
	}

	h.record(hs, []string{first, second}, true)
	notes, _ = s.ListNotifications(10)
	if len(notes) != 2 || !strings.Contains(notes[0].Title+notes[1].Title, second) {
		t.Fatalf("notifications = %+v, want the new login told", notes)
	}
}
