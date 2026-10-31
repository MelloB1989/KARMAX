package runtime

import (
	"time"

	"go.uber.org/zap"
)

// holdLease keeps a run's lease for as long as the run is actually in flight,
// and returns the function that stops holding it.
//
// A lease of a fixed term assumed every run returns by its deadline. A run
// only returns when its loop's code does, and code stuck in a call that does
// not take the run's context does not: memory-merge ran nineteen minutes on a
// fourteen-minute deadline and a fifteen-minute lease, so any trigger after
// minute fifteen would have started a second copy beside it. Renewed while
// the run lives, the lease cannot run out from under it; and since renewal
// stops with the process, a crashed run's lease still expires on its own.
//
// A run past its deadline is reported once. It keeps its loop until it
// returns — a stuck loop shows as running, and then as dark, never as two
// copies of itself.
func (rt *KarmaxRuntime) holdLease(loop, owner string, started time.Time) (stop func()) {
	done := make(chan struct{})
	ttl, deadline := leaseTTL, loopRunTimeout
	go func() {
		tick := time.NewTicker(ttl / 3)
		defer tick.Stop()
		overran := false
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			if held, err := rt.store.RenewLoopLease(loop, owner, ttl); err != nil {
				rt.log.Warn("could not renew a running loop's lease", zap.String("loop", loop), zap.Error(err))
			} else if !held {
				rt.log.Warn("a running loop no longer holds its lease", zap.String("loop", loop))
			}
			if took := time.Since(started); !overran && took > deadline {
				overran = true
				rt.log.Warn("loop run is past its deadline and has not returned; it keeps its lease until it does",
					zap.String("loop", loop), zap.Duration("deadline", deadline), zap.Duration("running", took))
			}
		}
	}()
	return func() { close(done) }
}
