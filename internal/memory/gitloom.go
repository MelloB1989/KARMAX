package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"go.uber.org/zap"
)

// The GitLoom-backed memory layer.
//
// GitLoom owns long-term memory outright: BM25 over the body, a separate
// embedded arm over the cues, a relationship graph with validity windows, and
// per-result provenance from git. When it is configured it IS the store, not a
// tier in front of one.
//
// It used to be treated as unreliable — every memory was mirrored into SQLite,
// queued in an outbox, and read back locally whenever the API did not answer.
// That was the right shape for a hosted service across home internet, and the
// wrong shape now that GitLoom runs locally as part of KARMAX. Two stores that
// can disagree is a worse failure than one store that can be down, because the
// disagreement is silent.
//
// SQLite keeps everything else it was always for: contacts, events, queues,
// timers, loop state, and the persistence that survives a crash.

// GitLoomConfig configures the memory store.
type GitLoomConfig struct {
	APIKey    string
	BaseURL   string
	Namespace string
	// Timeout bounds a single API call.
	Timeout time.Duration
	// PrimaryLocal is the local namespace that Namespace refers to. An operator
	// who sets GITLOOM_NAMESPACE means "put my memory there" — so that one
	// namespace maps across unchanged, and only ADDITIONAL agents get a suffix
	// to keep them out of each other's memory.
	PrimaryLocal string
}

// gitloomBackend reads and writes GitLoom.
type gitloomBackend struct {
	client *gitloom.Client
	cfg    GitLoomConfig
	log    *zap.Logger

	// healthy tracks whether the last call succeeded, so a degraded store is
	// reported once rather than on every query.
	mu      sync.RWMutex
	healthy bool
	lastErr string
}

// GitLoomConfigFromEnv reads the remote memory settings. Returns ok=false when
// no API key is configured, which is how a self-hosted install with no GitLoom
// account stays on the local store.
func GitLoomConfigFromEnv(namespace string) (GitLoomConfig, bool) {
	key := strings.TrimSpace(os.Getenv("GITLOOM_API_KEY"))
	if key == "" {
		return GitLoomConfig{}, false
	}
	cfg := GitLoomConfig{
		APIKey:       key,
		BaseURL:      strings.TrimSpace(os.Getenv("GITLOOM_BASE_URL")),
		Namespace:    strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE")),
		Timeout:      30 * time.Second,
		PrimaryLocal: namespace,
	}
	if cfg.Namespace == "" {
		cfg.Namespace = namespace
	}
	return cfg, true
}

