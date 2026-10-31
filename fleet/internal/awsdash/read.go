package awsdash

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// identity is the account and the name (the ARN's last part) the keys are.
func identity(ctx context.Context, c *caller) (account, user string, err error) {
	b, err := c.call(ctx, "sts", "get-caller-identity", "--region", homeRegion)
	if err != nil {
		return "", "", err
	}
	var v struct{ Account, Arn string }
	if err := json.Unmarshal(b, &v); err != nil {
		return "", "", fmt.Errorf("unreadable answer: %w", err)
	}
	return v.Account, path.Base(v.Arn), nil
}

// enabledRegions is every region the account has enabled, sorted.
func enabledRegions(ctx context.Context, c *caller) ([]string, error) {
	b, err := c.call(ctx, "ec2", "describe-regions", "--region", homeRegion)
	if err != nil {
		return nil, err
	}
	var v struct{ Regions []struct{ RegionName string } }
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("unreadable answer: %w", err)
	}
	var out []string
	for _, r := range v.Regions {
		out = append(out, r.RegionName)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no regions")
	}
	slices.Sort(out)
	return out, nil
}

// lister reads one service in one region.
type lister struct {
	label string // what its error is named: lambda, apigatewayv2…
	read  func(ctx context.Context, c *caller, region string) ([]fleetapi.AWSResource, error)
}

// list is a lister of one CLI command, its answer parsed by parse.
func list(args []string, parse func(b []byte, region string) ([]fleetapi.AWSResource, error)) lister {
	return lister{label: args[0], read: func(ctx context.Context, c *caller, region string) ([]fleetapi.AWSResource, error) {
		b, err := c.call(ctx, append(slices.Clone(args), "--region", region)...)
		if err != nil {
			return nil, err
		}
		return parse(b, region)
	}}
}

// lambdaQuery keeps what the tab shows of a function: never its
// environment, which holds secrets.
const lambdaQuery = "Functions[].{FunctionName:FunctionName,Runtime:Runtime,MemorySize:MemorySize,LastModified:LastModified,PackageType:PackageType,Architectures:Architectures}"

// regional is what is read in each region.
var regional = []lister{
	list([]string{"ec2", "describe-instances"}, parseEC2),
	list([]string{"lambda", "list-functions", "--query", lambdaQuery}, parseLambda),
	list([]string{"rds", "describe-db-instances"}, parseRDS),
	{label: "ecs", read: readECS},
	list([]string{"eks", "list-clusters"}, names("eks", "clusters")),
	list([]string{"dynamodb", "list-tables"}, names("dynamodb", "TableNames")),
	list([]string{"elbv2", "describe-load-balancers"}, parseELB),
	list([]string{"apigatewayv2", "get-apis"}, parseHTTPAPIs),
	list([]string{"apigateway", "get-rest-apis"}, parseRESTAPIs),
	list([]string{"ecr", "describe-repositories"}, parseECR),
	list([]string{"cognito-idp", "list-user-pools", "--max-results", "60"}, parseCognito),
	list([]string{"sqs", "list-queues"}, parseSQS),
	list([]string{"sns", "list-topics"}, parseSNS),
}

// global is what is read once, in homeRegion.
var global = []lister{
	{label: "s3", read: list([]string{"s3api", "list-buckets", "--max-buckets", "1000"}, parseS3).read},
	list([]string{"cloudfront", "list-distributions"}, parseCloudFront),
}

