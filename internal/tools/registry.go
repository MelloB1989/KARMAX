package tools

import (
	"fmt"
	"strings"
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

// CanonicalName returns the provider-safe tool name used when exposing tools
// to LLM APIs that reject dots in function names.
func CanonicalName(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}

func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := t.Manifest().Name
	r.tools[name] = t
	r.tools[CanonicalName(name)] = t
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
	seen := make(map[string]bool, len(r.tools))
	for _, t := range r.tools {
		manifest := t.Manifest()
		if seen[manifest.Name] {
			continue
		}
		seen[manifest.Name] = true
		manifests = append(manifests, manifest)
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
