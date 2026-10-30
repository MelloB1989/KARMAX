package aws

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkaws "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// callTimeout bounds one AWS call, pages included.
const callTimeout = 45 * time.Second

// SDKDialer opens profiles with the AWS SDK for Go, reading the same config
// and credentials files the aws CLI does: SSO sessions, `aws login`, static
// keys and assumed roles alike. Credentials stay inside the SDK; nothing
// here reads, logs or returns them.
func SDKDialer(configFile, credentialsFile string) Dialer {
	return func(ctx context.Context, p Profile, region string) (API, error) {
		opts := []func(*config.LoadOptions) error{
			config.WithSharedConfigProfile(p.Name),
			config.WithRegion(region),
			// A laptop has no instance metadata; asking for it only adds a wait.
			config.WithEC2IMDSClientEnableState(imds.ClientDisabled),
		}
		if configFile != "" {
			opts = append(opts, config.WithSharedConfigFiles([]string{configFile}))
		}
		if credentialsFile != "" {
			opts = append(opts, config.WithSharedCredentialsFiles([]string{credentialsFile}))
		}
		cfg, err := config.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, &AuthError{Err: err}
		}
		return &sdkAPI{cfg: cfg}, nil
	}
}

type sdkAPI struct {
	cfg sdkaws.Config

	mu      sync.Mutex
	account string
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, callTimeout)
}

func (a *sdkAPI) Identity(ctx context.Context) (string, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	// Cached by the SDK until they expire, so this is the login check
	// without a call, most of the time.
	if _, err := a.cfg.Credentials.Retrieve(ctx); err != nil {
		return "", classifyLogin(err)
	}
	a.mu.Lock()
	known := a.account
	a.mu.Unlock()
	if known != "" {
		return known, nil
	}
	out, err := sts.NewFromConfig(a.cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", classify(err)
	}
	a.mu.Lock()
	a.account = sdkaws.ToString(out.Account)
	a.mu.Unlock()
	return sdkaws.ToString(out.Account), nil
}

func (a *sdkAPI) Alarms(ctx context.Context) ([]Alarm, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out []Alarm
	p := cloudwatch.NewDescribeAlarmsPaginator(cloudwatch.NewFromConfig(a.cfg), &cloudwatch.DescribeAlarmsInput{
		AlarmTypes: []cwtypes.AlarmType{cwtypes.AlarmTypeMetricAlarm, cwtypes.AlarmTypeCompositeAlarm},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, classify(err)
		}
		for _, m := range page.MetricAlarms {
			out = append(out, metricAlarm(m))
		}
		for _, c := range page.CompositeAlarms {
			out = append(out, Alarm{
				Name: sdkaws.ToString(c.AlarmName), ARN: sdkaws.ToString(c.AlarmArn), Description: sdkaws.ToString(c.AlarmDescription),
				Composite: true, Rule: sdkaws.ToString(c.AlarmRule), State: string(c.StateValue), Reason: sdkaws.ToString(c.StateReason),
				UpdatedAt: stateChangedAt(c.StateTransitionedTimestamp, c.StateUpdatedTimestamp),
			})
		}
	}
	return out, nil
}

func metricAlarm(m cwtypes.MetricAlarm) Alarm {
	a := Alarm{
		Name: sdkaws.ToString(m.AlarmName), ARN: sdkaws.ToString(m.AlarmArn), Description: sdkaws.ToString(m.AlarmDescription),
		State: string(m.StateValue), Reason: sdkaws.ToString(m.StateReason),
		UpdatedAt: stateChangedAt(m.StateTransitionedTimestamp, m.StateUpdatedTimestamp),
		Namespace: sdkaws.ToString(m.Namespace), Metric: sdkaws.ToString(m.MetricName),
		Statistic: string(m.Statistic), Period: sdkaws.ToInt32(m.Period), EvaluationPeriods: sdkaws.ToInt32(m.EvaluationPeriods),
		Comparison: string(m.ComparisonOperator), Threshold: m.Threshold,
	}
	if a.Statistic == "" {
		a.Statistic = sdkaws.ToString(m.ExtendedStatistic)
	}
	for _, d := range m.Dimensions {
		a.Dimensions = append(a.Dimensions, Dimension{Name: sdkaws.ToString(d.Name), Value: sdkaws.ToString(d.Value)})
	}
	for _, q := range m.Metrics {
		switch {
		case q.Expression != nil:
			a.Expressions = append(a.Expressions, sdkaws.ToString(q.Id)+" = "+sdkaws.ToString(q.Expression))
		case q.MetricStat != nil && q.MetricStat.Metric != nil:
			ms := q.MetricStat
			a.Expressions = append(a.Expressions, sdkaws.ToString(q.Id)+" = "+sdkaws.ToString(ms.Metric.Namespace)+" "+
				sdkaws.ToString(ms.Metric.MetricName)+" "+sdkaws.ToString(ms.Stat))
		}
	}
	return a
}

