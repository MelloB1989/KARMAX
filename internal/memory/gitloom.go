package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	// Timezone is the operator's IANA zone; empty means UTC.
	Timezone string
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

	loc *time.Location
	// nsReady is set once the namespace is known to exist.
	nsReady atomic.Bool
	nsMu    sync.Mutex
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
		Timezone:     SystemTimezone(),
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
	loc := time.UTC
	if l, err := time.LoadLocation(cfg.Timezone); err == nil && cfg.Timezone != "" {
		loc = l
	}
	return &gitloomBackend{
		client:  gitloom.New(cfg.APIKey, opts...),
		cfg:     cfg,
		log:     log,
		healthy: true,
		loc:     loc,
	}
}

// ensureNamespace creates the namespace once; a write to a missing one is a 404.
func (g *gitloomBackend) ensureNamespace(ctx context.Context) error {
	if g.nsReady.Load() {
		return nil
	}
	g.nsMu.Lock()
	defer g.nsMu.Unlock()
	if g.nsReady.Load() {
		return nil
	}
	if err := g.client.CreateNamespace(ctx, g.cfg.Namespace); err != nil && !isNotFound(err) {
		// A plain 404 is a server without the endpoint, which makes it on first write.
		return fmt.Errorf("gitloom: could not create namespace %s: %w", g.cfg.Namespace, err)
	}
	g.nsReady.Store(true)
	return nil
}

// put writes formed memories, creating the namespace first.
func (g *gitloomBackend) put(ctx context.Context, ms ...gitloom.NewMemory) error {
	if err := g.ensureNamespace(ctx); err != nil {
		g.setHealth(false, err)
		return err
	}
	err := g.client.Write(ctx, ms, &gitloom.WriteOptions{Namespace: g.cfg.Namespace, Timezone: g.cfg.Timezone})
	if err != nil {
		if isNamespaceMissing(err) {
			g.nsReady.Store(false)
		}
		g.setHealth(false, err)
		return err
	}
	g.setHealth(true, nil)
	return nil
}

// userTags are the caller's tags; Get's merged list also holds inferred ones.
func userTags(m *gitloom.StoredMemory) []string {
	if len(m.UserTags) > 0 {
		return m.UserTags
	}
	return m.Tags
}

// laterOf is the more recent of two instants.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// write stores one memory, folding it onto whatever is already at its path.
//
// A GitLoom write replaces the whole file and KARMAX files every memory about
// one subject at one path, so writing raw would make the newest fact about a
// subject delete the forty already there.
func (g *gitloomBackend) write(ctx context.Context, e MemoryEntry, path string, related []string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	if err := g.ensureNamespace(cctx); err != nil {
		g.setHealth(false, err)
		return err
	}
	merged, err := g.foldOntoStored(cctx, ToGitLoom(e, path, related), EventTime(e))
	if err != nil {
		g.setHealth(false, err)
		return err
	}
	return g.put(cctx, merged)
}

