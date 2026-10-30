package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

// ReadTool lets the agent look deeper into what the AWS source reported:
// an alarm with its history and metric, any metric's datapoints, cost by
// service, and which profiles work. Every operation is a read; there is no
// way to change anything through it.
type ReadTool struct {
	src *Source

	mu   sync.Mutex
	cost map[string]costHit // billed reads already made, by account and period
}

type costHit struct {
	at  time.Time
	out []ServiceCost
}

// costHitTTL is how long a billed cost read is reused rather than paid again.
const costHitTTL = time.Hour

// NewReadTool is the agent's read-only view of the AWS source's accounts.
func NewReadTool(src *Source) *ReadTool {
	return &ReadTool{src: src, cost: map[string]costHit{}}
}

func (t *ReadTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "aws.read",
		Description: "Read-only look into the operator's AWS accounts, through the profiles in ~/.aws/config that the AWS event source reads. " +
			"op=profiles: which profiles work, which account each reads, and the exact login command for any that do not. " +
			"op=alarms: an account's CloudWatch alarms (state=ALARM to narrow). " +
			"op=alarm: one alarm with its last day of state changes and its metric's recent datapoints (hours, default 3). " +
			"op=metric: any metric's datapoints (namespace, metric, dimensions as Name=Value,..., stat, period seconds, hours). " +
			"op=cost: unblended cost by service from start to end (YYYY-MM-DD, end exclusive; default yesterday), before credits; one billed Cost Explorer call ($0.01), reused for an hour. " +
			"Name the account with profile (and region), or account (id or alias); an AWS event's data carries both. " +
			"Read-only: it cannot change anything. Never delete or modify an AWS resource, by any means, without asking the operator first.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"op": {"type": "string", "enum": ["profiles", "alarms", "alarm", "metric", "cost"]},
				"profile": {"type": "string", "description": "AWS profile name from ~/.aws/config."},
				"account": {"type": "string", "description": "Account id or alias, instead of a profile."},
				"region": {"type": "string", "description": "Region; default the profile's own. Cost is always read in us-east-1."},
				"alarm": {"type": "string", "description": "Alarm name, for op=alarm."},
				"state": {"type": "string", "description": "ALARM, OK or INSUFFICIENT_DATA, for op=alarms."},
				"namespace": {"type": "string"},
				"metric": {"type": "string"},
				"dimensions": {"type": "string", "description": "Name=Value pairs, comma-separated."},
				"stat": {"type": "string", "description": "Average, Sum, Maximum, Minimum, SampleCount or a percentile like p99."},
				"period": {"type": "integer", "description": "Seconds per datapoint; default the alarm's, else 300."},
				"hours": {"type": "integer", "description": "How far back to read datapoints; default 3."},
				"start": {"type": "string", "description": "YYYY-MM-DD, for op=cost."},
				"end": {"type": "string", "description": "YYYY-MM-DD, exclusive, for op=cost."}
			},
			"required": ["op"]
		}`),
	}
}

func (t *ReadTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	out, err := t.run(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(out), nil
}

func (t *ReadTool) run(ctx context.Context, in map[string]any) (any, error) {
	if t.src == nil {
		return nil, fmt.Errorf("the AWS source is off")
	}
	op := strings.ToLower(str(in, "op"))
	if op == "profiles" {
		return map[string]any{"profiles": t.src.Health()}, nil
	}
	region := str(in, "region")
	if op == "cost" {
		region = billingRegion
	}
	p, region, api, err := t.src.resolve(ctx, str(in, "profile"), str(in, "account"), region)
	if err != nil {
		return nil, err
	}
	base := map[string]any{"profile": p.Name, "region": region}
	now := t.src.now()
	hours := num(in, "hours", 3)

	switch op {
	case "alarms":
		alarms, err := api.Alarms(ctx)
		if err != nil {
			return nil, err
		}
		state := strings.ToUpper(str(in, "state"))
		var keep []Alarm
		for _, a := range alarms {
			if state == "" || a.State == state {
				keep = append(keep, a)
			}
		}
		base["alarms"] = keep
		return base, nil

	case "alarm":
		name := str(in, "alarm")
		if name == "" {
			return nil, fmt.Errorf("op=alarm needs alarm=<name>")
		}
		alarms, err := api.Alarms(ctx)
		if err != nil {
			return nil, err
		}
		var a *Alarm
		for i := range alarms {
			if alarms[i].Name == name {
				a = &alarms[i]
			}
		}
		if a == nil {
			return nil, fmt.Errorf("no alarm %q in %s/%s", name, p.Name, region)
		}
		base["alarm"] = a
		if hist, err := api.AlarmHistory(ctx, now.Add(-24*time.Hour), now); err == nil {
			var mine []Transition
			for _, h := range hist {
				if h.Alarm == name {
					mine = append(mine, h)
				}
			}
			base["history"] = mine
		} else {
			base["history_error"] = err.Error()
		}
		if a.Metric != "" {
			period := int32(num(in, "period", int(a.Period)))
			if period <= 0 {
				period = 300
			}
			pts, err := api.Metric(ctx, MetricQuery{Namespace: a.Namespace, Metric: a.Metric, Dimensions: a.Dimensions,
				Statistic: a.Statistic, Period: period, From: now.Add(-time.Duration(hours) * time.Hour), To: now})
			if err != nil {
				base["datapoints_error"] = err.Error()
			} else {
				base["datapoints"] = pts
			}
		}
		return base, nil

	case "metric":
		q := MetricQuery{Namespace: str(in, "namespace"), Metric: str(in, "metric"), Statistic: str(in, "stat"),
			Period: int32(num(in, "period", 300)), From: now.Add(-time.Duration(hours) * time.Hour), To: now}
		if q.Namespace == "" || q.Metric == "" {
			return nil, fmt.Errorf("op=metric needs namespace and metric")
		}
		for _, pair := range strings.Split(str(in, "dimensions"), ",") {
			if k, v, ok := strings.Cut(pair, "="); ok && strings.TrimSpace(k) != "" {
				q.Dimensions = append(q.Dimensions, Dimension{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
			}
		}
		pts, err := api.Metric(ctx, q)
		if err != nil {
			return nil, err
		}
		base["datapoints"] = pts
		return base, nil

	case "cost":
		start, end := str(in, "start"), str(in, "end")
		if start == "" {
			start = now.UTC().AddDate(0, 0, -1).Format(dateLayout)
		}
		if end == "" {
			s, err := time.Parse(dateLayout, start)
			if err != nil {
				return nil, fmt.Errorf("start %q is not YYYY-MM-DD", start)
			}
			end = s.AddDate(0, 0, 1).Format(dateLayout)
		}
		key := p.Name + "|" + start + "|" + end
		t.mu.Lock()
		hit, ok := t.cost[key]
		t.mu.Unlock()
		if !ok || now.Sub(hit.at) > costHitTTL {
			svc, err := api.CostByService(ctx, start, end)
			if err != nil {
				return nil, err
			}
			hit = costHit{at: now, out: svc}
			t.mu.Lock()
			t.cost[key] = hit
			t.mu.Unlock()
		}
		total := 0.0
		for _, s := range hit.out {
			total += s.Amount
		}
		base["start"], base["end"], base["services"], base["total"] = start, end, hit.out, total
		return base, nil
	}
	return nil, fmt.Errorf("op must be profiles, alarms, alarm, metric or cost")
}

// resolve finds the profile to read through: the one named, or the one that
// reads the named account. Either must be one the source may read.
func (s *Source) resolve(ctx context.Context, profile, account, region string) (Profile, string, API, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ps, err := LoadProfiles(s.cfg.ConfigFile, s.cfg.CredentialsFile)
	if err != nil {
		return Profile{}, "", nil, err
	}
	ps = Allowed(ps, s.cfg.Profiles)

	var p *Profile
	switch {
	case profile != "":
		for i := range ps {
			if ps[i].Name == profile {
				p = &ps[i]
			}
		}
		if p == nil {
			return Profile{}, "", nil, fmt.Errorf("profile %q is not one the AWS source reads", profile)
		}
	case account != "":
		readers, _, err := s.plan(ctx, false)
		if err != nil {
			return Profile{}, "", nil, err
		}
		for _, r := range readers {
			if r.account == account || s.cfg.Aliases[r.account] == account {
				rp := r.profile
				p = &rp
				break
			}
		}
		if p == nil {
			return Profile{}, "", nil, fmt.Errorf("no working profile reads account %q", account)
		}
	default:
		return Profile{}, "", nil, fmt.Errorf("name the account: profile=<name> or account=<id or alias>")
	}
	if region == "" {
		region = p.Region
	}
	if region == "" {
		return Profile{}, "", nil, fmt.Errorf("profile %q has no region; pass region=", p.Name)
	}
	api, err := s.apiFor(ctx, *p, region)
	if err == nil {
		_, err = api.Identity(ctx)
	}
	if err != nil {
		if h := failure(*p, err); h.Fix != "" {
			return Profile{}, "", nil, fmt.Errorf("profile %s is not logged in; the operator can run: %s", p.Name, h.Fix)
		}
		return Profile{}, "", nil, err
	}
	return *p, region, api, nil
}

func str(in map[string]any, k string) string {
	switch v := in[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func num(in map[string]any, k string, def int) int {
	switch v := in[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}