// stateChangedAt is when the state last changed; the updated timestamp also
// moves when only the evaluation state does, so it is the fallback.
func stateChangedAt(transitioned, updated *time.Time) time.Time {
	if transitioned != nil {
		return *transitioned
	}
	return sdkaws.ToTime(updated)
}

func (a *sdkAPI) AlarmHistory(ctx context.Context, from, to time.Time) ([]Transition, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out []Transition
	p := cloudwatch.NewDescribeAlarmHistoryPaginator(cloudwatch.NewFromConfig(a.cfg), &cloudwatch.DescribeAlarmHistoryInput{
		AlarmTypes:      []cwtypes.AlarmType{cwtypes.AlarmTypeMetricAlarm, cwtypes.AlarmTypeCompositeAlarm},
		HistoryItemType: cwtypes.HistoryItemTypeStateUpdate,
		StartDate:       sdkaws.Time(from),
		EndDate:         sdkaws.Time(to),
		ScanBy:          cwtypes.ScanByTimestampAscending,
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, classify(err)
		}
		for _, h := range page.AlarmHistoryItems {
			// A contributor's own history belongs to the alarm's, not a transition of it.
			if h.AlarmContributorId != nil {
				continue
			}
			fromState, toState, reason := parseHistory(sdkaws.ToString(h.HistoryData), sdkaws.ToString(h.HistorySummary))
			out = append(out, Transition{
				Alarm: sdkaws.ToString(h.AlarmName), Composite: h.AlarmType == cwtypes.AlarmTypeCompositeAlarm,
				At: sdkaws.ToTime(h.Timestamp), From: fromState, To: toState, Reason: reason,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// parseHistory reads the states a history entry moved between.
func parseHistory(data, summary string) (from, to, reason string) {
	var h struct {
		Old struct {
			Value string `json:"stateValue"`
		} `json:"oldState"`
		New struct {
			Value  string `json:"stateValue"`
			Reason string `json:"stateReason"`
		} `json:"newState"`
	}
	if json.Unmarshal([]byte(data), &h) == nil && h.New.Value != "" {
		return h.Old.Value, h.New.Value, h.New.Reason
	}
	// "Alarm updated from OK to ALARM"
	if _, rest, ok := strings.Cut(summary, " from "); ok {
		if f, t, ok := strings.Cut(rest, " to "); ok {
			return strings.TrimSpace(f), strings.TrimSpace(t), ""
		}
	}
	return "", "", ""
}

func (a *sdkAPI) Metric(ctx context.Context, q MetricQuery) ([]Datapoint, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	in := &cloudwatch.GetMetricStatisticsInput{
		Namespace: sdkaws.String(q.Namespace), MetricName: sdkaws.String(q.Metric),
		StartTime: sdkaws.Time(q.From), EndTime: sdkaws.Time(q.To), Period: sdkaws.Int32(q.Period),
	}
	for _, d := range q.Dimensions {
		in.Dimensions = append(in.Dimensions, cwtypes.Dimension{Name: sdkaws.String(d.Name), Value: sdkaws.String(d.Value)})
	}
	stat := q.Statistic
	if stat == "" {
		stat = "Average"
	}
	percentile := strings.HasPrefix(strings.ToLower(stat), "p")
	if percentile {
		in.ExtendedStatistics = []string{stat}
	} else {
		in.Statistics = []cwtypes.Statistic{cwtypes.Statistic(stat)}
	}
	out, err := cloudwatch.NewFromConfig(a.cfg).GetMetricStatistics(ctx, in)
	if err != nil {
		return nil, classify(err)
	}
	var pts []Datapoint
	for _, d := range out.Datapoints {
		var v *float64
		switch {
		case percentile:
			if x, ok := d.ExtendedStatistics[stat]; ok {
				v = &x
			}
		case stat == "Sum":
			v = d.Sum
		case stat == "Maximum":
			v = d.Maximum
		case stat == "Minimum":
			v = d.Minimum
		case stat == "SampleCount":
			v = d.SampleCount
		default:
			v = d.Average
		}
		if v != nil {
			pts = append(pts, Datapoint{At: sdkaws.ToTime(d.Timestamp), Value: *v, Unit: string(d.Unit)})
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].At.Before(pts[j].At) })
	return pts, nil
}

func (a *sdkAPI) DailyCost(ctx context.Context, from, to string) ([]DayCost, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out []DayCost
	err := a.costPages(ctx, from, to, nil, func(r cetypes.ResultByTime) {
		m := r.Total["UnblendedCost"]
		out = append(out, DayCost{Date: sdkaws.ToString(r.TimePeriod.Start), Amount: amount(m.Amount),
			Unit: sdkaws.ToString(m.Unit), Estimated: r.Estimated})
	})
	return out, err
}

func (a *sdkAPI) CostByService(ctx context.Context, from, to string) ([]ServiceCost, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	by := map[string]*ServiceCost{}
	group := []cetypes.GroupDefinition{{Type: cetypes.GroupDefinitionTypeDimension, Key: sdkaws.String("SERVICE")}}
	err := a.costPages(ctx, from, to, group, func(r cetypes.ResultByTime) {
		for _, g := range r.Groups {
			if len(g.Keys) == 0 {
				continue
			}
			m := g.Metrics["UnblendedCost"]
			sc, ok := by[g.Keys[0]]
			if !ok {
				sc = &ServiceCost{Service: g.Keys[0], Unit: sdkaws.ToString(m.Unit)}
				by[g.Keys[0]] = sc
			}
			sc.Amount += amount(m.Amount)
		}
	})
	if err != nil {
		return nil, err
	}
	out := make([]ServiceCost, 0, len(by))
	for _, sc := range by {
		out = append(out, *sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	return out, nil
}

// costPages reads daily unblended cost over [from, to), page by page.
func (a *sdkAPI) costPages(ctx context.Context, from, to string, group []cetypes.GroupDefinition, each func(cetypes.ResultByTime)) error {
	ce := costexplorer.NewFromConfig(a.cfg)
	in := &costexplorer.GetCostAndUsageInput{
		TimePeriod:  &cetypes.DateInterval{Start: sdkaws.String(from), End: sdkaws.String(to)},
		Granularity: cetypes.GranularityDaily,
		Metrics:     []string{"UnblendedCost"},
		GroupBy:     group,
	}
	for {
		out, err := ce.GetCostAndUsage(ctx, in)
		if err != nil {
			return classify(err)
		}
		for _, r := range out.ResultsByTime {
			each(r)
		}
		if out.NextPageToken == nil || *out.NextPageToken == "" {
			return nil
		}
		in.NextPageToken = out.NextPageToken
	}
}

func (a *sdkAPI) Anomalies(ctx context.Context, from string) ([]Anomaly, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	ce := costexplorer.NewFromConfig(a.cfg)
	in := &costexplorer.GetAnomaliesInput{DateInterval: &cetypes.AnomalyDateInterval{StartDate: sdkaws.String(from)}}
	var out []Anomaly
	for {
		page, err := ce.GetAnomalies(ctx, in)
		if err != nil {
			return nil, classify(err)
		}
		for _, x := range page.Anomalies {
			an := Anomaly{
				ID: sdkaws.ToString(x.AnomalyId), Start: sdkaws.ToString(x.AnomalyStartDate), End: sdkaws.ToString(x.AnomalyEndDate),
				Dimension: sdkaws.ToString(x.DimensionValue), MonitorARN: sdkaws.ToString(x.MonitorArn),
				Dismissed: x.Feedback == cetypes.AnomalyFeedbackTypeNo,
			}
			if x.Impact != nil {
				an.Impact = x.Impact.TotalImpact
				an.Actual = sdkaws.ToFloat64(x.Impact.TotalActualSpend)
				an.Expected = sdkaws.ToFloat64(x.Impact.TotalExpectedSpend)
			}
			if x.AnomalyScore != nil {
				an.Score = x.AnomalyScore.MaxScore
			}
			for _, rc := range x.RootCauses {
				var parts []string
				for _, s := range []*string{rc.Service, rc.Region, rc.UsageType, rc.LinkedAccountName} {
					if v := sdkaws.ToString(s); v != "" {
						parts = append(parts, v)
					}
				}
				if len(parts) > 0 {
					an.RootCauses = append(an.RootCauses, strings.Join(parts, " / "))
				}
			}
			out = append(out, an)
		}
		if page.NextPageToken == nil || *page.NextPageToken == "" {
			return out, nil
		}
		in.NextPageToken = page.NextPageToken
	}
}

func (a *sdkAPI) Budgets(ctx context.Context, accountID string) ([]Budget, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	client := budgets.NewFromConfig(a.cfg)
	var out []Budget
	p := budgets.NewDescribeBudgetsPaginator(client, &budgets.DescribeBudgetsInput{AccountId: sdkaws.String(accountID)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, classify(err)
		}
		for _, b := range page.Budgets {
			out = append(out, budget(b))
		}
	}
	for i := range out {
		np := budgets.NewDescribeNotificationsForBudgetPaginator(client, &budgets.DescribeNotificationsForBudgetInput{
			AccountId: sdkaws.String(accountID), BudgetName: sdkaws.String(out[i].Name),
		})
		for np.HasMorePages() {
			page, err := np.NextPage(ctx)
			if err != nil {
				return nil, classify(err)
			}
			for _, n := range page.Notifications {
				out[i].Notifications = append(out[i].Notifications, BudgetNotification{
					Type: string(n.NotificationType), Comparison: string(n.ComparisonOperator), Threshold: n.Threshold,
					ThresholdType: string(n.ThresholdType), InAlarm: n.NotificationState == btypes.NotificationStateAlarm,
				})
			}
		}
	}
	return out, nil
}

func budget(b btypes.Budget) Budget {
	out := Budget{Name: sdkaws.ToString(b.BudgetName), TimeUnit: string(b.TimeUnit)}
	if b.BudgetLimit != nil {
		out.Limit, out.Unit = amount(b.BudgetLimit.Amount), sdkaws.ToString(b.BudgetLimit.Unit)
	}
	if cs := b.CalculatedSpend; cs != nil {
		if cs.ActualSpend != nil {
			out.Actual = amount(cs.ActualSpend.Amount)
		}
		if cs.ForecastedSpend != nil {
			out.Forecast = amount(cs.ForecastedSpend.Amount)
		}
	}
	return out
}

func amount(s *string) float64 {
	v, _ := strconv.ParseFloat(sdkaws.ToString(s), 64)
	return v
}

// classify sorts an SDK error by what fixes it: logging in again (AuthError),
// being granted access (DeniedError), or waiting (the error as it is).
func classify(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "UnauthorizedException",
			"AuthorizationError", "AuthorizationErrorException":
			return &DeniedError{Err: err}
		case "ExpiredToken", "ExpiredTokenException", "InvalidClientTokenId", "UnrecognizedClientException",
			"InvalidSignatureException", "SignatureDoesNotMatch", "IncompleteSignature", "MissingAuthenticationToken":
			return &AuthError{Err: err}
		}
		return err
	}
	var signing *v4.SigningError
	if errors.As(err, &signing) {
		return classifyLogin(err)
	}
	return err
}

// classifyLogin is a credentials failure: a login to redo, unless the
// network kept it from being checked at all.
func classifyLogin(err error) error {
	var netErr net.Error
	var opErr *net.OpError
	if errors.As(err, &netErr) || errors.As(err, &opErr) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	return &AuthError{Err: err}
}
