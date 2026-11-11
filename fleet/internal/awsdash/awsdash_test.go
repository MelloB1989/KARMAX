package awsdash

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// fakeCLI answers each command line from its fixtures: out is what it
// prints, fail what it fails with; anything else prints {} (nothing there).
type fakeCLI struct {
	mu    sync.Mutex
	out   map[string]string
	fail  map[string]string
	calls []string
}

func (f *fakeCLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	line := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, line)
	if msg, ok := f.fail[line]; ok {
		return nil, errors.New(msg)
	}
	if out, ok := f.out[line]; ok {
		return []byte(out), nil
	}
	return []byte("{}"), nil
}

// called is how many calls began with prefix.
func (f *fakeCLI) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// now is the tests' clock: 25 October 2026, evening.
var now = time.Date(2026, 10, 25, 21, 4, 0, 0, time.UTC)

const (
	daily     = "ce get-cost-and-usage --time-period Start=2026-09-01,End=2026-10-26 --granularity DAILY --metrics UnblendedCost --group-by Type=DIMENSION,Key=RECORD_TYPE --region us-east-1"
	byService = "ce get-cost-and-usage --time-period Start=2026-10-01,End=2026-10-26 --granularity MONTHLY --metrics UnblendedCost --group-by Type=DIMENSION,Key=SERVICE --filter " + noCredits + " --region us-east-1"
	forecast  = "ce get-cost-forecast --time-period Start=2026-10-25,End=2026-11-01 --metric UNBLENDED_COST --granularity MONTHLY --region us-east-1"
)

// dailyCost is September's usage at $2 a day and October's at $3 a day so
// far (today, the 25th, $1 so far), with a $1 credit on each of October's
// first ten days and tax on the 1st.
func dailyCost() string {
	group := func(kind, amt string) string {
		return fmt.Sprintf(`{"Keys":["%s"],"Metrics":{"UnblendedCost":{"Amount":"%s","Unit":"USD"}}}`, kind, amt)
	}
	var days []string
	for d := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); d.Before(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		groups := []string{group("Usage", "2.0")}
		switch {
		case d.Month() == 10 && d.Day() == 1:
			groups = []string{group("Usage", "2.5"), group("Tax", "0.5"), group("Credit", "-1.0")}
		case d.Month() == 10 && d.Day() <= 10:
			groups = []string{group("Usage", "3.0"), group("Credit", "-1.0")}
		case d.Month() == 10 && d.Day() == 25:
			groups = []string{group("Usage", "1.0")}
		case d.Month() == 10:
			groups = []string{group("Usage", "3.0")}
		}
		days = append(days, fmt.Sprintf(`{"TimePeriod":{"Start":"%s","End":"%s"},"Total":{},"Groups":[%s]}`,
			d.Format("2006-01-02"), d.AddDate(0, 0, 1).Format("2006-01-02"), strings.Join(groups, ",")))
	}
	return `{"ResultsByTime":[` + strings.Join(days, ",") + `]}`
}