// forgetSection removes ONE dated fact from the document that holds its
// subject, leaving the rest of the document intact.
//
// The store has no delete-a-section call: a write replaces the whole file. So
// the section is cut out here and the remainder written back. Deleting the file
// instead — the only thing the API offers directly — would take every other
// fact about that subject with it.
func (g *gitloomBackend) forgetSection(ctx context.Context, file, slug string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	existing, err := g.client.Get(cctx, file, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	if err != nil {
		if isAbsent(err) {
			return nil // already gone
		}
		g.setHealth(false, err)
		return err
	}
	if existing == nil || strings.TrimSpace(existing.Content) == "" {
		return nil
	}
	remaining, removed := RemoveSection(existing.Content, slug)
	if !removed {
		return nil // nothing matched; not an error, the fact is not there
	}
	if strings.TrimSpace(remaining) == "" {
		// The last fact in the document: drop the document itself rather than
		// leaving an empty file that still answers searches.
		return g.forget(ctx, file)
	}
	out := gitloom.NewMemory{
		Path: existing.Path, Content: remaining, Tags: userTags(existing),
		Confidence: existing.Confidence, Cues: existing.Cues, Related: existing.Related,
	}
	if !existing.OccurredAt.IsZero() {
		out.OccurredAt = gitloom.At(existing.OccurredAt)
	}
	return g.put(cctx, out)
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
func (g *gitloomBackend) foldOntoStored(ctx context.Context, m gitloom.NewMemory, at time.Time) (gitloom.NewMemory, error) {
	existing, err := g.client.Get(ctx, m.Path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	switch {
	case isAbsent(err):
		return m, nil // first memory about this subject
	case err != nil:
		return m, fmt.Errorf("gitloom: could not read %s to preserve it: %w", m.Path, err)
	case existing == nil || strings.TrimSpace(existing.Content) == "":
		return m, fmt.Errorf("gitloom: %s read back empty; refusing to overwrite what is there", m.Path)
	}
	merged := AppendSection(existing.Content, m, at, g.loc)
	merged.Tags = unionTags(userTags(existing), merged.Tags)
	merged.Cues = unionStrings(existing.Cues, merged.Cues, 5)
	merged.Related = unionStrings(existing.Related, merged.Related, 32)
	// The file is dated by its newest fact.
	if latest := laterOf(existing.OccurredAt, at); !latest.IsZero() {
		merged.OccurredAt = gitloom.At(latest)
	}
	return merged, nil
}

// isNotFound reports the API's "no memory at that path", which is the normal
// first-write case rather than a failure. A missing namespace is not this.
func isNotFound(err error) bool {
	var apiErr *gitloom.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code != codeNamespaceNotFound
}

const codeNamespaceNotFound = "namespace_not_found"

// isNamespaceMissing reports a 404 for the namespace itself.
func isNamespaceMissing(err error) bool {
	var apiErr *gitloom.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code == codeNamespaceNotFound
}

// isAbsent is either: for a read, nothing there is nothing there.
func isAbsent(err error) bool { return isNotFound(err) || isNamespaceMissing(err) }

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

// listCap is the most a filter-only recall returns in one call.
const listCap = 200

// allTiers makes a filter-only recall list the whole namespace.
var allTiers = []string{"facts", "incidents", "rules", "skills"}

// recent returns the most recently updated memories, newest last; more than one
// listing can serve falls back to the unordered tree walk.
func (g *gitloomBackend) recent(ctx context.Context, n int) ([]MemoryEntry, error) {
	if n > 0 && n <= listCap {
		out, err := g.list(ctx, n, time.Time{})
		if err != nil {
			return nil, err
		}
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out, nil
	}
	// Surveyed at full depth. Walking a shallow tree returns DIRECTORIES as its
	// leaves — they have paths and no summaries, so every entry came back with
	// empty content and the app showed a list of blank rows.
	all, err := g.survey(ctx, 0)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// list returns memories newest-updated first, optionally only those before until.
func (g *gitloomBackend) list(ctx context.Context, limit int, until time.Time) ([]MemoryEntry, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	// MaxChars is a budget for the whole response; without it each is a whole file.
	res, err := g.client.Recall(cctx, "", &gitloom.RecallOptions{
		Namespace: g.cfg.Namespace, Limit: limit, Tiers: allTiers, Until: until,
		TimeField: gitloom.TimeUpdated, TZ: g.cfg.Timezone, MaxChars: max(limit*300, 6000),
		NoProvenance: true, NoRelations: true, NoContext: true,
	})
	if err != nil {
		if isNamespaceMissing(err) {
			return nil, nil
		}
		g.setHealth(false, err)
		return nil, err
	}
	g.setHealth(true, nil)

	out := make([]MemoryEntry, 0, len(res.Memories))
	for _, m := range res.Memories {
		out = append(out, MemoryEntry{
			ID: m.Path, Namespace: g.cfg.Namespace, Role: RoleGitLoom,
			Content:   strings.TrimSpace(firstNonEmpty(m.Content, m.Snippet)),
			Category:  firstNonEmpty(m.Tier, "facts"),
			CreatedAt: firstTime(stamp(m.UpdatedAt, m.Updated), stamp(m.CreatedAt, m.Created)), OccurredAt: m.OccurredAt,
		})
	}
	return out, nil
}

// stamp is a time field, or the RFC 3339 string an older server sends instead.
func stamp(t time.Time, legacy string) time.Time {
	if !t.IsZero() {
		return t
	}
	if p, err := time.Parse(time.RFC3339, legacy); err == nil {
		return p
	}
	return time.Time{}
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// count reports how many memories the namespace holds.
func (g *gitloomBackend) count(ctx context.Context) (int, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	res, err := g.client.Tree(cctx, &gitloom.TreeOptions{Namespace: g.cfg.Namespace, Depth: 8})
	if err != nil {
		if isNamespaceMissing(err) {
			return 0, nil
		}
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

// SearchOpts narrow a retrieval. The zero value asks the question as written.
type SearchOpts struct {
	// Since and Until bound the time the memory's subject happened.
	Since, Until time.Time
	// Tags matches any of these; TagsAll requires every one.
	Tags, TagsAll []string
}

func (o SearchOpts) filtered() bool {
	return !o.Since.IsZero() || !o.Until.IsZero() || len(o.Tags) > 0 || len(o.TagsAll) > 0
}

// within trims results to the range; one with no date is kept.
func (o SearchOpts) within(rs []SearchResult) []SearchResult {
	if o.Since.IsZero() && o.Until.IsZero() {
		return rs
	}
	out := rs[:0:0]
	for _, r := range rs {
		t := EventTime(r.Entry)
		if !t.IsZero() && ((!o.Since.IsZero() && t.Before(o.Since)) || (!o.Until.IsZero() && t.After(o.Until))) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// charsPerHit is each hit's share of the content budget; uncapped, a file is 100KB.
const charsPerHit = 1200

// search runs a retrieval against GitLoom and renders it as KARMAX results.
func (g *gitloomBackend) search(ctx context.Context, query string, topK int, opts SearchOpts) ([]SearchResult, error) {
	// Nothing to look for is not a failed backend.
	if strings.TrimSpace(query) == "" && !opts.filtered() {
		return nil, nil
	}
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	ro := &gitloom.RecallOptions{
		Namespace: g.cfg.Namespace, Limit: topK,
		Rank: gitloom.RankFused, MaxChars: min(max(topK*charsPerHit, 3000), 24000),
		Tags: opts.Tags, TagsAll: opts.TagsAll, Since: opts.Since, Until: opts.Until,
		TZ: g.cfg.Timezone,
		// Provenance is a git-log walk per memory and was the whole cost of
		// the call — 3.29s with it, 0.40s without, measured on this namespace.
		// Relations stay: they are free, and they surface a person's whole
		// cluster in one call.
		NoProvenance: true,
	}
	if !opts.Since.IsZero() || !opts.Until.IsZero() {
		ro.TimeField = gitloom.TimeOccurred
	}
	res, err := g.client.Recall(cctx, query, ro)
	if err == nil {
		err = unreadRecall(res)
	}
	if err != nil {
		if errors.Is(err, gitloom.ErrNoQuery) || isNamespaceMissing(err) {
			return nil, nil
		}
		g.setHealth(false, err)
		return nil, err
	}
	g.setHealth(true, nil)
	g.log.Debug("gitloom: recall",
		zap.String("namespace", g.cfg.Namespace), zap.String("rank", res.Rank),
		zap.Bool("rank_fallback", res.RankFallback), zap.Int("hits", len(res.Memories)),
		zap.Int64("millis", res.Millis), zap.Int64("embed_ms", res.Timings.EmbedMillis),
		zap.Int64("lanes_ms", res.Timings.LanesMillis), zap.Int64("rank_ms", res.Timings.RankMillis))

	out := make([]SearchResult, 0, len(res.Memories))
	for _, m := range res.Memories {
		// Content is the whole memory. Before the retrieve API returned it, the
		// snippet came back empty and every hit cost a second request for its
		// body; that fetch remains only for a memory that still arrives bare.
		body := strings.TrimSpace(m.Content)
		if body == "" {
			body = strings.TrimSpace(m.Snippet)
		}
		if body == "" {
			body = g.body(cctx, m.Path)
		}
		entry := MemoryEntry{
			// The path IS the handle: it is what Forget takes, so a hit the
			// agent decides is wrong can be deleted without a second lookup.
			ID:        m.Path,
			Namespace: g.cfg.Namespace,
			Role:      RoleGitLoom,
			Content:   body,
		}
		// When the memory was written, which is what lets the agent reason
		// about staleness. Without it every hit looks equally fresh.
		entry.CreatedAt, entry.OccurredAt = stamp(m.CreatedAt, m.Created), m.OccurredAt
		// Relationships come back with the memory, so the agent sees the
		// cluster (a person → their employer → the deal) without another call.
		if len(m.Related) > 0 {
			var b strings.Builder
			b.WriteString(entry.Content)
			for _, r := range m.Related {
				// A relation with no text is a label pointing at nothing; in a
				// prompt it is only noise.
				if strings.TrimSpace(r.Snippet) == "" {
					continue
				}
				b.WriteString("\n  ↳ ")
				if r.Label != "" {
					b.WriteString(r.Label + ": ")
				}
				b.WriteString(strings.TrimSpace(r.Snippet))
			}
			entry.Content = b.String()
		}
		excerpt := strings.TrimSpace(m.Snippet)
		if excerpt == "" {
			excerpt = body
		}
		out = append(out, SearchResult{
			Entry:   entry,
			Score:   m.Score,
			Excerpt: truncate(excerpt, 200),
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
