package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"go.uber.org/zap"
)

type Registry struct {
	mu     sync.RWMutex
	agents map[string]*Agent
	bus    *bus.Bus
	store  *store.Store
	log    *zap.Logger
}

func NewRegistry(b *bus.Bus, s *store.Store, log *zap.Logger) *Registry {
	return &Registry{
		agents: make(map[string]*Agent),
		bus:    b,
		store:  s,
		log:    log,
	}
}

func (r *Registry) Register(def AgentDef, mem *memory.Manager, agentTools []tools.Tool, mcpTools []tools.Tool) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.agents[def.ID]; exists {
		return nil, fmt.Errorf("agent already registered: %s", def.ID)
	}

	a := NewAgent(def, r.bus, r.store, mem, agentTools, mcpTools, r.log)
	r.agents[def.ID] = a
	return a, nil
}

func (r *Registry) Get(id string) (*Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.agents[id]
	return a, ok
}

func (r *Registry) List() []*Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()

	agents := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		agents = append(agents, a)
	}
	return agents
}

func (r *Registry) StartAll(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, a := range r.agents {
		if a.def.Triggers.RunOnStart {
			if err := a.Start(ctx); err != nil {
				r.log.Error("failed to start agent", zap.String("agent", a.def.ID), zap.Error(err))
			}
		}
	}
	return nil
}

func (r *Registry) StopAll() error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, a := range r.agents {
		if a.Status() == StatusRunning || a.Status() == StatusPaused {
			if err := a.Stop(); err != nil {
				r.log.Error("failed to stop agent", zap.String("agent", a.def.ID), zap.Error(err))
			}
		}
	}
	return nil
}

func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	a, ok := r.agents[id]
	if !ok {
		return fmt.Errorf("agent not found: %s", id)
	}

	if a.Status() == StatusRunning || a.Status() == StatusPaused {
		a.Stop()
	}

	delete(r.agents, id)
	return nil
}

func (r *Registry) Snapshots() []AgentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	snaps := make([]AgentSnapshot, 0, len(r.agents))
	for _, a := range r.agents {
		snaps = append(snaps, a.Snapshot())
	}
	return snaps
}