func fixtures() *fakeCLI {
	return &fakeCLI{out: map[string]string{
		"sts get-caller-identity --region us-east-1": `{"UserId":"AIDAEXAMPLE","Account":"123456789012","Arn":"arn:aws:iam::123456789012:user/ops/fleet-reader"}`,
		"ec2 describe-instances --region ap-south-1": `{"Reservations":[{"Instances":[
			{"InstanceId":"i-0aaa","InstanceType":"t3.medium","State":{"Name":"running"},"LaunchTime":"2026-09-14T10:00:00+00:00",
			 "PrivateIpAddress":"10.0.1.12","PublicIpAddress":"203.0.113.7","Tags":[{"Key":"env","Value":"prod"},{"Key":"Name","Value":"api-prod-1"}]},
			{"InstanceId":"i-0bbb","InstanceType":"t3.micro","State":{"Name":"stopped"},"LaunchTime":"2026-01-02T03:04:05.000Z","PrivateIpAddress":"10.0.1.40"}]}]}`,
		"lambda list-functions --query " + lambdaQuery + " --region ap-south-1": `[
			{"FunctionName":"thumbnailer","Runtime":"python3.12","MemorySize":1024,"LastModified":"2026-10-04T18:30:43.000+0000","PackageType":"Zip","Architectures":["arm64"]},
			{"FunctionName":"pdf-export","MemorySize":2048,"LastModified":"2026-10-20T08:00:00.000+0000","PackageType":"Image","Architectures":["x86_64"]}]`,
		"rds describe-db-instances --region ap-south-1": `{"DBInstances":[{"DBInstanceIdentifier":"orders-db","DBInstanceClass":"db.t4g.medium","DBInstanceStatus":"available",
			"Engine":"postgres","EngineVersion":"16.4","AllocatedStorage":100,"MultiAZ":true,"InstanceCreateTime":"2025-03-01T00:00:00+00:00"}]}`,
		"ecs list-clusters --region ap-south-1": `{"clusterArns":["arn:aws:ecs:ap-south-1:123456789012:cluster/prod-cluster"]}`,
		"ecs describe-clusters --clusters arn:aws:ecs:ap-south-1:123456789012:cluster/prod-cluster --region ap-south-1": `{"clusters":[
			{"clusterName":"prod-cluster","status":"ACTIVE","runningTasksCount":7,"pendingTasksCount":0,"activeServicesCount":4}]}`,
		"eks list-clusters --region us-east-1":                             `{"clusters":["sandbox-eks"]}`,
		"dynamodb list-tables --region ap-south-1":                         `{"TableNames":["sessions","rate-limits"]}`,
		"elbv2 describe-load-balancers --region ap-south-1":                `{"LoadBalancers":[{"LoadBalancerName":"api-prod-alb","Type":"application","Scheme":"internet-facing","DNSName":"api-prod-alb-1.ap-south-1.elb.amazonaws.com","State":{"Code":"active"},"CreatedTime":"2025-06-01T00:00:00+00:00"}]}`,
		"apigatewayv2 get-apis --region ap-south-1":                        `{"Items":[{"ApiId":"a1b2c3","Name":"payments-api","ProtocolType":"HTTP","ApiEndpoint":"https://a1b2c3.execute-api.ap-south-1.amazonaws.com","CreatedDate":"2025-08-01T00:00:00+00:00"}]}`,
		"apigateway get-rest-apis --region us-east-1":                      `{"items":[{"id":"r3st01","name":"legacy-rest","createdDate":"2024-02-01T00:00:00+00:00","endpointConfiguration":{"types":["EDGE"]}}]}`,
		"ecr describe-repositories --region ap-south-1":                    `{"repositories":[{"repositoryName":"api","createdAt":"2025-01-01T00:00:00+00:00","imageTagMutability":"IMMUTABLE"}]}`,
		"cognito-idp list-user-pools --max-results 60 --region ap-south-1": `{"UserPools":[{"Id":"ap-south-1_EXAMPLE","Name":"example-users","CreationDate":"2025-04-01T00:00:00+00:00"}]}`,
		"sqs list-queues --region ap-south-1":                              `{"QueueUrls":["https://sqs.ap-south-1.amazonaws.com/123456789012/jobs","https://sqs.ap-south-1.amazonaws.com/123456789012/events.fifo"]}`,
		"sns list-topics --region ap-south-1":                              `{"Topics":[{"TopicArn":"arn:aws:sns:ap-south-1:123456789012:alerts"}]}`,
		"s3api list-buckets --max-buckets 1000 --region us-east-1": `{"Buckets":[{"Name":"example-logs","CreationDate":"2024-05-01T00:00:00+00:00","BucketRegion":"ap-south-1"},
			{"Name":"example-assets","CreationDate":"2023-05-01T00:00:00+00:00"}]}`,
		"cloudfront list-distributions --region us-east-1": `{"DistributionList":{"Items":[
			{"Id":"E1EXAMPLE","DomainName":"d111111abcdef8.cloudfront.net","Status":"Deployed","Enabled":true,"Comment":"site","Aliases":{"Quantity":1,"Items":["cdn.example.com"]},"LastModifiedTime":"2026-07-15T02:45:12.484000+00:00"},
			{"Id":"E2EXAMPLE","DomainName":"d222222abcdef8.cloudfront.net","Status":"Deployed","Enabled":false,"Comment":"old site","Aliases":{"Quantity":0}}]}}`,
		daily: dailyCost(),
		byService: `{"ResultsByTime":[{"Groups":[
			{"Keys":["Amazon Relational Database Service"],"Metrics":{"UnblendedCost":{"Amount":"20.5","Unit":"USD"}}},
			{"Keys":["Amazon Elastic Compute Cloud - Compute"],"Metrics":{"UnblendedCost":{"Amount":"48.25","Unit":"USD"}}},
			{"Keys":["AWS Cost Explorer"],"Metrics":{"UnblendedCost":{"Amount":"0.0001","Unit":"USD"}}},
			{"Keys":["Tax"],"Metrics":{"UnblendedCost":{"Amount":"4.25","Unit":"USD"}}}]}]}`,
		forecast: `{"Total":{"Amount":"25.5","Unit":"USD"}}`,
	}}
}

