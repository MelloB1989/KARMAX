package scheduler

import (
	"time"
)

type ScheduledJob struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Cron     string         `json:"cron"`
	AgentID  string         `json:"agent_id"`
	Payload  map[string]any `json:"payload"`
	Enabled  bool           `json:"enabled"`
	LastRun  *time.Time     `json:"last_run,omitempty"`
	NextRun  *time.Time     `json:"next_run,omitempty"`
	RunCount int64          `json:"run_count"`
	CatchUp  bool           `json:"catch_up"`
}
