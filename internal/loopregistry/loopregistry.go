// Package loopregistry installs, removes, and describes what a loop registry
// (github.com/MelloB1989/karmax-loops's index.json, by default) lists — the
// write side of a wasmloop.RegistryEntry.
//
// It used to live entirely inside cmd/karmax/registry_cmd.go, which was fine
// while the only caller was a terminal prompt. A desktop app cannot answer a
// "type the loop's name to confirm" prompt, so it needs the same fetch,
// digest-check, and signature/trust decision as an HTTP call it can make on
// its own — and it must reach exactly the same answer the CLI would, or
// "installed via the app" and "installed via `karmax loops install`" stop
// meaning the same thing. This package is that one answer, called by both.
package loopregistry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"gopkg.in/yaml.v3"
)

// InstalledNames is what's already on this machine, across both tiers a
// registry entry can be. Moved out of cmd/karmax so `karmax loops browse` and
// the registry API's "installed" flag can never answer that question two
// different ways.
func InstalledNames() map[string]bool {
	out := map[string]bool{}
	in := &wasmloop.Installer{Dir: wasmloop.Dir()}
	if entries, err := in.Installed(); err == nil {
		for _, e := range entries {
			out[e.Name] = true
		}
	}
	for _, l := range recipes.LoadAll(recipes.Dir()) {
		if l.Recipe != nil {
			out[l.Recipe.Name] = true
		}
	}
	return out
}

// RecipeTrigger reads a recipe's `on:` block back in the literal form the
// registry states a trigger in — a cron string, "on <event>" — not the
// human-friendly rendering internal/api's console gives the same field
// (see console_recipes.go's triggerLabel, which renders for a person reading
// a list; this is for a caller that already knows what a cron string is).
func RecipeTrigger(r *recipes.Recipe) string {
	switch {
	case r.On.Schedule != "":
		return r.On.Schedule
	case r.On.Event != "":
		return "on " + r.On.Event
	case r.On.Webhook != "":
		return "webhook " + r.On.Webhook
	case r.On.Manual:
		return "only when you run it"
	}
	return ""
}

// WorkflowTrigger is RecipeTrigger's counterpart for a signed loop's manifest.
func WorkflowTrigger(m wasmloop.Manifest) string {
	switch {
	case m.Schedule != "":
		return m.Schedule
	case len(m.Events) > 0:
		return "on " + strings.Join(m.Events, ", ")
	case m.Webhook != "":
		return "webhook " + m.Webhook
	}
	return ""
}

// ParseRecipeArtifact parses a fetched recipe's bytes without writing
// anything, for a caller that needs to describe it before installing (the
// CLI's confirmation screen, the API's detail view).
func ParseRecipeArtifact(name string, data []byte) (*recipes.Recipe, error) {
	path := filepath.Join(recipes.Dir(), name+".yaml")
	r, err := recipes.Parse(path, data)
	if err != nil {
		return nil, fmt.Errorf("the registry's copy of %s is not a valid recipe: %w", name, err)
	}
	return r, nil
}

// WriteRecipe installs a recipe's already-fetched (and digest-verified)
// bytes. No signature and no sandbox: a recipe is not code, it is data
// KARMAX interprets with its own tools, under the same Broker every other
// caller passes.
func WriteRecipe(name string, data []byte) (path string, replaced bool, err error) {
	dir := recipes.Dir()
	path = filepath.Join(dir, name+".yaml")
	if _, statErr := os.Stat(path); statErr == nil {
		replaced = true
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return path, replaced, err
	}
	err = os.WriteFile(path, data, 0o644)
	return path, replaced, err
}

// RemoveRecipe deletes an installed recipe's file — the uninstall half of
// WriteRecipe.
func RemoveRecipe(name string) error {
	return os.Remove(filepath.Join(recipes.Dir(), name+".yaml"))
}

// ErrUntrusted is returned by InstallWorkflow when a workflow verifies to
// less than registry tier and allowUntrusted was not set.
//
// It stands in for `karmax loops install`'s confirmUnreviewed prompt, which
// makes an operator type the loop's name back — a per-install, never-
// persisted acceptance of "nobody I trust vouched for this". A caller that
// cannot put a human at a terminal gets the same one-time acceptance as a
// single boolean instead.
var ErrUntrusted = errors.New("loopregistry: not countersigned by a registry this instance trusts")