func newDash(f *fakeCLI, clock *time.Time) *Dash {
	return New(Options{Run: f, Regions: []string{"ap-south-1", "us-east-1"}, Now: func() time.Time { return *clock }})
}

func find(r *fleetapi.AWSReport, service, id string) (fleetapi.AWSResource, bool) {
	for _, it := range r.Items {
		if it.Service == service && it.ID == id {
			return it, true
		}
	}
	return fleetapi.AWSResource{}, false
}

func TestAReportReadsTheWholeAccount(t *testing.T) {
	f, clock := fixtures(), now
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Account != "123456789012" || r.User != "fleet-reader" || !r.At.Equal(now) {
		t.Errorf("who: %q %q at %s", r.Account, r.User, r.At)
	}
	if strings.Join(r.Regions, ",") != "ap-south-1,us-east-1" {
		t.Errorf("regions %v", r.Regions)
	}
	if len(r.Errors) != 0 {
		t.Errorf("errors: %q", r.Errors)
	}
	want := []fleetapi.AWSResource{
		{Service: "ec2", Region: "ap-south-1", ID: "i-0aaa", Name: "api-prod-1", Type: "t3.medium", State: "running", Detail: "10.0.1.12 · 203.0.113.7", Created: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)},
		{Service: "ec2", Region: "ap-south-1", ID: "i-0bbb", Type: "t3.micro", State: "stopped", Detail: "10.0.1.40", Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{Service: "lambda", Region: "ap-south-1", ID: "thumbnailer", Type: "python3.12", Detail: "1024 MB · arm64", Created: time.Date(2026, 10, 4, 18, 30, 43, 0, time.UTC)},
		{Service: "lambda", Region: "ap-south-1", ID: "pdf-export", Type: "image", Detail: "2048 MB · x86_64", Created: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)},
		{Service: "rds", Region: "ap-south-1", ID: "orders-db", Type: "db.t4g.medium", State: "available", Detail: "postgres 16.4 · 100 GB · multi-AZ", Created: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "ecs", Region: "ap-south-1", ID: "prod-cluster", State: "active", Detail: "4 services · 7 tasks running"},
		{Service: "eks", Region: "us-east-1", ID: "sandbox-eks"},
		{Service: "dynamodb", Region: "ap-south-1", ID: "sessions"},
		{Service: "elb", Region: "ap-south-1", ID: "api-prod-alb", Type: "application", State: "active", Detail: "internet-facing · api-prod-alb-1.ap-south-1.elb.amazonaws.com", Created: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "apigateway", Region: "ap-south-1", ID: "a1b2c3", Name: "payments-api", Type: "HTTP", Detail: "https://a1b2c3.execute-api.ap-south-1.amazonaws.com", Created: time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "apigateway", Region: "us-east-1", ID: "r3st01", Name: "legacy-rest", Type: "REST", Detail: "EDGE", Created: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "ecr", Region: "ap-south-1", ID: "api", Detail: "immutable tags", Created: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "cognito", Region: "ap-south-1", ID: "ap-south-1_EXAMPLE", Name: "example-users", Created: time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "sqs", Region: "ap-south-1", ID: "events.fifo", Type: "fifo"},
		{Service: "sqs", Region: "ap-south-1", ID: "jobs", Type: "standard"},
		{Service: "sns", Region: "ap-south-1", ID: "alerts"},
		{Service: "s3", Region: "ap-south-1", ID: "example-logs", Created: time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "s3", Region: "global", ID: "example-assets", Created: time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC)},
		{Service: "cloudfront", Region: "global", ID: "E1EXAMPLE", Name: "cdn.example.com", State: "Deployed", Detail: "d111111abcdef8.cloudfront.net", Created: time.Date(2026, 7, 15, 2, 45, 12, 484000000, time.UTC)},
		{Service: "cloudfront", Region: "global", ID: "E2EXAMPLE", Name: "old site", State: "disabled", Detail: "d222222abcdef8.cloudfront.net"},
	}
	for _, w := range want {
		got, ok := find(r, w.Service, w.ID)
		if !ok {
			t.Errorf("no %s %s", w.Service, w.ID)
			continue
		}
		if !got.Created.Equal(w.Created) {
			t.Errorf("%s %s created %s, want %s", w.Service, w.ID, got.Created, w.Created)
		}
		got.Created, w.Created = time.Time{}, time.Time{}
		if got != w {
			t.Errorf("%s %s:\n got %+v\nwant %+v", w.Service, w.ID, got, w)
		}
	}
	if len(r.Items) != len(want)+1 { // and the rate-limits table
		t.Errorf("%d items, want %d", len(r.Items), len(want)+1)
	}
}