func newGitLoomBackend(cfg GitLoomConfig, log *zap.Logger) *gitloomBackend {
	opts := []gitloom.Option{gitloom.WithNamespace(cfg.Namespace)}
	if cfg.BaseURL != "" {
		opts = append(opts, gitloom.WithBaseURL(cfg.BaseURL))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &gitloomBackend{
		client:  gitloom.New(cfg.APIKey, opts...),
		cfg:     cfg,
		log:     log,
		healthy: true,
	}
}

// write stores one memory, folding it onto whatever is already at its path.
//
// A GitLoom write replaces the whole file and KARMAX files every memory about
// one subject at one path, so writing raw would make the newest fact about a
// subject delete the forty already there.
func (g *gitloomBackend) write(ctx context.Context, e MemoryEntry, path string, related []string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	merged, err := g.foldOntoStored(cctx, ToGitLoom(e, path, related))
	if err != nil {
		g.setHealth(false, err)
		return err
	}
	if err := g.client.Write(cctx, []gitloom.Memory{merged}, nil); err != nil {
		g.setHealth(false, err)
		return err
	}
	g.setHealth(true, nil)
	return nil
}

func (g *gitloomBackend) forget(ctx context.Context, path string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	if err := g.client.Forget(cctx, []string{path}, nil); err != nil {
		g.setHealth(false, err)
		return err
	}
	g.setHealth(true, nil)
	return nil
}

// foldOntoStored carries everything already at a path into the memory about to
// replace it.
//
// A failure here refuses the write rather than proceeding. Both failure modes
// would otherwise destroy history: a read error degrades to overwrite-with-
// append-of-nothing, and a memory that exists but reads back EMPTY is treated
// as "nothing to preserve" — which is exactly what happened when an older API
// returned only the text before the first ## header for a file written
// entirely as sections.
func (g *gitloomBackend) foldOntoStored(ctx context.Context, m gitloom.Memory) (gitloom.Memory, error) {
	existing, err := g.client.Get(ctx, m.Path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	switch {
	case isNotFound(err):
		return m, nil // first memory about this subject
	case err != nil:
		return m, fmt.Errorf("gitloom: could not read %s to preserve it: %w", m.Path, err)
	case existing == nil || strings.TrimSpace(existing.Content) == "":
		return m, fmt.Errorf("gitloom: %s read back empty; refusing to overwrite what is there", m.Path)
	}
	merged := AppendSection(existing.Content, m)
	merged.Tags = unionStrings(existing.Tags, merged.Tags, 24)
	merged.Cues = unionStrings(existing.Cues, merged.Cues, 5)
	merged.Related = unionStrings(existing.Related, merged.Related, 32)
	return merged, nil
}

// isNotFound reports the API's "no memory at that path", which is the normal
// first-write case rather than a failure.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *gitloom.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 404
	}
	return false
}

func (g *gitloomBackend) setHealth(ok bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	was := g.healthy
	g.healthy = ok
	if err != nil {
		g.lastErr = err.Error()
	} else {
		g.lastErr = ""
	}
	// Logged only on transition, so an outage is one line rather than one per
	// query for as long as it lasts.
	if was && !ok {
		g.log.Warn("gitloom: memory layer is unreachable; falling back to the local store",
			zap.String("error", g.lastErr))
	} else if !was && ok {
		g.log.Info("gitloom: memory layer recovered")
	}
}

func (g *gitloomBackend) status() (bool, string) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.healthy, g.lastErr
}

// recent returns the most recently touched memories, newest last.
//
// GitLoom has no "recent" call — it is a memory, not a log — so this walks the
// table of contents and takes the leaves. That is an approximation of recency
// and says so, rather than pretending the store answers a question it does not.
func (g *gitloomBackend) recent(ctx context.Context, n int) ([]MemoryEntry, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	res, err := g.client.Tree(cctx, &gitloom.TreeOptions{Namespace: g.cfg.Namespace, Depth: 3})
	if err != nil {
		g.setHealth(false, err)
		return nil, err
	}
	g.setHealth(true, nil)

	leaves := leafNodes(&res.Tree, nil)
	if n > 0 && len(leaves) > n {
		leaves = leaves[len(leaves)-n:]
	}
	out := make([]MemoryEntry, 0, len(leaves))
	for _, l := range leaves {
		content := strings.TrimSpace(l.Summary)
		if content == "" {
			content = strings.TrimSpace(l.Title)
		}
		out = append(out, MemoryEntry{
			ID: l.Path, Namespace: g.cfg.Namespace, Role: RoleGitLoom, Content: content,
		})
	}
	return out, nil
}

// count reports how many memories the namespace holds.
func (g *gitloomBackend) count(ctx context.Context) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	res, err := g.client.Tree(cctx, &gitloom.TreeOptions{Namespace: g.cfg.Namespace, Depth: 8})
	if err != nil {
		g.setHealth(false, err)
		return 0, err
	}
	g.setHealth(true, nil)
	return len(leafNodes(&res.Tree, nil)), nil
}

