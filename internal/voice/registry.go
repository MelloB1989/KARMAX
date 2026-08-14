package voice

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry holds the voice integrations this instance can place calls through.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	def       string
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Register adds a provider. The first one registered is the default, which is
// the right rule for an instance with one and an explicit choice for one with
// several.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.ToLower(strings.TrimSpace(p.Name()))
	if name == "" {
		return
	}
	if len(r.providers) == 0 {
		r.def = name
	}
	r.providers[name] = p
}

// Place routes a call through the named provider, or the default when name is
// empty. An unknown name lists what exists, because "no such provider" without
// the alternatives is a dead end.
func (r *Registry) Place(ctx context.Context, provider, to string, opts CallOptions) error {
	r.mu.RLock()
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		name = r.def
	}
	p := r.providers[name]
	var known []string
	for n := range r.providers {
		known = append(known, n)
	}
	r.mu.RUnlock()

	if p == nil {
		sort.Strings(known)
		if len(known) == 0 {
			return fmt.Errorf("no voice integrations are configured on this instance")
		}
		return fmt.Errorf("no voice integration named %q — this instance has: %s",
			provider, strings.Join(known, ", "))
	}
	return p.Place(ctx, to, opts)
}

// Names lists the registered integrations, sorted, default first.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for n := range r.providers {
		if n != r.def {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if r.def != "" {
		out = append([]string{r.def}, out...)
	}
	return out
}