// Items come by service, then region, then name.
func TestItemsAreSortedByServiceRegionAndName(t *testing.T) {
	f, clock := fixtures(), now
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	ok := slices.IsSortedFunc(r.Items, func(a, b fleetapi.AWSResource) int {
		return strings.Compare(a.Service+"\x00"+a.Region+"\x00"+label(a), b.Service+"\x00"+b.Region+"\x00"+label(b))
	})
	if !ok {
		var got []string
		for _, it := range r.Items {
			got = append(got, it.Service+"/"+it.Region+"/"+label(it))
		}
		t.Errorf("not sorted: %v", got)
	}
}

func TestTheMonthsCost(t *testing.T) {
	f, clock := fixtures(), now
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	c := r.Cost
	if c == nil {
		t.Fatal("no cost")
	}
	// What was used before credits: October, 24 days at $3 (tax in), and $1
	// today; September, 30 days at $2. The credits are said apart.
	if c.Currency != "USD" || c.MonthToDate != 73 || c.LastMonth != 60 || c.Credits != 10 || !c.At.Equal(now) {
		t.Errorf("cost %+v", c)
	}
	// The forecast is from today: the month so far before today, and it.
	if c.Forecast != 72+25.5 {
		t.Errorf("forecast %v", c.Forecast)
	}
	var svc []string
	for _, a := range c.ByService {
		svc = append(svc, fmt.Sprintf("%s=%.2f", a.Name, a.Amount))
	}
	if got := strings.Join(svc, " "); got != "Amazon Elastic Compute Cloud - Compute=48.25 Amazon Relational Database Service=20.50 Tax=4.25" {
		t.Errorf("by service: %s", got)
	}
	if len(c.Daily) != 14 || c.Daily[0].Name != "2026-10-12" || c.Daily[13].Name != "2026-10-25" || c.Daily[13].Amount != 1 || c.Daily[0].Amount != 3 {
		t.Errorf("daily %+v", c.Daily)
	}
}

