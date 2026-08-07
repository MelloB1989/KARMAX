package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// The GitLoom-backed memory layer.
//
// GitLoom owns retrieval: BM25 over the body, a separate embedded arm over the
// cues, a relationship graph with validity windows, and per-result provenance
// from git. KARMAX's local SQLite is demoted from source of truth to a
// write-through cache — it still holds every memory, but it is only read when
// the network is unavailable.
//
// Two properties make that dependency safe to take:
//
//   - A write is durable locally before it is sent. It goes to SQLite and to
//     the outbox in the same call; the flusher's only job is delivery. An
//     outage costs latency, not memories.
//   - A read that fails falls back to the local store. Degraded retrieval
//     quality beats a daemon that has forgotten who its operator is.

// GitLoomConfig configures the remote memory layer.
type GitLoomConfig struct {
	APIKey    string
	BaseURL   string
	Namespace string
	// FlushInterval is how often the outbox is drained. Writes are batched, so
	// this trades delivery latency against the number of commits GitLoom makes.
	FlushInterval time.Duration
	// BatchSize caps one delivery. One batch is one repo fetch, one commit
	// round and one push on GitLoom's side, so bigger batches are cheaper.
	BatchSize int
	// Timeout bounds a single API call.
	Timeout time.Duration
	// PrimaryLocal is the local namespace that Namespace refers to. An operator
	// who sets GITLOOM_NAMESPACE means "put my memory there" — so that one
	// namespace maps across unchanged, and only ADDITIONAL agents get a suffix
	// to keep them out of each other's memory.
	PrimaryLocal string
}

// gitloomBackend delivers to and reads from GitLoom Cloud.
type gitloomBackend struct {
	client *gitloom.Client
	cfg    GitLoomConfig
	db     *store.Store
	log    *zap.Logger

	stopCh chan struct{}
	stopMu sync.Once
	wg     sync.WaitGroup

	// healthy tracks whether the last call succeeded, so a degraded read path
	// is reported once rather than on every query.
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
		APIKey:        key,
		BaseURL:       strings.TrimSpace(os.Getenv("GITLOOM_BASE_URL")),
		Namespace:     strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE")),
		FlushInterval: 20 * time.Second,
		BatchSize:     100,
		Timeout:       30 * time.Second,
		PrimaryLocal:  namespace,
	}
	if cfg.Namespace == "" {
		cfg.Namespace = namespace
	}
	return cfg, true
}

func newGitLoomBackend(cfg GitLoomConfig, db *store.Store, log *zap.Logger) *gitloomBackend {
	opts := []gitloom.Option{gitloom.WithNamespace(cfg.Namespace)}
	if cfg.BaseURL != "" {
		opts = append(opts, gitloom.WithBaseURL(cfg.BaseURL))
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 20 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	g := &gitloomBackend{
		client:  gitloom.New(cfg.APIKey, opts...),
		cfg:     cfg,
		db:      db,
		log:     log,
		stopCh:  make(chan struct{}),
		healthy: true,
	}
	g.wg.Add(1)
	go g.flushLoop()
	return g
}

func (g *gitloomBackend) stop() {
	g.stopMu.Do(func() { close(g.stopCh) })
	g.wg.Wait()
}

// enqueue records a memory for delivery. It never talks to the network: the
// caller is on the write path of a message the operator is waiting on.
func (g *gitloomBackend) enqueue(e MemoryEntry, path string, related []string) error {
	m := ToGitLoom(e, path, related)
	payload, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return g.db.EnqueueOutbox(store.OutboxItem{
		ID: e.ID, Namespace: g.cfg.Namespace, Op: "write", Payload: string(payload),
	})
}

func (g *gitloomBackend) enqueueForget(id, path string) error {
	return g.db.EnqueueOutbox(store.OutboxItem{
		ID: "forget:" + id, Namespace: g.cfg.Namespace, Op: "forget", Payload: path,
	})
}

// flushLoop drains the outbox until stopped.
func (g *gitloomBackend) flushLoop() {
	defer g.wg.Done()
	// A first drain shortly after start, so memories written while the daemon
	// was down or offline are not held until the first full interval elapses.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-g.stopCh:
			// One last drain on the way out: a clean shutdown should not strand
			// a memory that was written a second ago.
			ctx, cancel := context.WithTimeout(context.Background(), g.cfg.Timeout)
			g.flush(ctx)
			cancel()
			return
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), g.cfg.Timeout)
			g.flush(ctx)
			cancel()
			timer.Reset(g.cfg.FlushInterval)
		}
	}
}

