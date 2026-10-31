package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/sources"
	sourcegh "github.com/MelloB1989/karmax/internal/sources/github"
	"github.com/MelloB1989/karmax/internal/tools"
)

func TestTheGitHubSourceHasAnOffSwitch(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, "off": false, "OFF": false, " off ": false} {
		if got := githubSourceOn(func(k string) string {
			if k == "KARMAX_SOURCE_GITHUB" {
				return v
			}
			return ""
		}); got != want {
			t.Errorf("KARMAX_SOURCE_GITHUB=%q: on = %v, want %v", v, got, want)
		}
	}
}

// Something said on GitHub as the operator is told to them afterwards: who
// it was said as, what was done, where.
func TestAGitHubActIsToldAfter(t *testing.T) {
	title, body := githubActNotice(ghconn.Act{Account: "octo-work", Action: "requested changes on", Repo: "contoso/app",
		Number: 12, URL: "https://github.com/contoso/app/pull/12#pullrequestreview-1", Body: strings.Repeat("Needs a test. ", 80)})
	for _, want := range []string{"octo-work", "requested changes on", "contoso/app#12"} {
		if !strings.Contains(title, want) {
			t.Errorf("title %q lacks %q", title, want)
		}
	}
	if !strings.Contains(body, "https://github.com/contoso/app/pull/12#pullrequestreview-1") || !strings.Contains(body, "Needs a test.") {
		t.Errorf("body = %q, want the link and what was said", body)
	}
	if len(body) > 400 {
		t.Errorf("body is %d bytes, want an excerpt", len(body))
	}
}

func TestTheGitHubToolsAreToolsTheAgentCanBeGranted(t *testing.T) {
	reg := tools.NewRegistry()
	registerGitHubTools(reg, &ghconn.Actor{Logins: noLogins{}})
	resolved, unresolved := reg.ResolveForAgent([]string{"github.*"})
	if len(unresolved) != 0 || len(resolved) != 5 {
		t.Fatalf("github.* resolved %d tools (unresolved %v), want the five acting tools", len(resolved), unresolved)
	}
	tl, ok := reg.Get("github.reply")
	if !ok {
		t.Fatal("github.reply is not registered")
	}
	res, err := tl.Execute(context.Background(), map[string]any{"repo": "contoso/app", "number": 12, "body": "hi"})
	if err != nil || !res.IsError || !strings.Contains(res.Error, "gh auth login") {
		t.Errorf("acting with no login = %+v, %v; want a tool error saying how to log in", res, err)
	}
}

type noLogins struct{}

func (noLogins) Accounts(context.Context) ([]ghconn.Account, error)    { return nil, nil }
func (noLogins) Token(context.Context, ghconn.Account) (string, error) { return "", errors.New("none") }

type twoLogins struct{ api string }

func (l twoLogins) Accounts(context.Context) ([]ghconn.Account, error) {
	return []ghconn.Account{
		{Host: "github.com", Login: "octo-user", State: "success", API: l.api + "/octo-user"},
		{Host: "github.com", Login: "octo-work", State: "success", API: l.api + "/octo-work"},
	}, nil
}

func (l twoLogins) Token(_ context.Context, a ghconn.Account) (string, error) {
	if a.Login == "octo-work" {
		return "", errors.New("no oauth token found for octo-work")
	}
	return "tok-" + a.Login, nil
}

// One tick: the working login publishes, the broken one is named in the log
// and its health row is checked there and then, not half an hour later.
func TestAGitHubTickPublishesAndReportsABrokenLogin(t *testing.T) {
	was := ghInstalled
	ghInstalled = func() bool { return true } // whether or not this test host has it
	defer func() { ghInstalled = was }()
	updated := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-octo-user" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/notifications") {
			fmt.Fprintf(w, `[{"id":"1","unread":true,"reason":"mention","updated_at":%q,
				"subject":{"title":"Fix login","type":"Issue","url":null},
				"repository":{"full_name":"contoso/app","html_url":"https://github.com/contoso/app","owner":{"login":"contoso"}}}]`, updated)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	logins := twoLogins{api: srv.URL}
	kv := map[string]string{}
	var events []sources.Event
	p := &sourcegh.Poller{Logins: logins, HTTP: srv.Client(),
		KV: sourcegh.KV{
			Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
			Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil },
		},
		Publish: func(e sources.Event) error { events = append(events, e); return nil },
	}
	reg := integration.NewRegistry(integration.NewResolver(nil, nil))
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }

	if err := githubTick(context.Background(), logf, p, logins, reg, srv.Client()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(events) != 1 || events[0].Account != "octo-user" {
		t.Errorf("events = %+v, want octo-user's mention", events)
	}
	for _, id := range []string{"gh:octo-user", "gh:octo-work"} {
		if _, ok := reg.Get(id); !ok {
			t.Errorf("%s has no health row", id)
		}
	}
	var work *integration.Status
	for _, st := range reg.Statuses() {
		if st.ID == "gh:octo-work" {
			st := st
			work = &st
		}
	}
	if work == nil || work.Healthy || !strings.Contains(work.Error, "octo-work") {
		t.Errorf("octo-work's status = %+v, want it checked and failing", work)
	}
	if log := strings.Join(lines, "\n"); !strings.Contains(log, "octo-work") || !strings.Contains(log, "1 event") {
		t.Errorf("log = %q, want the event count and the broken login", log)
	}
}

// A host without gh has no GitHub source. Its health row says so; the loop
// failing every few minutes would only repeat that as alerts.
func TestWithoutGhTheTickIsQuiet(t *testing.T) {
	was := ghInstalled
	ghInstalled = func() bool { return false }
	defer func() { ghInstalled = was }()

	p := &sourcegh.Poller{Logins: brokenGh{}}
	var lines []string
	err := githubTick(context.Background(), func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) },
		p, brokenGh{}, nil, nil)
	if err != nil || len(lines) != 0 {
		t.Errorf("tick without gh = %v, logged %q; want a quiet no-op", err, lines)
	}

	// With gh there but unable to list its logins, the source has stopped,
	// and that is an error worth surfacing.
	ghInstalled = func() bool { return true }
	if err := githubTick(context.Background(), func(string, ...any) {}, p, brokenGh{}, nil, nil); err == nil {
		t.Error("a gh that cannot list its logins was a silent tick")
	}
}

type brokenGh struct{}

func (brokenGh) Accounts(context.Context) ([]ghconn.Account, error) {
	return nil, errors.New(`exec: "gh": executable file not found in $PATH`)
}
func (brokenGh) Token(context.Context, ghconn.Account) (string, error) {
	return "", errors.New("no gh")
}
