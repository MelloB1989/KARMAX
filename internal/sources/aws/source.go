// Package aws is KARMAX's AWS event source: CloudWatch alarms going off and
// coming back, and what each account costs, for every usable profile in the
// shared AWS config.
//
// Profiles are read from the config each poll, so a login or a new profile
// takes effect without a restart. Profiles that reach the same account (and,
// for alarms, the same region) are one reader: the first in order whose
// login works reads it, and the rest are reported as skipped. A profile whose
// login has expired is skipped and its health carries the exact command that
// fixes it; it never stops the others.
//
// Everything here reads. Nothing creates, changes or deletes a cloud resource.
package aws

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// SourceName is the Source of every event published here.
const SourceName = "aws"

// billingRegion is where Cost Explorer and Budgets answer, whatever region
// the account runs in.
const billingRegion = "us-east-1"

// Health statuses, in the words the connector health table uses.
const (
	StatusHealthy  = "healthy"
	StatusFailed   = "failed"   // not logged in; Fix says how
	StatusDegraded = "degraded" // logged in, but something it should read could not be read
	StatusSkipped  = "skipped"  // another profile reads the same account
)

// Health is one profile's standing after the last poll.
type Health struct {
	Profile   string
	Status    string
	Account   string // the account (and region) it reads, when known
	Detail    string
	Fix       string // the command that brings a failed login back
	CheckedAt time.Time
}

// KV is the store the source keeps its cursors, cost cache and dedupe keys in.
type KV struct {
	Get func(group, key string) (string, bool, error)
	Set func(group, key, value string, ttl time.Duration) error
}

// Options builds a Source.
type Options struct {
	Config  Config
	Dial    Dialer
	KV      KV
	Publish func(sources.Event) error
	Now     func() time.Time
}

// Source reads every usable profile's alarms and cost.
type Source struct {
	cfg     Config
	dial    Dialer
	kv      KV
	seen    sources.Seen
	publish func(sources.Event) error
	now     func() time.Time

	mu        sync.Mutex
	apis      map[string]API    // profile@region
	accounts  map[string]string // profile -> account id learned by asking
	health    map[string]Health // from the last alarm poll
	costNotes map[string]string // profile -> what the last cost poll could not read
}

// seenTTL is how long a published item is remembered: far longer than any
// window an item could be read again in.
const seenTTL = 60 * 24 * time.Hour

// New builds a Source.
func New(o Options) *Source {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Source{
		cfg: o.Config.withDefaults(), dial: o.Dial, kv: o.KV, publish: o.Publish, now: o.Now,
		seen:      sources.Seen{Group: "aws-source-seen", TTL: seenTTL, Get: o.KV.Get, Set: o.KV.Set},
		apis:      map[string]API{},
		accounts:  map[string]string{},
		health:    map[string]Health{},
		costNotes: map[string]string{},
	}
}

// Health is every profile's standing, in profile order.
func (s *Source) Health() []Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Health, 0, len(s.health))
	for name, h := range s.health {
		if note := s.costNotes[name]; note != "" {
			h.Detail = joinDetail(h.Detail, "cost: "+note)
			if h.Status == StatusHealthy {
				h.Status = StatusDegraded
			}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Profile < out[j].Profile })
	return out
}

// reader is the one profile chosen to read an account (and region).
type reader struct {
	profile Profile
	account string // the account id
	region  string
	label   string // what events name it: alias-or-id/region
	api     API
}

// label names an account the way events and health show it.
func (s *Source) label(account, region string) string {
	name := account
	if a := s.cfg.Aliases[account]; a != "" {
		name = a
	}
	return name + "/" + region
}

// apiFor opens (once) the API for a profile in a region. The SDK caches
// credentials inside it, so keeping it keeps a login from being re-done.
func (s *Source) apiFor(ctx context.Context, p Profile, region string) (API, error) {
	key := p.Name + "@" + region
	if a, ok := s.apis[key]; ok {
		return a, nil
	}
	a, err := s.dial(ctx, p, region)
	if err != nil {
		return nil, err
	}
	s.apis[key] = a
	return a, nil
}

