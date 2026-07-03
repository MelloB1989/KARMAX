package memory

import (
	"sync"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

type ManagerFactory struct {
	baseDir  string
	db       *store.Store
	log      *zap.Logger
	managers map[string]*Manager
	mu       sync.RWMutex
}

func NewFactory(baseDir string, db *store.Store, log *zap.Logger) *ManagerFactory {
	return &ManagerFactory{
		baseDir:  baseDir,
		db:       db,
		log:      log,
		managers: make(map[string]*Manager),
	}
}

func (f *ManagerFactory) For(agentID, namespace string) *Manager {
	if namespace == "" {
		namespace = agentID
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if m, ok := f.managers[namespace]; ok {
		return m
	}

	m := NewManager(agentID, namespace, f.baseDir, f.db, f.log)
	f.managers[namespace] = m
	return m
}

func (f *ManagerFactory) StopAll() {
	f.mu.RLock()
	defer f.mu.RUnlock()

	for _, m := range f.managers {
		m.Stop()
	}
}

// Managers returns every live manager (one per namespace), for maintenance.
func (f *ManagerFactory) Managers() []*Manager {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*Manager, 0, len(f.managers))
	for _, m := range f.managers {
		out = append(out, m)
	}
	return out
}

func (f *ManagerFactory) List() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	namespaces := make([]string, 0, len(f.managers))
	for ns := range f.managers {
		namespaces = append(namespaces, ns)
	}
	return namespaces
}