// leafNodes flattens a tree to the nodes that are actual memories.
func leafNodes(n *gitloom.TreeNode, acc []gitloom.TreeNode) []gitloom.TreeNode {
	if n == nil {
		return acc
	}
	if len(n.Children) == 0 {
		if strings.TrimSpace(n.Path) != "" {
			acc = append(acc, *n)
		}
		return acc
	}
	for i := range n.Children {
		acc = leafNodes(&n.Children[i], acc)
	}
	return acc
}

// body fetches a memory's text by path, for hits that arrive without one.
//
// Bounded and best-effort: a document that cannot be read is one hit without
// text, not a failed search. The result is capped because a memory file
// accumulates every fact about its subject and the caller wants an excerpt, not
// the file.
func (g *gitloomBackend) body(ctx context.Context, path string) string {
	m, err := g.client.Get(ctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	if err != nil || m == nil {
		return ""
	}
	return truncate(strings.TrimSpace(m.Content), 1200)
}

// search runs a retrieval against GitLoom and renders it as KARMAX results.
func (g *gitloomBackend) search(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	res, err := g.client.Recall(cctx, query, &gitloom.RecallOptions{
		Namespace: g.cfg.Namespace, Limit: topK,
		// Provenance is a git-log walk per hit and is the whole cost of the
		// call — 3.29s with it, 0.40s without, measured on this namespace. The
		// agent is composing a WhatsApp reply, not a citation, so commit hashes
		// are latency it cannot spend. Relations stay: they are free and they
		// are what surfaces a person's whole cluster in one call.
		NoProvenance: true,
	})
	if err != nil {
		g.setHealth(false, err)
		return nil, err
	}
	g.setHealth(true, nil)

	out := make([]SearchResult, 0, len(res.Hits))
	for _, h := range res.Hits {
		// A hit with no text is a hit that answers nothing.
		//
		// The API scores and ranks correctly but returns an empty snippet, so
		// the body is fetched by path. This was invisible while retrieval also
		// had a local arm to fall back on — a fallback masking a broken primary
		// is exactly the failure mode that made keeping two stores worse than
		// depending on one, and removing it is what surfaced this.
		body := strings.TrimSpace(h.Snippet)
		if body == "" {
			body = g.body(cctx, h.Path)
		}
		entry := MemoryEntry{
			// The path IS the handle: it is what Forget takes, so a hit the
			// agent decides is wrong can be deleted without a second lookup.
			ID:        h.Path,
			Namespace: g.cfg.Namespace,
			Role:      RoleGitLoom,
			Content:   body,
		}
		// Provenance carries the date the memory was actually written, which is
		// what lets the agent reason about staleness. Without it every hit
		// looks equally fresh.
		if h.Provenance != nil {
			if t, err := time.Parse(time.RFC3339, h.Provenance.When); err == nil {
				entry.CreatedAt = t
			}
		}
		// Relationships come back with the hit, so the agent sees the cluster
		// (a person → their employer → the deal) without a second round trip.
		if len(h.Relations) > 0 {
			var b strings.Builder
			b.WriteString(entry.Content)
			for _, r := range h.Relations {
				b.WriteString("\n  ↳ ")
				if r.Label != "" {
					b.WriteString(r.Label + ": ")
				}
				b.WriteString(strings.TrimSpace(r.Snippet))
			}
			entry.Content = b.String()
		}
		out = append(out, SearchResult{
			Entry:   entry,
			Score:   h.Score,
			Excerpt: truncate(strings.TrimSpace(h.Snippet), 200),
		})
	}
	// Defined vocabulary is usually the most direct answer the store holds
	// about a term, so it leads rather than being dropped.
	for _, v := range res.Defined {
		if strings.TrimSpace(v.Definition) == "" {
			continue
		}
		out = append([]SearchResult{{
			Entry: MemoryEntry{
				ID: v.Path, Namespace: g.cfg.Namespace, Role: RoleGitLoom,
				Content: v.Term + ": " + v.Definition,
			},
			Score:   1.0,
			Excerpt: truncate(v.Definition, 200),
		}}, out...)
	}
	return out, nil
}
