package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

type Scheduler struct {
	cron  *cron.Cron
	store *store.Store
	bus   *bus.Bus
	log   *zap.Logger
	jobs  map[string]*jobEntry
	mu    sync.RWMutex
}

type jobEntry struct {
	job     ScheduledJob
	entryID cron.EntryID
}

func New(s *store.Store, b *bus.Bus, log *zap.Logger) *Scheduler {
	return &Scheduler{
		cron:  cron.New(cron.WithSeconds()),
		store: s,
		bus:   b,
		log:   log,
		jobs:  make(map[string]*jobEntry),
	}
}

func (s *Scheduler) AddJob(j ScheduledJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if j.ID == "" {
		j.ID = uuid.New().String()
	}

	// Remove existing if replacing
	if existing, ok := s.jobs[j.ID]; ok {
		s.cron.Remove(existing.entryID)
	}

	entryID, err := s.cron.AddFunc(j.Cron, func() {
		s.fireJob(j.ID)
	})
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", j.Cron, err)
	}

	s.jobs[j.ID] = &jobEntry{
		job:     j,
		entryID: entryID,
	}

	payloadJSON, _ := json.Marshal(j.Payload)
	return s.store.SaveJob(store.StoredJob{
		ID:       j.ID,
		Name:     j.Name,
		Cron:     j.Cron,
		AgentID:  j.AgentID,
		Payload:  string(payloadJSON),
		Enabled:  j.Enabled,
		LastRun:  j.LastRun,
		NextRun:  j.NextRun,
		RunCount: j.RunCount,
		CatchUp:  j.CatchUp,
	})
}

func (s *Scheduler) RemoveJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("job not found: %s", id)
	}

	s.cron.Remove(entry.entryID)
	delete(s.jobs, id)

	return s.store.DeleteJob(id)
}

func (s *Scheduler) Start(ctx context.Context) error {
	storedJobs, err := s.store.ListJobs()
	if err != nil {
		return fmt.Errorf("load jobs: %w", err)
	}

	for _, sj := range storedJobs {
		if !sj.Enabled {
			continue
		}

		var payload map[string]any
		json.Unmarshal([]byte(sj.Payload), &payload)

		j := ScheduledJob{
			ID:       sj.ID,
			Name:     sj.Name,
			Cron:     sj.Cron,
			AgentID:  sj.AgentID,
			Payload:  payload,
			Enabled:  sj.Enabled,
			LastRun:  sj.LastRun,
			NextRun:  sj.NextRun,
			RunCount: sj.RunCount,
			CatchUp:  sj.CatchUp,
		}

		if err := s.AddJob(j); err != nil {
			s.log.Error("failed to load job", zap.String("job", sj.Name), zap.Error(err))
		}
	}

	s.cron.Start()
	s.log.Info("scheduler started", zap.Int("jobs", len(s.jobs)))

	go func() {
		<-ctx.Done()
		s.Stop()
	}()

	return nil
}

func (s *Scheduler) Stop() {
	ctx := s.cron.Stop()
	<-ctx.Done()
	s.log.Info("scheduler stopped")
}

func (s *Scheduler) ListJobs() []ScheduledJob {
	s.mu.RLock()
	defer s.mu.RUnlock()

	jobs := make([]ScheduledJob, 0, len(s.jobs))
	for _, entry := range s.jobs {
		j := entry.job

		cronEntry := s.cron.Entry(entry.entryID)
		if !cronEntry.Next.IsZero() {
			next := cronEntry.Next
			j.NextRun = &next
		}

		jobs = append(jobs, j)
	}
	return jobs
}

func (s *Scheduler) RunJobNow(id string) error {
	s.mu.RLock()
	_, ok := s.jobs[id]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("job not found: %s", id)
	}

	go s.fireJob(id)
	return nil
}

func (s *Scheduler) fireJob(id string) {
	s.mu.RLock()
	entry, ok := s.jobs[id]
	s.mu.RUnlock()

	if !ok {
		return
	}

	j := entry.job
	now := time.Now()

	s.log.Info("firing scheduled job", zap.String("job", j.Name), zap.String("agent", j.AgentID))

	s.bus.Publish(bus.NewEvent(bus.EventScheduledJob, j.AgentID, map[string]any{
		"job_id":   j.ID,
		"job_name": j.Name,
		"agent_id": j.AgentID,
		"payload":  j.Payload,
	}))

	s.mu.Lock()
	entry.job.LastRun = &now
	entry.job.RunCount++
	s.mu.Unlock()

	cronEntry := s.cron.Entry(entry.entryID)
	var nextRun *time.Time
	if !cronEntry.Next.IsZero() {
		next := cronEntry.Next
		nextRun = &next
	}

	s.store.UpdateJobRun(id, now, nextRun)
}
