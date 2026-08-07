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

	// gitloom, when set, points every namespace's long-term memory at GitLoom
	// Cloud. Held here rather than passed to each call site so a namespace
	// created later cannot quietly come up on a different backend from the rest.
	gitloom *GitLoomConfig
}

// UseGitLoom routes long-term memory through GitLoom Cloud for every namespace,
// including ones created after this call. Each namespace becomes its own
// GitLoom namespace, which is also how GitLoom expects a multi-user host to
// shard: one repository and one index per namespace, so writes stay parallel.
func (f *ManagerFactory) UseGitLoom(cfg GitLoomConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gitloom = &cfg
	for ns, m := range f.managers {
		c := cfg
		c.Namespace = f.namespaceFor(ns)
		m.UseGitLoom(c)
	}
}

// namespaceFor derives the GitLoom namespace for a local one.
//
// The configured namespace maps the PRIMARY agent's memory across unchanged —
// setting GITLOOM_NAMESPACE=karmax means "my memory is at karmax", and
// silently redirecting it to karmax-nexus points the daemon at an empty
// namespace beside the one everything was migrated into. Only additional
// agents are suffixed, so a second agent on the same host cannot write into
// the first one's memory.
func (f *ManagerFactory) namespaceFor(local string) string {
	if f.gitloom == nil || f.gitloom.Namespace == "" {
		return local
	}
	if local == f.gitloom.PrimaryLocal || local == f.gitloom.Namespace {
		return f.gitloom.Namespace
	}
	return f.gitloom.Namespace + "-" + local
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
	if f.gitloom != nil {
		cfg := *f.gitloom
		cfg.Namespace = f.namespaceFor(namespace)
		m.UseGitLoom(cfg)
	}
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
