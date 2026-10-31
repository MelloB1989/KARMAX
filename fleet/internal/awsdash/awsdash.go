// Package awsdash reads an AWS account for fleetctl's AWS tab: who fleetd
// reads it as, what the month costs (Cost Explorer), and what runs in each
// region. It runs the AWS CLI v2 with the keys of an IAM access-key CSV,
// which reach the CLI through its environment and nothing else.
//
// Reads are cached: the resources for Every (default 5m), the cost for
// CostEvery (default 6h), since each Cost Explorer call is charged ($0.01;
// a read is three). A part that cannot be read (a service, a region) is an
// entry in the report's Errors; the rest is still shown.
package awsdash

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// DefaultRegions are read when fleet.yaml names none.
var DefaultRegions = []string{"ap-south-1", "us-east-1"}

const (
	parallel    = 8                // CLI calls at once
	callTimeout = 30 * time.Second // one call
	maxErr      = 160              // runes of an error kept
	// A refresh reads the cost too once it is this old.
	costRefreshAfter = time.Hour
	homeRegion       = "us-east-1" // STS, Cost Explorer and the global services
)

// Options are fleet.yaml's aws: section, and the tests' fakes.
type Options struct {
	KeysCSV   string
	Regions   []string      // [all] is every region the account has enabled
	Every     time.Duration // the resources are read again after this
	CostEvery time.Duration // Cost Explorer is read again after this
	CLI       string        // the aws binary (FindCLI)
	Run       Runner        // nil runs CLI with KeysCSV's keys
	Now       func() time.Time
}

// Dash is one account's dashboard: the last read of it, and the next.
type Dash struct {
	o Options

	mu      sync.Mutex
	regions []string // resolved: [all] read once
	account string
	user    string
	items   []fleetapi.AWSResource
	errs    []string
	resAt   time.Time
	cost    *fleetapi.AWSCost
	costErr []string
	costAt  time.Time // the last try, read or not
	costOK  bool      // the last try read the cost
}

// New is a dashboard that reads nothing until asked.
func New(o Options) *Dash {
	if len(o.Regions) == 0 {
		o.Regions = DefaultRegions
	}
	if o.Every <= 0 {
		o.Every = 5 * time.Minute
	}
	if o.CostEvery <= 0 {
		o.CostEvery = 6 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Dash{o: o}
}

// Report is the account as last read, read again where that is old: the
// resources after Every (or at once on refresh), the cost after CostEvery
// (on refresh, once it is an hour old). Callers wait for one another, so
// one read serves them all. It fails only when there are no keys to read
// with.
func (d *Dash) Report(ctx context.Context, refresh bool) (*fleetapi.AWSReport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.o.Now()
	needRes := refresh || d.resAt.IsZero() || now.Sub(d.resAt) >= d.o.Every
	costWait := d.o.CostEvery
	if !d.costOK {
		costWait = d.o.Every // a failed read is tried again with the resources
	}
	needCost := d.costAt.IsZero() || now.Sub(d.costAt) >= costWait || refresh && now.Sub(d.costAt) >= costRefreshAfter
	if needRes || needCost {
		run := d.o.Run
		if run == nil {
			k, err := readKeys(d.o.KeysCSV)
			if err != nil {
				return nil, err
			}
			run = newCLI(d.o.CLI, k)
		}
		c := &caller{run: run, sem: make(chan struct{}, parallel)}
		d.read(ctx, c, now, needRes, needCost)
	}
	return d.report(), nil
}

// read reads who the keys are, then the resources and the cost side by side.
// Keys that do not work stop it there: every other call would fail the same.
func (d *Dash) read(ctx context.Context, c *caller, now time.Time, res, cost bool) {
	if res || d.account == "" {
		acct, user, err := identity(ctx, c)
		if err != nil {
			d.errs, d.resAt = []string{trim("sts: " + err.Error())}, now
			d.costErr, d.costAt, d.costOK = nil, now, false
			return
		}
		d.account, d.user = acct, user
	}
	var wg sync.WaitGroup
	if res {
		wg.Go(func() {
			regions, errs := d.resolveRegions(ctx, c)
			d.items, d.errs = readResources(ctx, c, regions)
			d.errs = append(errs, d.errs...)
			d.regions, d.resAt = regions, now
		})
	}
	if cost {
		wg.Go(func() {
			got, errs := readCost(ctx, c, now)
			d.costErr, d.costAt, d.costOK = errs, now, got != nil
			if got != nil {
				d.cost = got
			}
		})
	}
	wg.Wait()
}

// resolveRegions is the regions to read: fleet.yaml's, or with [all] every
// region the account has enabled (read once; the defaults if it fails).
func (d *Dash) resolveRegions(ctx context.Context, c *caller) ([]string, []string) {
	if !slices.Equal(d.o.Regions, []string{"all"}) {
		return d.o.Regions, nil
	}
	if d.regions != nil {
		return d.regions, nil
	}
	got, err := enabledRegions(ctx, c)
	if err != nil {
		return DefaultRegions, []string{trim("regions: " + err.Error())}
	}
	return got, nil
}

func (d *Dash) report() *fleetapi.AWSReport {
	r := &fleetapi.AWSReport{
		At: d.resAt, Account: d.account, User: d.user,
		Regions: slices.Clone(d.regions),
		Items:   slices.Clone(d.items),
		Errors:  append(slices.Clone(d.errs), d.costErr...),
	}
	if r.Regions == nil {
		r.Regions = slices.Clone(d.o.Regions)
	}
	if r.Items == nil {
		r.Items = []fleetapi.AWSResource{}
	}
	if r.Errors == nil {
		r.Errors = []string{}
	}
	if d.cost != nil {
		c := *d.cost
		r.Cost = &c
	}
	return r
}

// caller runs CLI calls, parallel at once, each within callTimeout.
type caller struct {
	run Runner
	sem chan struct{}
}

// call runs one CLI command. It prints nothing for some empty lists (sqs
// list-queues): that is an empty answer, {}.
func (c *caller) call(ctx context.Context, args ...string) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.sem }()
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	b, err := c.run.Run(cctx, args...)
	if err == nil && len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	return b, err
}

// trim is an error cut to maxErr runes, on one line.
func trim(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxErr {
		return string(r[:maxErr-1]) + "…"
	}
	return s
}
