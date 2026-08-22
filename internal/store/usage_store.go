package store

import (
	"time"

	"github.com/google/uuid"
)

// Per-call model usage.
//
// Recorded so spend is a number rather than a guess. The Kiro gateway returned
// zero for every usage field, which is why nobody noticed the router was
// carrying 28k tokens into a decision that needed a few thousand — on a
// subscription that costs nothing extra, and on metered inference it is the
// whole bill.

// ModelUsage is one model call's token consumption.
type ModelUsage struct {
	ID           string
	AgentID      string
	Provider     string
	Model        string
	Kind         string // main | memory | summary | subagent — which instance spent it
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	CreatedAt    time.Time
}

// UsageTotal aggregates usage over a window.
type UsageTotal struct {
	Model        string
	Kind         string
	Calls        int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
}

// RecordModelUsage files one call. A call that reported nothing at all is
// skipped rather than stored as a row of zeroes, so a provider that omits usage
// is visible as missing data instead of as free inference.
func (s *Store) RecordModelUsage(u ModelUsage) error {
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheRead == 0 && u.CacheWrite == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if u.ID == "" {
		u.ID = uuid.New().String()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	_, err := s.exec(`
INSERT INTO model_usage (id, agent_id, provider, model, kind, input_tokens, output_tokens, cache_read, cache_write, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.AgentID, u.Provider, u.Model, u.Kind,
		u.InputTokens, u.OutputTokens, u.CacheRead, u.CacheWrite, u.CreatedAt)
	return err
}

// UsageSince totals consumption per model and instance kind since a point in
// time, heaviest spender first.
func (s *Store) UsageSince(since time.Time) ([]UsageTotal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT model, kind, COUNT(*),
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM model_usage
WHERE created_at >= ?
GROUP BY model, kind
ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageTotal
	for rows.Next() {
		var t UsageTotal
		if err := rows.Scan(&t.Model, &t.Kind, &t.Calls,
			&t.InputTokens, &t.OutputTokens, &t.CacheRead, &t.CacheWrite); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PruneModelUsage drops usage older than a cutoff.
func (s *Store) PruneModelUsage(before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.exec(`DELETE FROM model_usage WHERE created_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DailyUsage is one day's spend, for spotting the day a run rate changed.
type DailyUsage struct {
	Day          string
	Calls        int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	Model        string
}

// UsageByDay totals usage per day and model. A monthly figure hides the day
// something started costing more, which is the day worth looking at.
func (s *Store) UsageByDay(since time.Time) ([]DailyUsage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`
SELECT date(created_at) AS day, model, COUNT(*),
       COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0)
FROM model_usage
WHERE created_at >= ?
GROUP BY day, model
ORDER BY day ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DailyUsage
	for rows.Next() {
		var d DailyUsage
		if err := rows.Scan(&d.Day, &d.Model, &d.Calls,
			&d.InputTokens, &d.OutputTokens, &d.CacheRead, &d.CacheWrite); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
