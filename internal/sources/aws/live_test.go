package aws

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// TestLiveProfiles reads the real accounts in ~/.aws/config: which profiles
// work, which need a login, and how many alarms each account has in which
// state. Read-only, and it prints counts and states, never account data.
//
//	KARMAX_LIVE_AWS=1 go test ./internal/sources/aws -run TestLive -v
//
// KARMAX_LIVE_AWS_COST=1 also runs one cost poll: two billed Cost Explorer
// calls ($0.01 each) per account read, so narrow it with KARMAX_AWS_PROFILES.
func TestLiveProfiles(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_AWS") != "1" {
		t.Skip("set KARMAX_LIVE_AWS=1 to read the real AWS accounts")
	}
	home, _ := os.UserHomeDir()
	cfg, on := ConfigFromEnv(os.Getenv, home)
	if !on {
		t.Skip("KARMAX_SOURCE_AWS=off")
	}
	kv := map[string]string{}
	var published []sources.Event
	src := New(Options{
		Config: cfg,
		Dial:   SDKDialer(cfg),
		KV: KV{
			Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
			Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil },
		},
		Publish: func(e sources.Event) error { published = append(published, e); return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rep, err := src.PollAlarms(ctx)
	t.Logf("alarms: %d account/regions read, %d alarms, %d in ALARM, %d events; err=%v",
		rep.Readers, rep.Alarms, rep.InAlarm, rep.Published, err)
	for _, h := range src.Health() {
		t.Logf("  %-44s %-9s %s", h.Profile, h.Status, clip(h.Detail, 140))
	}
	for _, f := range rep.Fixes {
		t.Logf("  fix: %s", f)
	}
	states := map[string]int{}
	for _, e := range published {
		states[e.Type+urgency(e.Urgent)]++
	}
	t.Logf("events by type: %v", sortedCounts(states))

	if os.Getenv("KARMAX_LIVE_AWS_COST") != "1" {
		return
	}
	published = nil
	crep, err := src.PollCost(ctx)
	t.Logf("cost: %d accounts read, %d billed calls, %d events; err=%v", crep.Readers, crep.BilledCalls, crep.Published, err)
	kinds := map[string]int{}
	for _, e := range published {
		kinds[e.Type+urgency(e.Urgent)]++
	}
	t.Logf("cost events by type: %v", sortedCounts(kinds))
	for _, h := range src.Health() {
		if h.Status != StatusSkipped {
			t.Logf("  %-44s %-9s %s", h.Profile, h.Status, clip(h.Detail, 140))
		}
	}
}

// TestLiveDailyCost makes one billed Cost Explorer call for the first allowed
// profile and says how many of the last eight days had any spend, before
// credits and after.
//
//	KARMAX_LIVE_AWS_DAYS=1 KARMAX_AWS_PROFILES=<profile> go test ./internal/sources/aws -run TestLiveDailyCost -v
func TestLiveDailyCost(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_AWS_DAYS") != "1" {
		t.Skip("set KARMAX_LIVE_AWS_DAYS=1 for one billed Cost Explorer call")
	}
	home, _ := os.UserHomeDir()
	cfg, _ := ConfigFromEnv(os.Getenv, home)
	ps, err := LoadProfiles(cfg.ConfigFile, cfg.CredentialsFile)
	if err != nil {
		t.Fatal(err)
	}
	ps = Allowed(ps, cfg.Profiles)
	if len(ps) == 0 {
		t.Skip("no profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	api, err := SDKDialer(cfg)(ctx, ps[0], billingRegion)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	days, err := api.DailyCost(ctx, now.AddDate(0, 0, -8).Format(dateLayout), now.Format(dateLayout))
	if err != nil {
		t.Fatal(err)
	}
	spent := 0
	for _, d := range days {
		if d.Amount >= 0.005 {
			spent++
		}
	}
	t.Logf("%d days read, %d with spend (credits counted in: %v)", len(days), spent, cfg.IncludeCredits)
}

func urgency(u bool) string {
	if u {
		return " (urgent)"
	}
	return ""
}

func sortedCounts(m map[string]int) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+strconv.Itoa(v))
	}
	sort.Strings(out)
	return out
}
