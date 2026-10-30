package github

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// The source against this machine's real gh logins, read-only: two ticks over
// a throwaway store (so every login is on first sight), then which login would
// act on the repositories seen. Prints counts, types, repositories and login
// names — never a title, a comment or a token. Skipped unless
// KARMAX_LIVE_GITHUB is set:
//
//	KARMAX_LIVE_GITHUB=1 go test ./internal/sources/github/ -run Live -v
func TestLiveAgainstTheRealGhLogins(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_GITHUB") == "" {
		t.Skip("set KARMAX_LIVE_GITHUB=1 to read this machine's real GitHub notifications (read-only)")
	}
	cli := ghconn.CLI{Binary: hostpaths.Gh()}
	kv := map[string]string{}
	var events []sources.Event
	p := &Poller{
		Logins: cli,
		KV: KV{
			Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
			Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil },
		},
		Publish: func(e sources.Event) error { events = append(events, e); return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := p.Tick(ctx)
	if err != nil {
		t.Fatalf("first tick: %v", err)
	}
	for _, a := range res.Accounts {
		status := "ok"
		if a.Err != nil {
			status = "FAILED: " + a.Err.Error()
		}
		t.Logf("tick 1  %-24s published %3d  %s", a.Account, a.Published, status)
	}

	types, repos := map[string]int{}, map[string]int{}
	urgent := 0
	for _, e := range events {
		types[e.Account+"  "+e.Type]++
		repos[e.Account+"  "+e.Data["repo"].(string)]++
		if e.Urgent {
			urgent++
		}
		if !strings.HasPrefix(e.URL, "https://") || e.Key == "" || e.Title == "" {
			t.Errorf("event without a link, key or title: type %s key %q", e.Type, e.Key)
		}
	}
	t.Logf("%d events, %d urgent", len(events), urgent)
	for _, k := range sortedKeys(types) {
		t.Logf("  type  %-60s %d", k, types[k])
	}
	for _, k := range sortedKeys(repos) {
		t.Logf("  repo  %-60s %d", k, repos[k])
	}

	// Straight away again: GitHub's interval says wait, and nothing repeats.
	before := len(events)
	res, err = p.Tick(ctx)
	if err != nil {
		t.Fatalf("second tick: %v", err)
	}
	for _, a := range res.Accounts {
		t.Logf("tick 2  %-24s published %d  waiting %v  unchanged %v  err %v", a.Account, a.Published, a.Waiting, a.NotModified, a.Err)
	}
	if len(events) != before {
		t.Errorf("the second tick published %d more", len(events)-before)
	}

	// Which login would act, for a few repositories seen.
	actor := &ghconn.Actor{Logins: cli}
	var tool connectorkit.Tool
	for _, tl := range actor.Tools() {
		if tl.Name == "github.accounts" {
			tool = tl
		}
	}
	seen := 0
	for _, k := range sortedKeys(repos) {
		if seen == 4 {
			break
		}
		seen++
		_, repo, _ := strings.Cut(k, "  ")
		out, err := tool.Call(ctx, connectorkit.Credentials{}, map[string]any{"repo": repo})
		if err != nil {
			t.Logf("  act on %-40s error: %v", repo, err)
			continue
		}
		m := out.(map[string]any)
		t.Logf("  act on %-40s as %v %v", repo, m["acts_as"], m["choose"])
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
