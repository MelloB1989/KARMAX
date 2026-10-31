package fleetapi

import (
	"context"
	"fmt"
	"time"
)

// The demo's AWS account: made up, like its agents (account 123456789012,
// example names, documentation IPs). The AWS tab's tests and fleetctl tui
// --demo run on it.

func (f *Fake) AWS(ctx context.Context, refresh bool) (*AWSReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return nil, fmt.Errorf("403: aws is for admins")
	}
	// A read changes nothing in the fleet: no new snapshot.
	f.Calls = append(f.Calls, fmt.Sprintf("aws refresh=%v", refresh))
	return fakeAWS(f.Now()), nil
}

func fakeAWS(now time.Time) *AWSReport {
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const day = 24 * time.Hour
	r := &AWSReport{
		At:      ago(3 * time.Minute),
		Account: "123456789012",
		User:    "fleet-reader",
		Regions: []string{"ap-south-1", "us-east-1"},
		Errors: []string{
			"eks ap-south-1: AccessDeniedException: User: arn:aws:iam::123456789012:user/fleet-reader is not authorized to perform: eks:ListClusters",
			"cognito-idp us-east-1: no answer in time",
		},
	}
	// The last 14 days, oldest first: weekdays busier than weekends.
	daily := []float64{15.82, 16.40, 17.95, 18.31, 16.02, 12.48, 11.90, 16.75, 17.20, 19.84, 21.36, 18.07, 13.11, 9.44}
	c := &AWSCost{Currency: "USD", MonthToDate: 412.38, LastMonth: 468.20, Forecast: 511.62, Credits: 25, At: ago(2*time.Hour + 14*time.Minute)}
	for i, v := range daily {
		c.Daily = append(c.Daily, AWSAmount{Name: now.AddDate(0, 0, i-len(daily)+1).Format("2006-01-02"), Amount: v})
	}
	for _, s := range []AWSAmount{
		{"Amazon Elastic Compute Cloud - Compute", 186.40}, {"Amazon Relational Database Service", 92.15},
		{"EC2 - Other", 38.70}, {"Amazon Simple Storage Service", 24.10}, {"Amazon CloudFront", 19.85},
		{"Amazon Elastic Load Balancing", 16.30}, {"AWS Lambda", 9.42}, {"Amazon Virtual Private Cloud", 8.90},
		{"Amazon DynamoDB", 6.11}, {"AmazonCloudWatch", 4.85}, {"Amazon EC2 Container Registry (ECR)", 2.30},
		{"Amazon Route 53", 2.00}, {"Tax", 1.30},
	} {
		c.ByService = append(c.ByService, s)
	}
	r.Cost = c

	add := func(svc, region, id, name, typ, state, detail string, made time.Time) {
		r.Items = append(r.Items, AWSResource{Service: svc, Region: region, ID: id, Name: name, Type: typ, State: state, Detail: detail, Created: made})
	}
	const mum, nva, glb = "ap-south-1", "us-east-1", "global"
	add("apigateway", mum, "a1b2c3d4e5", "payments-api", "HTTP", "", "https://a1b2c3d4e5.execute-api.ap-south-1.amazonaws.com", ago(214*day))
	add("apigateway", nva, "r3st0example", "legacy-rest", "REST", "", "EDGE", ago(631*day))
	add("cloudfront", glb, "E1EXAMPLE12345", "cdn.example.com", "", "Deployed", "d111111abcdef8.cloudfront.net", ago(12*day))
	add("cloudfront", glb, "E2EXAMPLE67890", "static.example.com", "", "InProgress", "d222222abcdef8.cloudfront.net", ago(9*time.Minute))
	add("cognito", mum, "ap-south-1_EXAMPLE", "example-users", "", "", "", ago(402*day))
	add("dynamodb", mum, "feature-flags", "", "", "", "", time.Time{})
	add("dynamodb", mum, "rate-limits", "", "", "", "", time.Time{})
	add("dynamodb", mum, "sessions", "", "", "", "", time.Time{})
	add("ec2", mum, "i-0a1b2c3d4e5f60001", "api-prod-1", "t3.medium", "running", "10.0.1.12 · 203.0.113.10", ago(41*day))
	add("ec2", mum, "i-0a1b2c3d4e5f60002", "api-prod-2", "t3.medium", "running", "10.0.2.31 · 203.0.113.11", ago(41*day))
	add("ec2", mum, "i-0a1b2c3d4e5f60004", "bastion", "t3.micro", "stopped", "10.0.0.5", ago(388*day))
	add("ec2", mum, "i-0a1b2c3d4e5f60003", "worker-batch", "c6i.large", "running", "10.0.3.7", ago(6*day))
	add("ec2", nva, "i-0f9e8d7c6b5a40001", "staging-web", "t3.small", "pending", "10.1.0.20", ago(2*time.Minute))
	add("ecr", mum, "api", "", "", "", "immutable tags", ago(301*day))
	add("ecr", mum, "web", "", "", "", "mutable tags", ago(301*day))
	add("ecr", mum, "worker", "", "", "", "immutable tags", ago(122*day))
	add("ecs", mum, "prod-cluster", "", "", "active", "4 services · 7 tasks running", time.Time{})
	add("elb", mum, "api-prod-alb", "", "application", "active", "internet-facing · api-prod-alb-1234567890.ap-south-1.elb.amazonaws.com", ago(298*day))
	add("elb", mum, "internal-nlb", "", "network", "active", "internal · internal-nlb-0987654321.elb.ap-south-1.amazonaws.com", ago(120*day))
	for _, fn := range []struct {
		name, runtime, detail string
		age                   time.Duration
	}{
		{"auth-hooks", "nodejs20.x", "256 MB · arm64", 3 * day}, {"cleanup-sessions", "python3.11", "128 MB · arm64", 90 * day},
		{"image-resize", "nodejs20.x", "1536 MB · x86_64", 17 * day}, {"nightly-report", "python3.12", "512 MB · arm64", 26 * time.Hour},
		{"pdf-export", "image", "2048 MB · x86_64", 5 * day}, {"queue-drain", "provided.al2023", "256 MB · arm64", 44 * day},
		{"thumbnailer", "python3.12", "1024 MB · arm64", 21 * day}, {"webhook-relay", "nodejs20.x", "128 MB · arm64", 8 * time.Hour},
	} {
		add("lambda", mum, fn.name, "", fn.runtime, "", fn.detail, ago(fn.age))
	}
	add("lambda", nva, "cert-renewer", "", "python3.12", "", "256 MB · arm64", ago(160*day))
	add("lambda", nva, "cost-alerts", "", "python3.12", "", "128 MB · arm64", ago(33*day))
	add("lambda", nva, "edge-auth", "", "nodejs18.x", "", "128 MB · x86_64", ago(512*day))
	add("lambda", nva, "log-shipper", "", "provided.al2023", "", "512 MB · arm64", ago(71*day))
	add("rds", mum, "analytics-replica", "", "db.t4g.small", "available", "postgres 16.4 · 100 GB", ago(77*day))
	add("rds", mum, "legacy-mysql", "", "db.t3.micro", "stopped", "mysql 8.0.39 · 20 GB", ago(700*day))
	add("rds", mum, "orders-db", "", "db.t4g.medium", "available", "postgres 16.4 · 100 GB · multi-AZ", ago(240*day))
	for _, b := range []struct {
		name, region string
		age          time.Duration
	}{
		{"example-assets-prod", mum, 610 * day}, {"example-athena-results", mum, 180 * day}, {"example-backups", mum, 700 * day},
		{"example-cdn-origin", nva, 400 * day}, {"example-logs", mum, 700 * day}, {"example-static-site", nva, 95 * day},
		{"example-terraform-state", mum, 820 * day}, {"example-uploads", mum, 300 * day},
	} {
		add("s3", b.region, b.name, "", "", "", "", ago(b.age))
	}
	add("sns", mum, "alerts", "", "", "", "", time.Time{})
	add("sns", mum, "deploys", "", "", "", "", time.Time{})
	add("sqs", mum, "events.fifo", "", "fifo", "", "", time.Time{})
	add("sqs", mum, "jobs", "", "standard", "", "", time.Time{})
	add("sqs", mum, "jobs-dlq", "", "standard", "", "", time.Time{})
	return r
}