// InstallWorkflow verifies and installs a signed loop exactly as `karmax
// loops install <name> --yes` does: the same digest check (already done by
// wasmloop.Client.Fetch before this is called), the same signature
// verification, the same tier decision.
//
// in.Trust is the instance's OWN configured trust (registries, revocations,
// any persisted --allow-community) — used to decide the real tier. Inspecting
// is done against a version of it relaxed to reach a verdict rather than
// error out, because the tier decision belongs to allowUntrusted here, not to
// wasmloop.Verify's own all-or-nothing gate (see the AllowCommunity/
// AllowUntrusted comments on wasmloop.Trust).
func InstallWorkflow(in *wasmloop.Installer, data []byte, allowUntrusted bool) (preview *wasmloop.Preview, alreadyCurrent bool, err error) {
	lenient := *in
	lenient.Trust.AllowCommunity = true
	lenient.Trust.AllowUntrusted = true

	preview, err = lenient.Inspect(data)
	if err != nil {
		return nil, false, err
	}
	if preview.Verdict.Tier != wasmloop.TierRegistry && !allowUntrusted {
		return preview, false, ErrUntrusted
	}
	// Same name AND same version already on disk: nothing would change, so
	// nothing needs the restart a real install does.
	alreadyCurrent = preview.Upgrade != "" && preview.Upgrade == preview.Manifest.Version
	if alreadyCurrent {
		return preview, true, nil
	}
	if _, err := lenient.Install(data); err != nil {
		return preview, false, err
	}
	return preview, false, nil
}

// FetchWorkflowManifest downloads a workflow's companion loop.yaml — the
// hand-written manifest published beside its artifact — without touching the
// signed .kloop. Used only to DESCRIBE a workflow before installing it, since
// downloading and unpacking a multi-megabyte module just to show its trigger
// and tool list would make every marketplace detail view pay a WASM-sized
// price for a few lines of YAML.
func FetchWorkflowManifest(ctx context.Context, c *wasmloop.Client, source string) (wasmloop.Manifest, string, error) {
	source = strings.Trim(strings.TrimSpace(source), "/")
	if source == "" {
		return wasmloop.Manifest{}, "", fmt.Errorf("loopregistry: this entry has no source path to read a manifest from")
	}
	url := c.BaseURL + "/" + source + "/loop.yaml"
	body, err := c.GetRaw(ctx, url)
	if err != nil {
		return wasmloop.Manifest{}, "", fmt.Errorf("loopregistry: could not read %s: %w", url, err)
	}
	var m wasmloop.Manifest
	if err := yaml.Unmarshal(body, &m); err != nil {
		return wasmloop.Manifest{}, "", fmt.Errorf("loopregistry: %s is not a valid loop manifest: %w", url, err)
	}
	return m, string(body), nil
}

// GitHubSourceURL turns a registry entry's Source (a path inside the registry
// repo) into a browsable GitHub URL — only when the registry's base is
// GitHub's raw-content host, the one host this can derive a /tree/ URL for
// without guessing. Anything else (a private mirror, a different host)
// answers "", since a wrong guess is worse than an absent link.
func GitHubSourceURL(baseURL, source string) string {
	source = strings.Trim(strings.TrimSpace(source), "/")
	if source == "" {
		return ""
	}
	const prefix = "https://raw.githubusercontent.com/"
	if !strings.HasPrefix(baseURL, prefix) {
		return ""
	}
	parts := strings.SplitN(strings.TrimPrefix(baseURL, prefix), "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	owner, repo, branch := parts[0], parts[1], strings.SplitN(parts[2], "/", 2)[0]
	return fmt.Sprintf("https://github.com/%s/%s/tree/%s/%s", owner, repo, branch, source)
}

// IndexCache holds a registry's index for a short window so a screen that
// re-renders often — a marketplace tab regaining focus, a poll — does not
// refetch the whole catalogue every time, while a network blip still serves
// the last good copy instead of turning a working list blank.
type IndexCache struct {
	ttl time.Duration

	mu        sync.Mutex
	idx       *wasmloop.Index
	fetchedAt time.Time
}

// NewIndexCache builds a cache with the given freshness window.
func NewIndexCache(ttl time.Duration) *IndexCache { return &IndexCache{ttl: ttl} }

// Get returns the cached index when it is fresh, or fetches a new one.
// refresh forces a fetch regardless of age. A failed fetch falls back to
// whatever is cached, however stale (stale=true), rather than an error; only
// a failed fetch with nothing cached yet is returned as err.
func (c *IndexCache) Get(ctx context.Context, refresh bool) (idx *wasmloop.Index, fetchedAt time.Time, baseURL string, stale bool, err error) {
	client := wasmloop.NewClient()
	baseURL = client.BaseURL

	c.mu.Lock()
	fresh := !refresh && c.idx != nil && time.Since(c.fetchedAt) < c.ttl
	if fresh {
		idx, fetchedAt = c.idx, c.fetchedAt
	}
	c.mu.Unlock()
	if fresh {
		return idx, fetchedAt, baseURL, false, nil
	}

	fetched, ferr := client.Index(ctx)
	if ferr != nil {
		c.mu.Lock()
		idx, fetchedAt = c.idx, c.fetchedAt
		c.mu.Unlock()
		if idx != nil {
			return idx, fetchedAt, baseURL, true, nil
		}
		return nil, time.Time{}, baseURL, false, ferr
	}

	now := time.Now()
	c.mu.Lock()
	c.idx, c.fetchedAt = fetched, now
	c.mu.Unlock()
	return fetched, now, baseURL, false, nil
}
