package fleetapi

import "time"

// PathAWS is GET /v1/aws[?refresh=1]: the operator's AWS account at a glance
// (admins). fleetd reads it with the account's keys, at most once a few
// minutes unless refresh is asked for.
const PathAWS = "/v1/aws"

// AWSReport is GET /v1/aws.
type AWSReport struct {
	At      time.Time     `json:"at"` // when it was read
	Account string        `json:"account"`
	User    string        `json:"user,omitempty"` // the identity fleetd reads as
	Regions []string      `json:"regions"`
	Cost    *AWSCost      `json:"cost,omitempty"`
	Items   []AWSResource `json:"items"`
	// Errors are the parts that could not be read (a service, a region):
	// the rest is still shown.
	Errors []string `json:"errors"`
}

// AWSCost is the month so far, from Cost Explorer: what was used, before
// credits and refunds, which Credits says apart.
type AWSCost struct {
	Currency    string      `json:"currency"`
	MonthToDate float64     `json:"month_to_date"`
	Forecast    float64     `json:"forecast"`   // the month's end, 0 when unknown
	LastMonth   float64     `json:"last_month"` // all of it
	ByService   []AWSAmount `json:"by_service"` // month to date, largest first
	Daily       []AWSAmount `json:"daily"`      // the last 14 days, oldest first; Name is the date
	// Credits is the credits and refunds this month, as a positive amount:
	// what is paid is MonthToDate less it.
	Credits float64 `json:"credits,omitempty"`
	// At is when it was read: Cost Explorer is read seldom (each call is
	// charged), so it is often older than the resources.
	At time.Time `json:"at,omitzero"`
}

// AWSAmount is a cost with what it is for.
type AWSAmount struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
}

// AWSResource is one resource: an instance, a function, a bucket, a database…
type AWSResource struct {
	Service string `json:"service"` // ec2, lambda, s3, rds, ecs, eks, cloudfront, dynamodb, elb, …
	Region  string `json:"region"`  // "global" for global services
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"`  // t3.medium, python3.12, db.t4g.micro…
	State   string `json:"state,omitempty"` // running, stopped, available, Deployed…
	Detail  string `json:"detail,omitempty"`
	// Created is when it was made; for lambda and cloudfront, which do not
	// say, when it last changed.
	Created time.Time `json:"created,omitzero"`
}
