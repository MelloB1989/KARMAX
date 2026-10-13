package orgops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/orgops/fakegithub"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// --- fakes -------------------------------------------------------------------

type memKV struct {
	mu sync.Mutex
	m  map[string]map[string]string
}

func newMemKV() *memKV { return &memKV{m: map[string]map[string]string{}} }

func (k *memKV) KVSet(g, key, v string, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m[g] == nil {
		k.m[g] = map[string]string{}
	}
	k.m[g][key] = v
	return nil
}
func (k *memKV) KVGet(g, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[g][key]
	return v, ok, nil
}
func (k *memKV) KVList(g string) ([]store.KVEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []store.KVEntry
	for key, v := range k.m[g] {
		out = append(out, store.KVEntry{Group: g, Key: key, Value: v})
	}
	return out, nil
}
func (k *memKV) KVDelete(g, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m[g], key)
	return nil
}

// fakeKit implements the Kit methods the office uses; anything else panics,
// which is how a test learns the office reached for something new.
type fakeKit struct {
	loopkit.Kit
	mu       sync.Mutex
	said     []string
	harness  func(prompt string) string
	prompts  []string
	remember []string
}

func (k *fakeKit) Harness(_ context.Context, p string) (string, error) {
	k.mu.Lock()
	k.prompts = append(k.prompts, p)
	k.mu.Unlock()
	if k.harness == nil {
		return `{"verdict":"approve","summary":"Looks good.","risk":0.1,"comments":[]}`, nil
	}
	return k.harness(p), nil
}
func (k *fakeKit) HarnessWith(ctx context.Context, s loopkit.HarnessSpec) (loopkit.HarnessResult, error) {
	out, err := k.Harness(ctx, s.Prompt)
	return loopkit.HarnessResult{Output: out}, err
}
func (k *fakeKit) Summarize(_ context.Context, p string) (string, error) {
	return "card: " + truncate(p, 40), nil
}
func (k *fakeKit) CaseOpen(agent, key, title string) (loopkit.Case, error) {
	return loopkit.Case{ID: "case-" + key, Key: key, Agent: agent, Title: title}, nil
}
func (k *fakeKit) CaseSay(_ context.Context, caseID, channel, text string) error {
	return k.SendTo(context.Background(), channel, "", text)
}
func (k *fakeKit) CaseSetState(string, string) error          { return nil }
func (k *fakeKit) CaseLog(string, string, string) error       { return nil }
func (k *fakeKit) Audit(string, string, string, string) error { return nil }
func (k *fakeKit) Notify(string, string) error                { return nil }
func (k *fakeKit) Logf(string, ...any)                        {}
func (k *fakeKit) Remember(f string) error {
	k.mu.Lock()
	k.remember = append(k.remember, f)
	k.mu.Unlock()
	return nil
}
func (k *fakeKit) SendTo(_ context.Context, channel, _ string, text string) error {
	k.mu.Lock()
	k.said = append(k.said, channel+" | "+text)
	k.mu.Unlock()
	return nil
}
func (k *fakeKit) chat() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return strings.Join(k.said, "\n---\n")
}

type harness struct {
	t      *testing.T
	fake   *fakegithub.Server
	srv    *httptest.Server
	svc    *Service
	kit    *fakeKit
	events []map[string]any
	mu     sync.Mutex
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, kit: &fakeKit{}}
	h.fake = fakegithub.New("")
	h.srv = httptest.NewServer(h.fake)
	t.Cleanup(h.srv.Close)
	h.fake.SetBase(h.srv.URL)
	fakegithub.Seed(h.fake, "acme")
	cfg := Config{Enabled: true, Org: "acme", GitHub: GitHubConfig{APIURL: h.srv.URL, BotLogin: "karmax-bot"},
		Channels:  ChannelsConfig{Default: "gchat:spaces/eng"},
		Standards: Standards{TitlePattern: `^(feat|fix|chore|docs|refactor|test)(\(.+\))?: `, RequiredFiles: []string{"README.md"}},
		Repos:     []RepoConfig{{Match: "payments-api", Service: "payments", Owners: []string{"maya"}}},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h.svc = New(cfg, NewGitHub(h.srv.URL, "t"), newMemKV(), func(kind string, p map[string]any) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		cp := map[string]any{"event_kind": kind}
		for k, v := range p {
			cp[k] = v
		}
		h.events = append(h.events, cp)
		return nil
	})
	return h
}

