package runtime

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/integration"
)

// An integration's status becomes an account the agent acts through: the id's
// service, its login, and why it is not working when it is not.
func TestAnIntegrationIsAnAccount(t *testing.T) {
	cases := []struct {
		st   integration.Status
		want agent.AccountInfo
	}{
		{integration.Status{ID: "github", Name: "GitHub", Configured: true, Healthy: true, Checked: time.Now()},
			agent.AccountInfo{Service: "github", Name: "GitHub", Connected: true}},
		{integration.Status{ID: "github:work", Name: "GitHub", Account: "work", Configured: true, Healthy: true, Checked: time.Now()},
			agent.AccountInfo{Service: "github", Account: "work", Name: "GitHub", Connected: true}},
		{integration.Status{ID: "gog", Name: "Google", Configured: true, Healthy: false, Error: "token expired", Checked: time.Now()},
			agent.AccountInfo{Service: "gog", Name: "Google", Connected: true, Problem: "token expired"}},
		// Never checked is not evidence of a problem.
		{integration.Status{ID: "slack:team", Name: "Slack", Configured: true},
			agent.AccountInfo{Service: "slack", Account: "team", Name: "Slack", Connected: true}},
		{integration.Status{ID: "notion", Name: "Notion"},
			agent.AccountInfo{Service: "notion", Name: "Notion"}},
	}
	for _, c := range cases {
		if got := accountInfoOf(c.st); got != c.want {
			t.Errorf("accountInfoOf(%+v) = %+v, want %+v", c.st, got, c.want)
		}
	}
}