// flush delivers one batch. Writes are sent together in a single call; deletes
// go individually because they are rare and carry no batching benefit.
func (g *gitloomBackend) flush(ctx context.Context) {
	items, err := g.db.PendingOutbox(g.cfg.Namespace, g.cfg.BatchSize)
	if err != nil {
		g.log.Warn("gitloom: could not read the outbox", zap.Error(err))
		return
	}
	if len(items) == 0 {
		return
	}

	var (
		memories []gitloom.Memory
		writeIDs []string
		badIDs   []string
	)
	for _, it := range items {
		switch it.Op {
		case "forget":
			if err := g.client.Forget(ctx, []string{it.Payload}, nil); err != nil {
				g.setHealth(false, err)
				_ = g.db.FailOutbox([]string{it.ID}, err.Error())
				continue
			}
			_ = g.db.AckOutbox([]string{it.ID})
		default:
			var m gitloom.Memory
			if json.Unmarshal([]byte(it.Payload), &m) != nil {
				// Undecodable now is undecodable forever; drop it rather than
				// blocking every memory behind it.
				badIDs = append(badIDs, it.ID)
				continue
			}
			memories = append(memories, m)
			writeIDs = append(writeIDs, it.ID)
		}
	}
	if len(badIDs) > 0 {
		g.log.Error("gitloom: discarding undecodable outbox rows", zap.Int("count", len(badIDs)))
		_ = g.db.AckOutbox(badIDs)
	}
	if len(memories) == 0 {
		return
	}

	// A hosted write replaces the whole file, and KARMAX files every memory
	// about one subject at one path. Writing them raw would make the newest
	// fact about CampX delete the forty already there — so this batch is folded
	// per path, then folded again onto whatever is already stored.
	memories = g.mergeByPath(ctx, memories)

	if err := g.client.Write(ctx, memories, nil); err != nil {
		g.setHealth(false, err)
		// Left in the outbox deliberately: the next tick retries, and the
		// attempt counter is what makes a permanently-poisoned batch visible.
		_ = g.db.FailOutbox(writeIDs, err.Error())
		g.log.Warn("gitloom: delivery failed, memories stay queued",
			zap.Int("queued", len(memories)), zap.Error(err))
		return
	}
	g.setHealth(true, nil)
	if err := g.db.AckOutbox(writeIDs); err != nil {
		// The memories are stored; failing to forget that they were queued only
		// costs a duplicate write, which is idempotent by path.
		g.log.Warn("gitloom: delivered but could not clear the outbox", zap.Error(err))
	}
	g.log.Info("gitloom: delivered memories",
		zap.Int("count", len(memories)), zap.String("namespace", g.cfg.Namespace))
}

// mergeByPath folds a batch so each path is written once, carrying everything
// already stored there plus the new sections.
//
// One Get per distinct path, not per memory: a flush of fifty facts about five
// subjects costs five reads. A read that fails degrades to an overwrite-with-
// append-of-nothing, which would lose history, so it is skipped instead — the
// memory stays in the outbox and is retried when the API answers again.
func (g *gitloomBackend) mergeByPath(ctx context.Context, in []gitloom.Memory) []gitloom.Memory {
	byPath := map[string][]gitloom.Memory{}
	order := make([]string, 0, len(in))
	for _, m := range in {
		if _, seen := byPath[m.Path]; !seen {
			order = append(order, m.Path)
		}
		byPath[m.Path] = append(byPath[m.Path], m)
	}

	out := make([]gitloom.Memory, 0, len(order))
	for _, path := range order {
		merged := UnionMemories(byPath[path])

		existing, err := g.client.Get(ctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
		switch {
		case isNotFound(err):
			// First memory about this subject; nothing to preserve.
		case err != nil:
			g.log.Warn("gitloom: could not read the existing memory; leaving it queued rather than overwriting",
				zap.String("path", path), zap.Error(err))
			continue
		case existing == nil || strings.TrimSpace(existing.Content) == "":
			// The memory EXISTS but read back empty. A write is a whole-file
			// replace, so treating this as "nothing to preserve" destroys
			// whatever is actually there — which is exactly what happened when
			// an older API returned only the text before the first ## header
			// for a file written entirely as sections. Refuse and retry.
			g.log.Warn("gitloom: existing memory read back empty; refusing to overwrite it",
				zap.String("path", path))
			continue
		default:
			merged = AppendSection(existing.Content, merged)
			merged.Tags = unionStrings(existing.Tags, merged.Tags, 24)
			merged.Cues = unionStrings(existing.Cues, merged.Cues, 5)
			merged.Related = unionStrings(existing.Related, merged.Related, 32)
		}
		out = append(out, merged)
	}
	return out
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

// search runs a retrieval against GitLoom and renders it as KARMAX results.
func (g *gitloomBackend) search(ctx context.Context, query string, topK int) ([]SearchResult, error) {
	res, err := g.client.Recall(ctx, query, &gitloom.RecallOptions{
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
		entry := MemoryEntry{
			// The path IS the handle: it is what Forget takes, so a hit the
			// agent decides is wrong can be deleted without a second lookup.
			ID:        h.Path,
			Namespace: g.cfg.Namespace,
			Role:      RoleGitLoom,
			Content:   strings.TrimSpace(h.Snippet),
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