// readResources reads every lister in every region (and the global ones),
// parallel at a time, and returns what it found, sorted, and what it
// could not read.
func readResources(ctx context.Context, c *caller, regions []string) ([]fleetapi.AWSResource, []string) {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		items = []fleetapi.AWSResource{}
		errs  []string
	)
	run := func(l lister, region, where string) {
		wg.Go(func() {
			got, err := l.read(ctx, c, region)
			mu.Lock()
			defer mu.Unlock()
			items = append(items, got...)
			if err != nil {
				errs = append(errs, trim(l.label+" "+where+": "+err.Error()))
			}
		})
	}
	for _, r := range regions {
		for _, l := range regional {
			run(l, r, r)
		}
	}
	for _, l := range global {
		run(l, homeRegion, "global")
	}
	wg.Wait()
	slices.SortFunc(items, func(a, b fleetapi.AWSResource) int {
		return cmpItems(a, b)
	})
	slices.Sort(errs)
	return items, errs
}

func cmpItems(a, b fleetapi.AWSResource) int {
	for _, p := range [][2]string{{a.Service, b.Service}, {a.Region, b.Region}, {label(a), label(b)}, {a.ID, b.ID}} {
		if c := strings.Compare(p[0], p[1]); c != 0 {
			return c
		}
	}
	return 0
}

// label is what a resource is called: its name, else its ID.
func label(r fleetapi.AWSResource) string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

// awsTime is a time as the CLI prints it: ISO 8601 in one of a few forms.
type awsTime struct{ time.Time }

func (t *awsTime) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil || s == "" {
		return nil // a number or null: unknown
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05.999999999Z07:00"} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v.UTC()
			return nil
		}
	}
	return nil
}

func unreadable(err error) error { return fmt.Errorf("unreadable answer: %w", err) }

// joined is the non-empty parts, " · " between them.
func joined(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}

