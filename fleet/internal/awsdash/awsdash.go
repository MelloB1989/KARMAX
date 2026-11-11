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
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// DefaultRegions are read when fleet.yaml names none.
var DefaultRegions = []string{"all"}

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
	CacheFile string        // the last read is kept here, so a restart starts with it ("" is memory only)
	Run       Runner        // nil runs CLI with KeysCSV's keys
	Now       func() time.Time
}

// Dash is one account's dashboard: the last read of it, and the next.
type Dash struct {
	o Options

	rmu sync.Mutex  // one read at a time
	bg  atomic.Bool // a read behind Report is under way
	wg  sync.WaitGroup

	mu      sync.Mutex // guards what follows
	regions []string   // resolved: [all] read once
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
	d := &Dash{o: o}
	d.load()
	return d
}

// Report is the account as last read, at once. What is old is read again
// behind it, so a view never waits on a read of every region: the resources
// after Every, the cost after CostEvery. Only a first read with nothing
// cached, and a refresh, wait for the read; a refresh reads the resources
// now and the cost once it is an hour old. It fails only when there are no
// keys to read with.
func (d *Dash) Report(ctx context.Context, refresh bool) (*fleetapi.AWSReport, error) {
	d.mu.Lock()
	needRes, needCost := d.needs(refresh)
	cached := !d.resAt.IsZero()
	d.mu.Unlock()
	switch {
	case !needRes && !needCost:
	case cached && !refresh:
		d.background()
	default:
		if err := d.fetch(ctx, refresh); err != nil {
			return nil, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.report(), nil
}

// Wait returns when the reads behind Report are done (tests).
func (d *Dash) Wait() { d.wg.Wait() }

// needs is what is old enough to read again; d.mu is held.
func (d *Dash) needs(refresh bool) (res, cost bool) {
	now := d.o.Now()
	res = refresh || d.resAt.IsZero() || now.Sub(d.resAt) >= d.o.Every
	costWait := d.o.CostEvery
	if !d.costOK {
		costWait = d.o.Every // a failed read is tried again with the resources
	}
	cost = d.costAt.IsZero() || now.Sub(d.costAt) >= costWait || refresh && now.Sub(d.costAt) >= costRefreshAfter
	return res, cost
}

// background reads again what is old, once at a time, on its own clock: the
// caller that started it may be gone.
func (d *Dash) background() {
	if !d.bg.CompareAndSwap(false, true) {
		return
	}
	d.wg.Go(func() {
		defer d.bg.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_ = d.fetch(ctx, false)
	})
}

// fetch reads what is old, one read at a time (a read that came while
// another ran finds it done), and keeps the answer on disk.
func (d *Dash) fetch(ctx context.Context, refresh bool) error {
	d.rmu.Lock()
	defer d.rmu.Unlock()
	d.mu.Lock()
	now := d.o.Now()
	needRes, needCost := d.needs(refresh)
	d.mu.Unlock()
	if !needRes && !needCost {
		return nil
	}
	run := d.o.Run
	if run == nil {
		k, err := readKeys(d.o.KeysCSV)
		if err != nil {
			return err
		}
		run = newCLI(d.o.CLI, k)
	}
	c := &caller{run: run, sem: make(chan struct{}, parallel)}
	d.read(ctx, c, now, needRes, needCost)
	d.save()
	return nil
}

// read reads who the keys are, then the resources and the cost side by side.
// Keys that do not work stop it there: every other call would fail the same.
func (d *Dash) read(ctx context.Context, c *caller, now time.Time, res, cost bool) {
	d.mu.Lock()
	known, regions := d.account != "", d.regions
	d.mu.Unlock()
	if res || !known {
		acct, user, err := identity(ctx, c)
		if err != nil {
			d.mu.Lock()
			d.errs, d.resAt = []string{trim("sts: " + err.Error())}, now
			d.costErr, d.costAt, d.costOK = nil, now, false
			d.mu.Unlock()
			return
		}
		d.mu.Lock()
		d.account, d.user = acct, user
		d.mu.Unlock()
	}
	var wg sync.WaitGroup
	if res {
		wg.Go(func() {
			regs, errs := d.resolveRegions(ctx, c, regions)
			items, rerrs := readResources(ctx, c, regs)
			d.mu.Lock()
			d.items, d.errs = items, append(errs, rerrs...)
			d.regions, d.resAt = regs, now
			d.mu.Unlock()
		})
	}
	if cost {
		wg.Go(func() {
			got, errs := readCost(ctx, c, now)
			d.mu.Lock()
			d.costErr, d.costAt, d.costOK = errs, now, got != nil
			if got != nil {
				d.cost = got
			}
			d.mu.Unlock()
		})
	}
	wg.Wait()
}

// resolveRegions is the regions to read: fleet.yaml's, or with [all] every
// region the account has enabled (read once; the defaults if it fails).
func (d *Dash) resolveRegions(ctx context.Context, c *caller, known []string) ([]string, []string) {
	if !slices.Equal(d.o.Regions, []string{"all"}) {
		return d.o.Regions, nil
	}
	if known != nil {
		return known, nil
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
