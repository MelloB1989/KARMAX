package aws

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// fakeAPI is one profile's account, scripted by a test.
type fakeAPI struct {
	mu          sync.Mutex
	account     string
	identityErr error

	alarms     []Alarm
	alarmsErr  error
	history    []Transition
	historyErr error
	historyAsk []time.Time // the from of each history read

	days      map[string]float64 // date -> cost
	costErr   error
	costAsk   [][2]string
	anomalies []Anomaly
	anomErr   error
	budgets   []Budget
	budgetErr error

	services []ServiceCost
	points   []Datapoint
	metricQ  []MetricQuery

	calls map[string]int
}

func (f *fakeAPI) hit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
}

func (f *fakeAPI) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeAPI) Identity(context.Context) (string, error) {
	f.hit("Identity")
	return f.account, f.identityErr
}

func (f *fakeAPI) Alarms(context.Context) ([]Alarm, error) {
	f.hit("Alarms")
	return f.alarms, f.alarmsErr
}

func (f *fakeAPI) AlarmHistory(_ context.Context, from, to time.Time) ([]Transition, error) {
	f.hit("AlarmHistory")
	f.historyAsk = append(f.historyAsk, from)
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	var out []Transition
	for _, h := range f.history {
		if !h.At.Before(from) && h.At.Before(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (f *fakeAPI) Metric(_ context.Context, q MetricQuery) ([]Datapoint, error) {
	f.hit("Metric")
	f.metricQ = append(f.metricQ, q)
	return f.points, nil
}

func (f *fakeAPI) DailyCost(_ context.Context, from, to string) ([]DayCost, error) {
	f.hit("DailyCost")
	f.costAsk = append(f.costAsk, [2]string{from, to})
	if f.costErr != nil {
		return nil, f.costErr
	}
	var out []DayCost
	start, _ := time.Parse(dateLayout, from)
	end, _ := time.Parse(dateLayout, to)
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		ds := d.Format(dateLayout)
		out = append(out, DayCost{Date: ds, Amount: f.days[ds], Unit: "USD"})
	}
	return out, nil
}

func (f *fakeAPI) CostByService(context.Context, string, string) ([]ServiceCost, error) {
	f.hit("CostByService")
	return f.services, f.costErr
}

func (f *fakeAPI) Anomalies(context.Context, string) ([]Anomaly, error) {
	f.hit("Anomalies")
	return f.anomalies, f.anomErr
}

func (f *fakeAPI) Budgets(context.Context, string) ([]Budget, error) {
	f.hit("Budgets")
	return f.budgets, f.budgetErr
}

// harness is a Source over fake accounts, a map for the KV store, and a
// clock the test moves.
type harness struct {
	src       *Source
	apis      map[string]*fakeAPI
	kv        map[string]string
	published []sources.Event
	now       time.Time
	dialed    []string
}

const testConfig = `
[profile ops-admin]
sso_session = example-sso
sso_account_id = 111122223333
region = us-east-1

[profile ops-readonly]
sso_session = example-sso
sso_account_id = 111122223333
region = us-east-1

[profile ops-eu]
sso_session = example-sso
sso_account_id = 111122223333
region = eu-west-1

[profile staging]
sso_session = other-sso
sso_account_id = 444455556666
region = ap-south-1

[profile static]
region = us-east-1
`

func newHarness(t *testing.T, cfg Config, apis map[string]*fakeAPI) *harness {
	t.Helper()
	dir := t.TempDir()
	if cfg.ConfigFile == "" {
		cfg.ConfigFile = filepath.Join(dir, "config")
		if err := os.WriteFile(cfg.ConfigFile, []byte(testConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.CredentialsFile == "" {
		cfg.CredentialsFile = filepath.Join(dir, "credentials")
	}
	h := &harness{apis: apis, kv: map[string]string{}, now: time.Date(2026, 10, 31, 1, 0, 0, 0, time.UTC)}
	h.src = New(Options{
		Config: cfg.withDefaults(),
		Dial: func(_ context.Context, p Profile, region string) (API, error) {
			h.dialed = append(h.dialed, p.Name+"@"+region)
			a, ok := h.apis[p.Name]
			if !ok {
				return nil, &AuthError{Err: errors.New("no fake for " + p.Name)}
			}
			return a, nil
		},
		KV: KV{
			Get: func(g, k string) (string, bool, error) { v, ok := h.kv[g+"/"+k]; return v, ok, nil },
			Set: func(g, k, v string, _ time.Duration) error { h.kv[g+"/"+k] = v; return nil },
		},
		Publish: func(e sources.Event) error { h.published = append(h.published, e); return nil },
		Now:     func() time.Time { return h.now },
	})
	return h
}

func (h *harness) take() []sources.Event {
	out := h.published
	h.published = nil
	return out
}

func (h *harness) health(profile string) Health {
	for _, x := range h.src.Health() {
		if x.Profile == profile {
			return x
		}
	}
	return Health{}
}

func f64(v float64) *float64 { return &v }
