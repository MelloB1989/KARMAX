package agent

import (
	"context"
	"time"

	"go.uber.org/zap"
)

func (a *Agent) StartHealthCheck(ctx context.Context) {
	interval := time.Duration(a.def.HealthCheck.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		consecutiveFailures := 0

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if a.Status() != StatusRunning {
					continue
				}

				healthy := a.checkHealth()
				if healthy {
					consecutiveFailures = 0
					continue
				}

				consecutiveFailures++
				a.log.Warn("health check failed",
					zap.Int("consecutive_failures", consecutiveFailures))

				if consecutiveFailures >= 3 {
					a.log.Error("agent unhealthy, triggering restart")
					a.handleRestart(ctx)
					consecutiveFailures = 0
				}
			}
		}
	}()
}

func (a *Agent) checkHealth() bool {
	a.mu.RLock()
	status := a.status
	lastEvt := a.lastEvent
	a.mu.RUnlock()

	if status != StatusRunning {
		return false
	}

	if a.def.HealthCheck.PingPrompt != "" {
		// Agent responds to pings via its event loop — check if it's still processing
		if !lastEvt.IsZero() && time.Since(lastEvt) < 5*time.Minute {
			return true
		}
	}

	return true
}

func (a *Agent) handleRestart(ctx context.Context) {
	policy := a.def.RestartPolicy
	if policy == RestartNever {
		a.mu.Lock()
		a.status = StatusFailed
		a.mu.Unlock()
		return
	}

	a.mu.Lock()
	a.restarts++
	restarts := a.restarts
	a.mu.Unlock()

	if a.def.MaxRestarts > 0 && restarts > a.def.MaxRestarts {
		a.log.Error("max restarts exceeded", zap.Int("restarts", restarts))
		a.mu.Lock()
		a.status = StatusFailed
		a.mu.Unlock()
		return
	}

	// Exponential backoff
	backoff := time.Duration(1<<uint(min(restarts, 8))) * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}

	a.log.Info("restarting agent", zap.Duration("backoff", backoff), zap.Int("restart_count", restarts))

	a.Stop()
	time.Sleep(backoff)

	if ctx.Err() == nil {
		a.Start(ctx)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
