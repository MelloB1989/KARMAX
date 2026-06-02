package tools

import (
	"fmt"
	"sync"
)

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

var Global = &Registry{tools: make(map[string]Tool)}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Manifest().Name] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) List() []ToolManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	manifests := make([]ToolManifest, 0, len(r.tools))
	for _, t := range r.tools {
		manifests = append(manifests, t.Manifest())
	}
	return manifests
}

func (r *Registry) ResolveForAgent(names []string) ([]Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	resolved := make([]Tool, 0, len(names))
	for _, name := range names {
		t, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("tool not found: %s", name)
		}
		resolved = append(resolved, t)
	}
	return resolved, nil
}

func (r *Registry) All() map[string]Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]Tool, len(r.tools))
	for k, v := range r.tools {
		result[k] = v
	}
	return result
}