// failure is the health of a profile that could not be used.
func failure(p Profile, err error) Health {
	if isAuth(err) {
		fix := p.LoginFix()
		return Health{Profile: p.Name, Status: StatusFailed, Fix: fix,
			Detail: "not logged in (" + clip(err.Error(), 160) + "); run: " + fix}
	}
	return Health{Profile: p.Name, Status: StatusDegraded, Detail: clip(err.Error(), 240)}
}

// plan picks one reader per account, or per account and region when
// perRegion; costs are per account and read in the billing region.
func (s *Source) plan(ctx context.Context, perRegion bool) ([]reader, map[string]Health, error) {
	profiles, err := LoadProfiles(s.cfg.ConfigFile, s.cfg.CredentialsFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read the AWS config: %w", err)
	}
	profiles = Allowed(profiles, s.cfg.Profiles)

	health := map[string]Health{}
	type group struct {
		account, region string
		cands           []Profile
	}
	var order []string
	groups := map[string]*group{}
	for _, p := range profiles {
		region := p.Region
		if !perRegion {
			region = billingRegion
		}
		if region == "" {
			health[p.Name] = Health{Profile: p.Name, Status: StatusFailed,
				Detail: "no region in the AWS config", Fix: "aws configure set region <region> --profile " + p.Name}
			continue
		}
		account := p.AccountID
		if account == "" {
			account = s.accounts[p.Name]
		}
		if account == "" {
			// Not in the config: only asking tells which account this is.
			a, err := s.apiFor(ctx, p, region)
			if err == nil {
				account, err = a.Identity(ctx)
			}
			if err != nil {
				health[p.Name] = failure(p, err)
				continue
			}
			s.accounts[p.Name] = account
		}
		key := account
		if perRegion {
			key += "/" + region
		}
		g, ok := groups[key]
		if !ok {
			g = &group{account: account, region: region}
			groups[key] = g
			order = append(order, key)
		}
		g.cands = append(g.cands, p)
	}

	var readers []reader
	for _, key := range order {
		g := groups[key]
		label := s.label(g.account, g.region)
		if !perRegion {
			label = s.label(g.account, "global")
		}
		same := "same account as "
		if perRegion {
			same = "same account and region as "
		}
		var chosen *reader
		for _, p := range g.cands {
			if chosen != nil {
				health[p.Name] = Health{Profile: p.Name, Status: StatusSkipped, Account: label,
					Detail: same + chosen.profile.Name + ", which reads it"}
				continue
			}
			a, err := s.apiFor(ctx, p, g.region)
			if err == nil {
				_, err = a.Identity(ctx)
			}
			if err != nil {
				h := failure(p, err)
				h.Account = label
				health[p.Name] = h
				continue
			}
			chosen = &reader{profile: p, account: g.account, region: g.region, label: label, api: a}
			health[p.Name] = Health{Profile: p.Name, Status: StatusHealthy, Account: label}
		}
		if chosen != nil {
			readers = append(readers, *chosen)
		}
	}
	return readers, health, nil
}

// fixes are the distinct login commands failed profiles need, in order.
func fixes(health map[string]Health) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range health {
		if h.Status == StatusFailed && h.Fix != "" && !seen[h.Fix] {
			seen[h.Fix] = true
			out = append(out, h.Fix)
		}
	}
	sort.Strings(out)
	return out
}

// emit publishes an event the first time its key is seen.
func (s *Source) emit(e sources.Event) (bool, error) {
	first, err := s.seen.First(e.Key)
	if err != nil || !first {
		return false, err
	}
	if s.publish == nil {
		return true, nil
	}
	return true, s.publish(e)
}

func joinDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