func (h *harness) published(kind string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.events {
		if e["event_kind"] == kind {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) pendingCodes() []string {
	ps, _ := listJSON[Pending](h.svc, groupPending)
	var out []string
	for _, p := range ps {
		out = append(out, p.Code)
	}
	return out
}

// --- tests ---------------------------------------------------------------------

func TestReviewBlocksALeakedSecret(t *testing.T) {
	h := newHarness(t, nil)
	pr := h.fake.OpenPR(fakegithub.PR{Repo: "acme/payments-api", Title: "add stripe client", Author: "dev1",
		Files: []fakegithub.File{{Filename: "stripe.go", Patch: "@@ -1,2 +1,3 @@\n package main\n+var awsKey = \"AKIAABCDEFGHIJKLMNOP\"\n func x() {}"}}})

	res, err := h.svc.ReviewPR(context.Background(), h.kit, "acme/payments-api", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event != "REQUEST_CHANGES" {
		t.Fatalf("event = %s, want REQUEST_CHANGES", res.Event)
	}
	snap := h.fake.Snapshot()
	reviews := snap["reviews"].([]fakegithub.Review)
	if len(reviews) != 1 || reviews[0].Event != "REQUEST_CHANGES" || !strings.Contains(reviews[0].Body, "aws-access-key") {
		t.Fatalf("review not posted as expected: %+v", reviews)
	}
	if len(reviews[0].Comments) == 0 {
		t.Fatal("want an inline comment on the leaked key")
	}
	if !strings.Contains(reviews[0].Body, "title") {
		t.Fatal("want the title-convention finding in the review")
	}
	chat := h.kit.chat()
	if !strings.Contains(chat, "⛔") || strings.Contains(chat, "AKIAABCDEFGHIJKLMNOP") {
		t.Fatalf("chat should flag the PR without repeating the secret:\n%s", chat)
	}
	if len(h.pendingCodes()) != 0 || len(snap["merges"].([]string)) != 0 {
		t.Fatal("a blocked PR must not be queued for merge")
	}
}

func TestCleanPRMergesAutomaticallyOnGreenCI(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Policy = Policy{Merge: ModeAuto, ApprovePR: ModeAuto}
	})
	pr := h.fake.OpenPR(fakegithub.PR{Repo: "acme/payments-api", Title: "feat: add refunds endpoint", Body: "adds refunds", Author: "dev1",
		Files: []fakegithub.File{{Filename: "refunds.go", Patch: "@@ -0,0 +1,2 @@\n+package main\n+func refund() {}"},
			{Filename: "refunds_test.go", Patch: "@@ -0,0 +1,1 @@\n+package main"}}})
	ctx := context.Background()
	res, err := h.svc.ReviewPR(ctx, h.kit, "acme/payments-api", pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if res.Event != "APPROVE" || res.Merge != ModeAuto {
		t.Fatalf("got event %s merge %s", res.Event, res.Merge)
	}
	if got := h.fake.Snapshot()["merges"].([]string); len(got) != 0 {
		t.Fatalf("merged before CI reported: %v", got)
	}
	h.fake.SetCI("acme/payments-api", pr.Number, "success")
	h.svc.OnCheckSuite(ctx, h.kit, map[string]any{"repo": "acme/payments-api", "number": float64(pr.Number)})
	if got := h.fake.Snapshot()["merges"].([]string); len(got) != 1 {
		t.Fatalf("want merged on green CI, merges = %v\nchat:\n%s", got, h.kit.chat())
	}
	if !strings.Contains(h.kit.chat(), "✅ Merged acme/payments-api#1") {
		t.Fatalf("chat:\n%s", h.kit.chat())
	}
}

func TestMergeWaitsForHumanApprovalInAnyChat(t *testing.T) {
	h := newHarness(t, nil) // default policy: merge needs approval
	pr := h.fake.OpenPR(fakegithub.PR{Repo: "acme/auth-service", Title: "fix: token expiry", Body: "x", Author: "dev2",
		Files: []fakegithub.File{{Filename: "README.md", Patch: "@@ -1 +1,2 @@\n x\n+docs"}}})
	ctx := context.Background()
	if _, err := h.svc.ReviewPR(ctx, h.kit, "acme/auth-service", pr.Number); err != nil {
		t.Fatal(err)
	}
	codes := h.pendingCodes()
	if len(codes) != 1 {
		t.Fatalf("want one approval waiting, got %v\n%s", codes, h.kit.chat())
	}
	h.fake.SetCI("acme/auth-service", pr.Number, "success")

	cmd, ok := ParseCommand("@karmax approve " + strings.ToLower(codes[0]))
	if !ok || cmd.Verb != "approve" {
		t.Fatalf("parse: %+v %v", cmd, ok)
	}
	if err := h.svc.HandleCommand(ctx, h.kit, cmd, "gchat:spaces/eng", "Maya"); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.Snapshot()["merges"].([]string); len(got) != 1 {
		t.Fatalf("approval should merge on green CI; merges=%v\n%s", got, h.kit.chat())
	}
	// A second approve of the same code does nothing.
	_ = h.svc.HandleCommand(ctx, h.kit, cmd, "gchat:spaces/eng", "Raj")
	if got := h.fake.Snapshot()["merges"].([]string); len(got) != 1 {
		t.Fatalf("double approve merged twice: %v", got)
	}
}

func TestIncidentAutopilotFixesAndShips(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Policy = Policy{IncidentFix: ModeAuto, IncidentMerge: ModeAuto}
	})
	ctx := context.Background()
	// The "engineer" opens a PR on the branch it was told to use.
	h.kit.harness = func(p string) string {
		if !strings.Contains(p, "KARMAX_RESULT") {
			return `{"verdict":"approve","summary":"Minimal, tested fix.","risk":0.2,"comments":[]}`
		}
		var branch string
		for _, f := range strings.Fields(p) {
			if strings.HasPrefix(f, "karmax/fix-inc-") {
				branch = strings.TrimSuffix(f, ".")
			}
		}
		pr := h.fake.OpenPR(fakegithub.PR{Repo: "acme/payments-api", Title: "fix: nil customer on refund", Body: "Incident",
			Author: "karmax-bot", Head: branch,
			Files: []fakegithub.File{{Filename: "refunds.go", Patch: "@@ -1 +1,2 @@\n x\n+if c == nil { return errNoCustomer }"},
				{Filename: "refunds_test.go", Patch: "@@ -0,0 +1 @@\n+package main"}}})
		return fmt.Sprintf("done\nKARMAX_RESULT {\"pr_url\":\"%s/acme/payments-api/pull/%d\",\"root_cause\":\"nil customer dereference in refund handler\",\"summary\":\"guarded nil\"}",
			h.srv.URL, pr.Number)
	}

	in, err := h.svc.HandleAlert(ctx, h.kit, Alert{Key: "probe:payments", Source: "probe", Title: "payments is failing",
		Detail: "80% of checks returned HTTP 500", Service: "payments", Severity: "critical"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Repo != "acme/payments-api" {
		t.Fatalf("service should map to its repo, got %q", in.Repo)
	}
	if !strings.Contains(h.kit.chat(), "🚨 *Incident") || !strings.Contains(h.kit.chat(), "@maya") {
		t.Fatalf("owners not alerted:\n%s", h.kit.chat())
	}
	fixes := h.published(EventFix)
	if len(fixes) != 1 {
		t.Fatalf("auto policy should dispatch a fix, events=%v", h.events)
	}
	// A repeat of the same alert does not open a second incident.
	again, _ := h.svc.HandleAlert(ctx, h.kit, Alert{Key: "probe:payments", Title: "payments is failing", Service: "payments"})
	if again.ID != in.ID {
		t.Fatal("duplicate alert opened a second incident")
	}

	if err := h.svc.FixIncident(ctx, h.kit, str(fixes[0], "incident"), ""); err != nil {
		t.Fatal(err)
	}
	got, _ := h.svc.IncidentByID(in.ID)
	if got.State != "fix-pr-open" || got.PRNumber == 0 || got.RootCause == "" {
		t.Fatalf("incident after fix: %+v\n%s", got, h.kit.chat())
	}
	h.fake.SetCI("acme/payments-api", got.PRNumber, "success")
	h.svc.SweepAutomerge(ctx, h.kit)
	got, _ = h.svc.IncidentByID(in.ID)
	if got.State != "fix-merged" {
		t.Fatalf("want fix merged, got %s\n%s", got.State, h.kit.chat())
	}
	if _, err := h.svc.HandleAlert(ctx, h.kit, Alert{Key: "probe:payments", Resolved: true, Detail: "healthy again"}); err != nil {
		t.Fatal(err)
	}
	got, _ = h.svc.IncidentByID(in.ID)
	if got.State != "resolved" || !strings.Contains(h.kit.chat(), "✅ *Resolved*") {
		t.Fatalf("not resolved: %+v", got)
	}
	if len(h.kit.remember) == 0 {
		t.Fatal("the incident should be remembered")
	}
}

func TestIncidentFixNeedsApprovalByDefault(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	in, _ := h.svc.HandleAlert(ctx, h.kit, Alert{Title: "500s on checkout", Service: "payments", Severity: "critical"})
	if len(h.published(EventFix)) != 0 {
		t.Fatal("default policy must not dispatch a fix unasked")
	}
	codes := h.pendingCodes()
	if len(codes) != 1 {
		t.Fatalf("want an approval, got %v", codes)
	}
	if err := h.svc.HandleCommand(ctx, h.kit, Command{Verb: "approve", Code: codes[0]}, "", "maya"); err != nil {
		t.Fatal(err)
	}
	if fx := h.published(EventFix); len(fx) != 1 || fx[0]["incident"] != in.ID {
		t.Fatalf("approval should dispatch the fix: %v", fx)
	}
}

func TestProbeOpensAndClosesWithHysteresis(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer svc.Close()
	h := newHarness(t, func(c *Config) {
		c.Probes = []ProbeConfig{{Name: "payments", URL: svc.URL, Window: 4, Repo: "payments-api"}}
	})
	ctx := context.Background()
	var trans []Transition
	for i := 0; i < 3; i++ {
		trans = append(trans, h.svc.RunProbes(ctx)...)
	}
	if len(trans) != 1 || !trans[0].Firing {
		t.Fatalf("want exactly one firing transition, got %+v", trans)
	}
	failing.Store(false)
	trans = nil
	for i := 0; i < 4; i++ {
		trans = append(trans, h.svc.RunProbes(ctx)...)
	}
	if len(trans) != 1 || trans[0].Firing {
		t.Fatalf("want one recovery, got %+v", trans)
	}
	if err := h.svc.RecordProbe(trans[0]); err != nil {
		t.Fatal(err)
	}
	if a := h.published(EventAlert); len(a) != 1 || a[0]["resolved"] != true {
		t.Fatalf("recovery not published: %v", a)
	}
}

func TestRepoFactoryBootstrapsStandards(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Policy = Policy{RepoCreate: ModeAuto}
		c.Factory = FactoryConfig{Templates: map[string]string{"go": "template-go-service"}, ProtectDefaultBranch: true,
			RequiredChecks: []string{"ci"}, Bootstrap: map[string]string{".github/pull_request_template.md": "## What\n## Why\n"}}
	})
	cmd, ok := ParseCommand("create repo ledger-svc from go: double-entry ledger")
	if !ok {
		t.Fatal("create not parsed")
	}
	if err := h.svc.HandleCommand(context.Background(), h.kit, cmd, "slack:#eng", "maya"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.fake.File("acme/ledger-svc", ".github/workflows/ci.yml"); !ok {
		t.Fatal("template contents missing")
	}
	if _, ok := h.fake.File("acme/ledger-svc", ".github/pull_request_template.md"); !ok {
		t.Fatal("bootstrap file missing")
	}
	if !strings.Contains(h.kit.chat(), "📦 Created *acme/ledger-svc*") {
		t.Fatalf("chat:\n%s", h.kit.chat())
	}
	if ix := h.published(EventIndex); len(ix) != 1 {
		t.Fatal("new repo should be indexed")
	}
}

func TestKnowledgeIndexesRepoDocs(t *testing.T) {
	h := newHarness(t, nil)
	card, err := h.svc.IndexRepo(context.Background(), h.kit, "payments-api")
	if err != nil {
		t.Fatal(err)
	}
	if card.Repo != "acme/payments-api" || len(h.kit.remember) != 1 {
		t.Fatalf("card %+v remembered %v", card, h.kit.remember)
	}
	if len(h.svc.Cards("payments")) != 1 || len(h.svc.Cards("kubernetes")) != 0 {
		t.Fatal("card lookup")
	}
}

func TestReviewUsesOrgAndRepoRules(t *testing.T) {
	h := newHarness(t, nil)
	pr := h.fake.OpenPR(fakegithub.PR{Repo: "acme/payments-api", Title: "feat: amounts", Author: "d",
		Files: []fakegithub.File{{Filename: "a.go", Patch: "@@ -0,0 +1 @@\n+var amount float64"}}})
	if _, err := h.svc.ReviewPR(context.Background(), h.kit, "acme/payments-api", pr.Number); err != nil {
		t.Fatal(err)
	}
	if len(h.kit.prompts) == 0 || !strings.Contains(h.kit.prompts[0], "Money is integer minor units") {
		t.Fatal("the repo's CLAUDE.md should be in the review prompt")
	}
}

func TestParseCommand(t *testing.T) {
	cases := map[string]Command{
		"approve K7F2Q":                      {Verb: "approve", Code: "K7F2Q"},
		"karmax: reject k7f2q":               {Verb: "reject", Code: "K7F2Q"},
		"fix INC-AB2CD in acme/payments-api": {Verb: "fix", Incident: "INC-AB2CD", Repo: "acme/payments-api"},
		"merge payments-api#12":              {Verb: "merge", Repo: "payments-api", Number: 12},
		"review acme/auth-service#3":         {Verb: "review", Repo: "acme/auth-service", Number: 3},
		"/karmax status":                     {Verb: "status"},
		"index all":                          {Verb: "index", Repo: "all"},
	}
	for in, want := range cases {
		got, ok := ParseCommand(in)
		if !ok || got != want {
			t.Errorf("%q => %+v %v, want %+v", in, got, ok, want)
		}
	}
	for _, chatter := range []string{"go ahead with lunch", "can you review this?", "status of the build looks bad", "approve"} {
		if c, ok := ParseCommand(chatter); ok {
			t.Errorf("%q should be conversation, parsed as %+v", chatter, c)
		}
	}
}

func TestNormalizeAlerts(t *testing.T) {
	am := `{"alerts":[{"status":"firing","labels":{"alertname":"HighErrorRate","service":"payments","severity":"critical"},
	        "annotations":{"summary":"5xx above 5%","description":"payments 5xx 12%"}}]}`
	got, err := NormalizeAlerts([]byte(am))
	if err != nil || len(got) != 1 || got[0].Service != "payments" || got[0].Severity != "critical" || got[0].Resolved {
		t.Fatalf("alertmanager: %+v %v", got, err)
	}
	sentry := `{"action":"triggered","data":{"event":{"title":"TypeError: x is undefined","project":"web","level":"error","web_url":"https://s/1"}}}`
	got, _ = NormalizeAlerts([]byte(sentry))
	if len(got) != 1 || got[0].Source != "sentry" || got[0].Service != "web" {
		t.Fatalf("sentry: %+v", got)
	}
	got, _ = NormalizeAlerts([]byte(`{"title":"disk full","service":"db","status":"resolved"}`))
	if len(got) != 1 || !got[0].Resolved || got[0].Service != "db" {
		t.Fatalf("generic: %+v", got)
	}
}

func TestPolicyRiskCeilingTurnsAutoIntoApprove(t *testing.T) {
	p := Policy{Merge: ModeAuto, MaxAutoRisk: 0.4}.withDefaults()
	if p.Decide(ActMerge, 0.2) != ModeAuto || p.Decide(ActMerge, 0.7) != ModeApprove {
		t.Fatal("risk ceiling not applied")
	}
	repoCfg := Config{Org: "acme", Policy: Policy{Merge: ModeApprove},
		Repos: []RepoConfig{{Match: "acme/sandbox-*", Policy: Policy{Merge: ModeAuto}}}}.WithDefaults()
	if repoCfg.PolicyFor("acme/sandbox-1").Merge != ModeAuto || repoCfg.PolicyFor("acme/core").Merge != ModeApprove {
		t.Fatal("per-repo override")
	}
}

func TestGitHubWebhookIntakeVerifiesAndPublishes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.GitHub.WebhookSecret = "s3cret" })
	intake := httptest.NewServer(h.svc.GitHubWebhookHandler())
	defer intake.Close()
	h.fake.Webhook, h.fake.Secret = intake.URL, "s3cret"
	h.fake.OpenPR(fakegithub.PR{Repo: "acme/payments-api", Title: "feat: x", Author: "d"})
	if got := h.published("github.pr.opened"); len(got) != 1 || got[0]["repo"] != "acme/payments-api" {
		t.Fatalf("delivery not published: %v / %v", h.events, h.fake.Delivered)
	}
	h.fake.Secret = "wrong"
	h.fake.OpenPR(fakegithub.PR{Repo: "acme/payments-api", Title: "feat: y", Author: "d"})
	if got := h.published("github.pr.opened"); len(got) != 1 {
		t.Fatal("a badly signed delivery was accepted")
	}
}
