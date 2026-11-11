package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
	"github.com/MelloB1989/karmax/fleet/internal/quotaprobe"
	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// QuotaReader reads an account's live windows by its token (quotaprobe.Prober).
type QuotaReader interface {
	Read(ctx context.Context, token string) (quotaprobe.Reading, error)
}

// quotaProbe is the live reading of every account's windows: a status line
// has them only as of a session's last turn, so an idle account's are old.
type quotaProbe struct {
	read   QuotaReader
	tokens map[string]string // account -> its token
	every  time.Duration

	wg    sync.WaitGroup
	mu    sync.Mutex
	busy  bool
	at    time.Time
	got   map[string]reconcile.Quota
	fails map[string]string // account -> the last failure told
}

// ProbeQuota turns on the live reading: each account's windows are read by
// its own token every `every`, and win over what a status line last said.
func (f *Fleet) ProbeQuota(r QuotaReader, tokens map[string]string, every time.Duration) {
	f.qprobe = &quotaProbe{read: r, tokens: tokens, every: every, got: map[string]reconcile.Quota{}, fails: map[string]string{}}
}

// Probed is the account's live windows when read within max, else none.
func (f *Fleet) Probed(account string, now time.Time, max time.Duration) (reconcile.Quota, bool) {
	p := f.qprobe
	if p == nil {
		return reconcile.Quota{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	q, ok := p.got[account]
	return q, ok && now.Sub(q.At) <= max
}

// probeDue starts a read of every account in the background when the last is
// older than the interval; the tick never waits for it.
func (f *Fleet) probeDue(now time.Time) {
	p := f.qprobe
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.busy || now.Sub(p.at) < p.every {
		p.mu.Unlock()
		return
	}
	p.busy, p.at = true, now
	p.mu.Unlock()
	p.wg.Go(func() {
		defer func() { p.mu.Lock(); p.busy = false; p.mu.Unlock() }()
		f.probeAll(context.Background(), now)
	})
}

func (f *Fleet) probeAll(ctx context.Context, now time.Time) {
	p := f.qprobe
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for acct, tok := range p.tokens {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			defer cancel()
			r, err := p.read.Read(cctx, tok)
			p.mu.Lock()
			if err != nil {
				if p.fails[acct] != err.Error() {
					p.fails[acct] = err.Error()
					f.Log("quota probe %s: %v", acct, err)
				}
				p.mu.Unlock()
				return
			}
			delete(p.fails, acct)
			q := reconcile.Quota{FiveHour: r.FiveHour, FiveHourResets: r.FiveResets, SevenDay: r.SevenDay, SevenDayResets: r.SevenResets, At: now}
			p.got[acct] = q
			p.mu.Unlock()
			if err := f.db.AddQuota(ledger.Quota{Account: acct, At: now, FiveHour: r.FiveHour, FiveResets: r.FiveResets,
				SevenDay: r.SevenDay, SevenResets: r.SevenResets, Source: "probe"}); err != nil {
				f.Log("ledger: quota %s: %v", acct, err)
			}
		}()
	}
	wg.Wait()
	f.touch()
}
