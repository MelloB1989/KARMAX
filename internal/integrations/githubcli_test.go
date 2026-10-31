package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

type stubLogins struct {
	accounts []ghconn.Account
	err      error
}

func (s stubLogins) Accounts(context.Context) ([]ghconn.Account, error) { return s.accounts, s.err }
func (s stubLogins) Token(_ context.Context, a ghconn.Account) (string, error) {
	if a.Login == "octo-gone" {
		return "", errors.New("no oauth token found for octo-gone")
	}
	return "tok-" + a.Login, nil
}

// A GitHub at /<login>/ that accepts each login's own token, except
// octo-work's, which has been revoked.
func ghStub(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if rest != "notifications" || r.Method != http.MethodGet {
			t.Errorf("health asked for %s %s", r.Method, r.URL.Path)
		}
		if login == "octo-work" || r.Header.Get("Authorization") != "Bearer tok-"+login {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEveryGhLoginHasItsOwnHealth(t *testing.T) {
	srv := ghStub(t)
	logins := stubLogins{accounts: []ghconn.Account{
		{Host: "github.com", Login: "octo-user", State: "success", API: srv.URL + "/octo-user"},
		{Host: "github.com", Login: "octo-work", State: "success", API: srv.URL + "/octo-work"},
		{Host: "github.com", Login: "octo-gone", State: "success", API: srv.URL + "/octo-gone"},
	}}
	reg := integration.NewRegistry(integration.NewResolver(nil, nil))
	registerGitHubLogins(context.Background(), reg, logins, srv.Client())

	got := map[string]integration.Status{}
	for _, st := range reg.CheckAll(context.Background()) {
		got[st.ID] = st
	}
	if st := got["gh:octo-user"]; !st.Healthy || !st.Configured || st.Kind != integration.KindConnector ||
		st.AuthKind != connectorkit.AuthCLI || st.Account != "octo-user" {
		t.Errorf("octo-user = %+v, want a working CLI-session connector", st)
	}
	if st := got["gh:octo-work"]; st.Healthy || !strings.Contains(st.Error, "401") {
		t.Errorf("octo-work = %+v, want it failing because GitHub rejected the token", st)
	}
	if st := got["gh:octo-gone"]; st.Healthy || !strings.Contains(st.Error, "octo-gone") {
		t.Errorf("octo-gone = %+v, want it failing because gh has no token", st)
	}
	for _, st := range got {
		if strings.Contains(st.Error, "tok-") {
			t.Errorf("%s's error carries a token: %s", st.ID, st.Error)
		}
	}
	if st := got["gh"]; !st.Healthy {
		t.Errorf("gh = %+v, want it working: it holds logins", st)
	}
}

// With no login there is no source, and the health row says what to do.
func TestNoGhLoginSaysHowToConnectOne(t *testing.T) {
	for _, logins := range []stubLogins{{}, {err: errors.New("exec: gh: not found")}} {
		reg := integration.NewRegistry(integration.NewResolver(nil, nil))
		registerGitHubLogins(context.Background(), reg, logins, http.DefaultClient)
		sts := reg.CheckAll(context.Background())
		if len(sts) != 1 || sts[0].ID != "gh" || sts[0].Healthy || !strings.Contains(sts[0].Error, "gh auth login") {
			t.Errorf("with %+v: statuses = %+v, want one failing gh row saying to log in", logins, sts)
		}
	}
}

// A login added while KARMAX runs gets a row the next time accounts are seen.
func TestALoginAddedLaterGetsARow(t *testing.T) {
	srv := ghStub(t)
	reg := integration.NewRegistry(integration.NewResolver(nil, nil))
	registerGitHubLogins(context.Background(), reg, stubLogins{}, srv.Client())
	RegisterGitHubLogin(reg, stubLogins{}, ghconn.Account{Host: "github.com", Login: "octo-user", API: srv.URL + "/octo-user"}, srv.Client())
	if _, ok := reg.Get("gh:octo-user"); !ok {
		t.Error("the new login has no health row")
	}
}