// Cost Explorer charges a cent a call: it is read every CostEvery, not
// with the resources, and a refresh reads it only when it is an hour old.
func TestCostIsReadSeldom(t *testing.T) {
	f, clock := fixtures(), now
	d := newDash(f, &clock)
	read := func(refresh bool) {
		t.Helper()
		if _, err := d.Report(context.Background(), refresh); err != nil {
			t.Fatal(err)
		}
		d.Wait() // an old read goes on behind the answer
	}
	read(false)
	read(false)
	if n := f.called("ec2 describe-instances"); n != 2 { // 2 regions, once
		t.Errorf("resources read %d times, want once", n/2)
	}
	clock = now.Add(6 * time.Minute)
	read(false)
	if n := f.called("ec2 describe-instances"); n != 4 {
		t.Errorf("resources read %d times after 6m, want twice", n/2)
	}
	read(true)
	if n := f.called("ec2 describe-instances"); n != 6 {
		t.Errorf("a refresh did not read the resources: %d", n/2)
	}
	if n := f.called("ce "); n != 3 {
		t.Errorf("cost explorer called %d times, want 3 (one read)", n)
	}
	clock = now.Add(61 * time.Minute)
	read(false)
	if n := f.called("ce "); n != 3 {
		t.Errorf("cost read again within 6h without a refresh: %d", n)
	}
	read(true)
	if n := f.called("ce "); n != 6 {
		t.Errorf("a refresh did not read an hour-old cost: %d", n)
	}
	clock = now.Add(61*time.Minute + 6*time.Hour)
	read(false)
	if n := f.called("ce "); n != 9 {
		t.Errorf("cost not read again after 6h: %d", n)
	}
}

// A part that fails is named in Errors; the rest is still shown.
func TestAFailedPartIsAnErrorTheRestIsShown(t *testing.T) {
	f, clock := fixtures(), now
	f.fail = map[string]string{
		"lambda list-functions --query " + lambdaQuery + " --region ap-south-1": "AccessDeniedException: User: arn:aws:iam::123456789012:user/ops/fleet-reader is not authorized to perform: lambda:ListFunctions on resource: * because no identity-based policy allows the lambda:ListFunctions action",
		forecast: "AccessDeniedException: no ce:GetCostForecast",
	}
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) != 2 || !strings.HasPrefix(r.Errors[0], "lambda ap-south-1: AccessDeniedException: User: arn:aws:iam::") ||
		r.Errors[1] != "cost forecast: AccessDeniedException: no ce:GetCostForecast" {
		t.Errorf("errors %q", r.Errors)
	}
	if n := len([]rune(r.Errors[0])); n > maxErr {
		t.Errorf("an error of %d runes", n)
	}
	if _, ok := find(r, "ec2", "i-0aaa"); !ok {
		t.Error("ec2 is not shown")
	}
	if r.Cost == nil || r.Cost.MonthToDate != 73 || r.Cost.Forecast != 0 {
		t.Errorf("cost %+v", r.Cost)
	}
}

// The CLI prints nothing for an empty list (sqs list-queues): that is no
// queues, not an error.
func TestAnEmptyAnswerIsNothingThere(t *testing.T) {
	f, clock := fixtures(), now
	f.out["sqs list-queues --region ap-south-1"] = ""
	f.out["lambda list-functions --query "+lambdaQuery+" --region us-east-1"] = "\n"
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil || len(r.Errors) != 0 {
		t.Fatalf("%v %q", err, r.Errors)
	}
	if _, ok := find(r, "sqs", "jobs"); ok {
		t.Error("a queue from nothing")
	}
}

// A new account has no forecast yet: that is not an error.
func TestNoForecastYetIsQuiet(t *testing.T) {
	f, clock := fixtures(), now
	f.fail = map[string]string{forecast: "DataUnavailableException: Insufficient amount of historical data to generate forecast."}
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil || len(r.Errors) != 0 || r.Cost == nil || r.Cost.Forecast != 0 {
		t.Errorf("%v %q %+v", err, r.Errors, r.Cost)
	}
}

// Keys that do not work fail every call: one error says so, and nothing
// else is tried.
func TestKeysThatDoNotWorkStopTheRead(t *testing.T) {
	f, clock := fixtures(), now
	f.fail = map[string]string{"sts get-caller-identity --region us-east-1": "InvalidClientTokenId: The security token included in the request is invalid."}
	r, err := newDash(f, &clock).Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Errors) != 1 || r.Errors[0] != "sts: InvalidClientTokenId: The security token included in the request is invalid." {
		t.Errorf("errors %q", r.Errors)
	}
	if len(f.calls) != 1 || r.Cost != nil || len(r.Items) != 0 {
		t.Errorf("went on after the identity failed: %q", f.calls)
	}
}