func parseEC2(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		Reservations []struct {
			Instances []struct {
				InstanceID       string `json:"InstanceId"`
				InstanceType     string
				State            struct{ Name string }
				LaunchTime       awsTime
				PrivateIPAddress string `json:"PrivateIpAddress"`
				PublicIPAddress  string `json:"PublicIpAddress"`
				Tags             []struct{ Key, Value string }
			}
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, r := range v.Reservations {
		for _, in := range r.Instances {
			it := fleetapi.AWSResource{Service: "ec2", Region: region, ID: in.InstanceID, Type: in.InstanceType, State: in.State.Name,
				Detail: joined(in.PrivateIPAddress, in.PublicIPAddress), Created: in.LaunchTime.Time}
			for _, t := range in.Tags {
				if t.Key == "Name" {
					it.Name = t.Value
				}
			}
			out = append(out, it)
		}
	}
	return out, nil
}

// parseLambda reads lambdaQuery's answer. Lambda says when a function was
// last changed, not made: that is its Created.
func parseLambda(b []byte, region string) ([]fleetapi.AWSResource, error) {
	type function struct {
		FunctionName, Runtime, PackageType string
		MemorySize                         int
		LastModified                       awsTime
		Architectures                      []string
	}
	var v []function
	if err := json.Unmarshal(b, &v); err != nil {
		// Without the query (a CLI that ignored it): the whole answer.
		var all struct{ Functions []function }
		if json.Unmarshal(b, &all) != nil {
			return nil, unreadable(err)
		}
		v = all.Functions
	}
	var out []fleetapi.AWSResource
	for _, f := range v {
		typ := f.Runtime
		if f.PackageType == "Image" {
			typ = "image"
		}
		arch := ""
		if len(f.Architectures) > 0 {
			arch = f.Architectures[0]
		}
		out = append(out, fleetapi.AWSResource{Service: "lambda", Region: region, ID: f.FunctionName, Type: typ,
			Detail: joined(fmt.Sprintf("%d MB", f.MemorySize), arch), Created: f.LastModified.Time})
	}
	return out, nil
}

func parseRDS(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		DBInstances []struct {
			DBInstanceIdentifier, DBInstanceClass, DBInstanceStatus, Engine, EngineVersion string
			AllocatedStorage                                                               int
			MultiAZ                                                                        bool
			InstanceCreateTime                                                             awsTime
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, d := range v.DBInstances {
		az := ""
		if d.MultiAZ {
			az = "multi-AZ"
		}
		out = append(out, fleetapi.AWSResource{Service: "rds", Region: region, ID: d.DBInstanceIdentifier, Type: d.DBInstanceClass,
			State:  d.DBInstanceStatus,
			Detail: joined(strings.TrimSpace(d.Engine+" "+d.EngineVersion), fmt.Sprintf("%d GB", d.AllocatedStorage), az), Created: d.InstanceCreateTime.Time})
	}
	return out, nil
}

// readECS lists the clusters, then describes them for their services and
// tasks; when that fails, the clusters are still shown.
func readECS(ctx context.Context, c *caller, region string) ([]fleetapi.AWSResource, error) {
	b, err := c.call(ctx, "ecs", "list-clusters", "--region", region)
	if err != nil {
		return nil, err
	}
	var l struct {
		ClusterArns []string `json:"clusterArns"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, unreadable(err)
	}
	if len(l.ClusterArns) == 0 {
		return nil, nil
	}
	var out []fleetapi.AWSResource
	for _, arn := range l.ClusterArns {
		out = append(out, fleetapi.AWSResource{Service: "ecs", Region: region, ID: path.Base(arn)})
	}
	args := append([]string{"ecs", "describe-clusters", "--clusters"}, l.ClusterArns[:min(len(l.ClusterArns), 100)]...)
	b, err = c.call(ctx, append(args, "--region", region)...)
	if err != nil {
		return out, err
	}
	var d struct {
		Clusters []struct {
			ClusterName         string `json:"clusterName"`
			Status              string `json:"status"`
			RunningTasksCount   int    `json:"runningTasksCount"`
			ActiveServicesCount int    `json:"activeServicesCount"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return out, unreadable(err)
	}
	for _, cl := range d.Clusters {
		for i := range out {
			if out[i].ID == cl.ClusterName {
				out[i].State = strings.ToLower(cl.Status)
				out[i].Detail = fmt.Sprintf("%d services · %d tasks running", cl.ActiveServicesCount, cl.RunningTasksCount)
			}
		}
	}
	return out, nil
}

// names parses an answer that is a list of names under key.
func names(service, key string) func(b []byte, region string) ([]fleetapi.AWSResource, error) {
	return func(b []byte, region string) ([]fleetapi.AWSResource, error) {
		var v map[string]json.RawMessage
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, unreadable(err)
		}
		var ns []string
		if raw, ok := v[key]; ok {
			if err := json.Unmarshal(raw, &ns); err != nil {
				return nil, unreadable(err)
			}
		}
		var out []fleetapi.AWSResource
		for _, n := range ns {
			out = append(out, fleetapi.AWSResource{Service: service, Region: region, ID: n})
		}
		return out, nil
	}
}

func parseELB(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		LoadBalancers []struct {
			LoadBalancerName, Type, Scheme string
			DNSName                        string `json:"DNSName"`
			State                          struct{ Code string }
			CreatedTime                    awsTime
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, l := range v.LoadBalancers {
		out = append(out, fleetapi.AWSResource{Service: "elb", Region: region, ID: l.LoadBalancerName, Type: l.Type, State: l.State.Code,
			Detail: joined(l.Scheme, l.DNSName), Created: l.CreatedTime.Time})
	}
	return out, nil
}

func parseHTTPAPIs(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		Items []struct {
			APIID        string `json:"ApiId"`
			Name         string
			ProtocolType string
			APIEndpoint  string `json:"ApiEndpoint"`
			CreatedDate  awsTime
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, a := range v.Items {
		out = append(out, fleetapi.AWSResource{Service: "apigateway", Region: region, ID: a.APIID, Name: a.Name, Type: a.ProtocolType,
			Detail: a.APIEndpoint, Created: a.CreatedDate.Time})
	}
	return out, nil
}

func parseRESTAPIs(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		Items []struct {
			ID                    string  `json:"id"`
			Name                  string  `json:"name"`
			CreatedDate           awsTime `json:"createdDate"`
			EndpointConfiguration struct {
				Types []string `json:"types"`
			} `json:"endpointConfiguration"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, a := range v.Items {
		out = append(out, fleetapi.AWSResource{Service: "apigateway", Region: region, ID: a.ID, Name: a.Name, Type: "REST",
			Detail: strings.Join(a.EndpointConfiguration.Types, ","), Created: a.CreatedDate.Time})
	}
	return out, nil
}

func parseECR(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		Repositories []struct {
			RepositoryName     string  `json:"repositoryName"`
			CreatedAt          awsTime `json:"createdAt"`
			ImageTagMutability string  `json:"imageTagMutability"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, r := range v.Repositories {
		tags := ""
		if r.ImageTagMutability != "" {
			tags = strings.ToLower(r.ImageTagMutability) + " tags"
		}
		out = append(out, fleetapi.AWSResource{Service: "ecr", Region: region, ID: r.RepositoryName, Detail: tags, Created: r.CreatedAt.Time})
	}
	return out, nil
}

func parseCognito(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct {
		UserPools []struct {
			ID           string `json:"Id"`
			Name         string
			CreationDate awsTime
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, p := range v.UserPools {
		out = append(out, fleetapi.AWSResource{Service: "cognito", Region: region, ID: p.ID, Name: p.Name, Created: p.CreationDate.Time})
	}
	return out, nil
}

// fifo is a queue or topic's kind, by its name.
func fifo(name string) string {
	if strings.HasSuffix(name, ".fifo") {
		return "fifo"
	}
	return "standard"
}

func parseSQS(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct{ QueueUrls []string }
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, u := range v.QueueUrls {
		n := path.Base(u)
		out = append(out, fleetapi.AWSResource{Service: "sqs", Region: region, ID: n, Type: fifo(n)})
	}
	return out, nil
}

func parseSNS(b []byte, region string) ([]fleetapi.AWSResource, error) {
	var v struct{ Topics []struct{ TopicArn string } }
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, t := range v.Topics {
		n := t.TopicArn[strings.LastIndexByte(t.TopicArn, ':')+1:]
		out = append(out, fleetapi.AWSResource{Service: "sns", Region: region, ID: n})
	}
	return out, nil
}

// parseS3 places each bucket in its region when the answer says it (it does
// when asked with --max-buckets), else "global".
func parseS3(b []byte, _ string) ([]fleetapi.AWSResource, error) {
	var v struct {
		Buckets []struct {
			Name, BucketRegion string
			CreationDate       awsTime
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, bk := range v.Buckets {
		region := bk.BucketRegion
		if region == "" {
			region = "global"
		}
		out = append(out, fleetapi.AWSResource{Service: "s3", Region: region, ID: bk.Name, Created: bk.CreationDate.Time})
	}
	return out, nil
}

// parseCloudFront names a distribution by its first alias, else its
// comment. CloudFront says when one last changed, not when it was made.
func parseCloudFront(b []byte, _ string) ([]fleetapi.AWSResource, error) {
	var v struct {
		DistributionList struct {
			Items []struct {
				ID               string `json:"Id"`
				DomainName       string
				Status           string
				Enabled          bool
				Comment          string
				Aliases          struct{ Items []string }
				LastModifiedTime awsTime
			}
		}
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, unreadable(err)
	}
	var out []fleetapi.AWSResource
	for _, d := range v.DistributionList.Items {
		name := d.Comment
		if len(d.Aliases.Items) > 0 {
			name = d.Aliases.Items[0]
		}
		state := d.Status
		if !d.Enabled {
			state = "disabled"
		}
		out = append(out, fleetapi.AWSResource{Service: "cloudfront", Region: "global", ID: d.ID, Name: name, State: state,
			Detail: d.DomainName, Created: d.LastModifiedTime.Time})
	}
	return out, nil
}
