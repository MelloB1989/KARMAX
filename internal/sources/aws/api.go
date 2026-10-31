package aws

import (
	"context"
	"errors"
	"time"
)

// API is everything this source reads from one profile in one region. The
// SDK implements it (sdk.go); tests use a fake. Every method is a read.
type API interface {
	// Identity is the account the credentials belong to. It is also the
	// login check: an AuthError means the profile needs logging in again.
	Identity(ctx context.Context) (string, error)

	Alarms(ctx context.Context) ([]Alarm, error)
	// AlarmHistory is every alarm's state changes in [from, to), oldest first.
	AlarmHistory(ctx context.Context, from, to time.Time) ([]Transition, error)
	Metric(ctx context.Context, q MetricQuery) ([]Datapoint, error)

	// DailyCost is the unblended cost of each UTC day in [from, to), dates as
	// YYYY-MM-DD. Each call is billed by Cost Explorer.
	DailyCost(ctx context.Context, from, to string) ([]DayCost, error)
	// CostByService is the unblended cost per service over [from, to). Billed.
	CostByService(ctx context.Context, from, to string) ([]ServiceCost, error)
	// Anomalies are what Cost Anomaly Detection found from from on. Billed.
	Anomalies(ctx context.Context, from string) ([]Anomaly, error)
	// Budgets are the account's budgets with the state of each notification.
	Budgets(ctx context.Context, accountID string) ([]Budget, error)
}

// Dialer opens the API for a profile in a region.
type Dialer func(ctx context.Context, p Profile, region string) (API, error)

// AuthError is credentials that did not work: an expired SSO session, a
// revoked key. The profile is skipped until someone logs in again.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return "not logged in: " + e.Err.Error() }
func (e *AuthError) Unwrap() error { return e.Err }

// DeniedError is credentials that work but may not make this call.
type DeniedError struct{ Err error }

func (e *DeniedError) Error() string { return "access denied: " + e.Err.Error() }
func (e *DeniedError) Unwrap() error { return e.Err }

func isAuth(err error) bool   { var a *AuthError; return errors.As(err, &a) }
func isDenied(err error) bool { var d *DeniedError; return errors.As(err, &d) }

// Alarm is a CloudWatch alarm as it stands now.
type Alarm struct {
	Name        string
	ARN         string
	Description string
	Composite   bool
	Rule        string // a composite alarm's rule
	State       string // ALARM | OK | INSUFFICIENT_DATA
	Reason      string
	UpdatedAt   time.Time

	Namespace         string
	Metric            string
	Dimensions        []Dimension
	Statistic         string
	Period            int32 // seconds
	EvaluationPeriods int32
	Comparison        string
	Threshold         *float64
	// Expressions holds a metric-math alarm's queries, which have no single
	// metric of their own.
	Expressions []string
}

// Dimension is one name=value pair of a metric.
type Dimension struct{ Name, Value string }

// Transition is one state change from an alarm's history.
type Transition struct {
	Alarm     string
	Composite bool
	At        time.Time
	From, To  string
	Reason    string
}

// MetricQuery asks for a metric's recent datapoints.
type MetricQuery struct {
	Namespace  string
	Metric     string
	Dimensions []Dimension
	Statistic  string // Average, Sum, Maximum, Minimum, SampleCount; or a pNN percentile
	Period     int32  // seconds
	From, To   time.Time
}

// Datapoint is one value of a metric.
type Datapoint struct {
	At    time.Time
	Value float64
	Unit  string
}

// DayCost is one UTC day's cost.
type DayCost struct {
	Date      string // YYYY-MM-DD
	Amount    float64
	Unit      string // USD
	Estimated bool   // AWS has not finalised the day yet
}

// ServiceCost is one service's share of the cost over a period.
type ServiceCost struct {
	Service string
	Amount  float64
	Unit    string
}

// Anomaly is one finding of Cost Anomaly Detection.
type Anomaly struct {
	ID         string
	Start, End string // dates
	Dimension  string // what the monitor watches, e.g. a service
	RootCauses []string
	Impact     float64 // actual minus expected spend
	Actual     float64
	Expected   float64
	Score      float64
	Dismissed  bool // the operator marked it as not an anomaly
	MonitorARN string
}

// Budget is one AWS Budget and where its spend stands.
type Budget struct {
	Name          string
	TimeUnit      string // DAILY | MONTHLY | QUARTERLY | ANNUALLY
	Limit         float64
	Actual        float64
	Forecast      float64
	Unit          string
	Notifications []BudgetNotification
}

// BudgetNotification is one threshold set on a budget.
type BudgetNotification struct {
	Type          string // ACTUAL | FORECASTED
	Comparison    string // GREATER_THAN, LESS_THAN, EQUAL_TO
	Threshold     float64
	ThresholdType string // PERCENTAGE | ABSOLUTE_VALUE
	InAlarm       bool   // AWS reports the threshold as crossed
}

// dateLayout is how Cost Explorer and Budgets write a day.
const dateLayout = "2006-01-02"
