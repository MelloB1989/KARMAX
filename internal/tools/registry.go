package tools

import (
	"sort"
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

// agentScopedTools are injected per-agent at runtime (in agent.initModels)
// rather than registered in the global registry, because they need per-agent
// state (the memory manager, escalation callbacks, etc.). They may therefore
// legitimately appear in an agent's configured tool list without being present
// in the registry, and must not be treated as "unknown".
var agentScopedTools = map[string]bool{
	"memory.retrieve": true,
	"memory.ingest":   true,
	"memory.forget":   true,
	"comms.escalate":  true,
	"profile.update":  true,
}

// IsAgentScoped reports whether name refers to a tool that is injected
// per-agent at runtime instead of being registered globally.
func IsAgentScoped(name string) bool {
	dotted := strings.ReplaceAll(name, "_", ".")
	return agentScopedTools[name] || agentScopedTools[dotted]
}

// ResolveForAgent resolves the named tools from the registry. Names that are
// not present are returned in unresolved rather than causing a hard failure,
// so a single typo (or an agent-scoped tool name) no longer wipes out an
// agent's entire toolset. The caller decides how to report unresolved names.
//
// A name ending in '*' matches by prefix. Whole families of tools arrive from
// SDKs — wacli alone brings forty-six — and naming each one in config is a list
// that is wrong the moment the SDK grows another. The agent asks for the family.
func (r *Registry) ResolveForAgent(names []string) (resolved []Tool, unresolved []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	resolved = make([]Tool, 0, len(names))
	seen := make(map[string]bool, len(names))
	add := func(t Tool) bool {
		name := t.Manifest().Name
		if seen[name] {
			return false
		}
		seen[name] = true
		resolved = append(resolved, t)
		return true
	}

	for _, name := range names {
		if prefix, ok := strings.CutSuffix(name, "*"); ok {
			// Sorted, because the registry is a map and an unstable tool order
			// would invalidate the prompt cache on every restart.
			matches := make([]Tool, 0, 8)
			for key, t := range r.tools {
				if strings.HasPrefix(key, prefix) || strings.HasPrefix(key, CanonicalName(prefix)) {
					matches = append(matches, t)
				}
			}
			sort.Slice(matches, func(i, j int) bool {
				return matches[i].Manifest().Name < matches[j].Manifest().Name
			})
			matched := false
			for _, t := range matches {
				if add(t) {
					matched = true
				}
			}
			if !matched {
				unresolved = append(unresolved, name)
			}
			continue
		}
		if t, ok := r.tools[name]; ok {
			add(t)
			continue
		}
		unresolved = append(unresolved, name)
	}
	return resolved, unresolved
}