// regions: [all] is every region the account has enabled.
func TestAllRegions(t *testing.T) {
	f, clock := fixtures(), now
	f.out["ec2 describe-regions --region us-east-1"] = `{"Regions":[{"RegionName":"us-east-1"},{"RegionName":"eu-west-1"},{"RegionName":"ap-south-1"}]}`
	d := New(Options{Run: f, Regions: []string{"all"}, Now: func() time.Time { return clock }})
	r, err := d.Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Regions, ",") != "ap-south-1,eu-west-1,us-east-1" {
		t.Errorf("regions %v", r.Regions)
	}
	if f.called("ec2 describe-instances --region eu-west-1") != 1 {
		t.Error("eu-west-1 was not read")
	}
}

// Reads run a few at a time.
func TestReadsAreBounded(t *testing.T) {
	g := &gauge{fakeCLI: fixtures()}
	clock := now
	d := New(Options{Run: g, Regions: []string{"all"}, Now: func() time.Time { return clock }})
	var regions []string
	for i := range 6 {
		regions = append(regions, fmt.Sprintf(`{"RegionName":"xx-test-%d"}`, i+1))
	}
	g.out["ec2 describe-regions --region us-east-1"] = `{"Regions":[` + strings.Join(regions, ",") + `]}`
	if _, err := d.Report(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if g.most > parallel || g.most < 2 {
		t.Errorf("%d calls at once, want 2 to %d", g.most, parallel)
	}
}

// gauge is a fakeCLI that counts the calls running at once.
type gauge struct {
	*fakeCLI
	mu        sync.Mutex
	now, most int
}

func (g *gauge) Run(ctx context.Context, args ...string) ([]byte, error) {
	g.mu.Lock()
	g.now++
	g.most = max(g.most, g.now)
	g.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	defer func() { g.mu.Lock(); g.now--; g.mu.Unlock() }()
	return g.fakeCLI.Run(ctx, args...)
}

// Without a fake, the keys come from keys_csv; a file that cannot be read
// fails the report.
func TestNoKeysNoReport(t *testing.T) {
	d := New(Options{KeysCSV: "/nonexistent/keys.csv", CLI: "/bin/false"})
	if _, err := d.Report(context.Background(), false); err == nil || !strings.Contains(err.Error(), "keys_csv") {
		t.Errorf("err = %v", err)
	}
}

// What is old is answered from the cache at once and read again behind it.
func TestAStaleReadIsAnsweredFromTheCache(t *testing.T) {
	f, clock := fixtures(), now
	d := newDash(f, &clock)
	first, err := d.Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(10 * time.Minute)
	got, err := d.Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.At.Equal(first.At) || len(got.Items) != len(first.Items) {
		t.Errorf("an old read was waited for: at %v, was %v", got.At, first.At)
	}
	d.Wait()
	if again, _ := d.Report(context.Background(), false); !again.At.Equal(clock) {
		t.Errorf("not read again behind the answer: at %v, want %v", again.At, clock)
	}
}

// The last read is on disk: a new fleetd shows it before it reads anything.
func TestTheCacheSurvivesARestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "aws-cache.json")
	f, clock := fixtures(), now
	d := New(Options{Run: f, Regions: []string{"ap-south-1", "us-east-1"}, CacheFile: file, Now: func() time.Time { return clock }})
	first, err := d.Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", fi, err)
	}
	g := &fakeCLI{out: map[string]string{}}
	e := New(Options{Run: g, Regions: []string{"ap-south-1", "us-east-1"}, CacheFile: file, Now: func() time.Time { return clock.Add(time.Minute) }})
	got, err := e.Report(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != len(first.Items) || got.Cost == nil || got.Account != first.Account {
		t.Errorf("not restored: %d items, cost %v", len(got.Items), got.Cost)
	}
	if n := g.called(""); n != 0 {
		t.Errorf("a fresh cache was read again: %d calls", n)
	}
}
