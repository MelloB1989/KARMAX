package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
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

	pmu       sync.Mutex
	pathLocks map[string]*sync.Mutex
	pending   map[string]pendingWrite
	pseq      uint64
	// logFull is, per loop-log month, the last part known to be full.
	logFull map[string]int
	// now is the pending cache's clock; a test moves it.
	now func() time.Time
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

		pathLocks: map[string]*sync.Mutex{},
		pending:   map[string]pendingWrite{},
		logFull:   map[string]int{},
		now:       time.Now,
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
func (g *gitloomBackend) put(ctx context.Context, m gitloom.NewMemory, occurred time.Time) error {
	if err := g.ensureNamespace(ctx); err != nil {
		g.setHealth(false, err)
		return err
	}
	sentAt := g.now()
	err := g.client.Write(ctx, []gitloom.NewMemory{m}, &gitloom.WriteOptions{Namespace: g.cfg.Namespace, Timezone: g.cfg.Timezone})
	if err != nil {
		if isNamespaceMissing(err) {
			g.nsReady.Store(false)
		}
		g.setHealth(false, err)
		return err
	}
	g.log.Debug("gitloom: wrote", zap.String("path", m.Path), zap.Int("bytes", len(m.Content)))
	g.remember(m, occurred, sentAt)
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
	if IsLoopLog(e) {
		return g.writeLog(cctx, e, path, related)
	}
	_, err := g.writeAt(cctx, e, path, related, 0)
	return err
}

// writeAt folds one memory into the document at path and writes the result,
// unless it would change nothing. With a limit, a document the memory would
// take past it is left as it is and reported full; a document with nothing
// in it yet takes the memory whatever its size.
func (g *gitloomBackend) writeAt(ctx context.Context, e MemoryEntry, path string, related []string, limit int) (full bool, err error) {
	defer g.lockPath(path)()
	m := ToGitLoom(e, path, related)
	merged, occurred, changed, err := g.foldOntoStored(ctx, m, EventTime(e))
	if err != nil {
		g.setHealth(false, err)
		return false, err
	}
	if !changed {
		g.skipped(path)
		return false, nil
	}
	// Longer than the memory alone means it was folded onto what is there.
	if limit > 0 && len(merged.Content) > limit && !sameText(merged.Content, m.Content) {
		return true, nil
	}
	return false, g.put(ctx, merged, occurred)
}

// writeLog files a loop's log entry in the first part of its month's log with
// room for it. See CategoryLog.
func (g *gitloomBackend) writeLog(ctx context.Context, e MemoryEntry, base string, related []string) error {
	for part := g.openLogPart(base); part <= maxLogParts; part++ {
		full, err := g.writeAt(ctx, e, logPart(base, part), related, logDocCap)
		if err != nil || !full {
			return err
		}
		g.logPartFull(base, part)
	}
	return fmt.Errorf("gitloom: %s has %d full parts; refusing to start another", base, maxLogParts)
}

// maxLogParts stops a log that cannot be read right from probing forever.
const maxLogParts = 500

// openLogPart is the first part of a log not known to be full. Remembered
// rather than found by reading each time: reads are not monotonic, and a
// reader still serving a part from before it filled would take it back.
func (g *gitloomBackend) openLogPart(base string) int {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return g.logFull[base] + 1
}

func (g *gitloomBackend) logPartFull(base string, part int) {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	if part > g.logFull[base] {
		g.logFull[base] = part
	}
}

// skipped records a write that would have changed nothing. Every write makes
// the store re-upload the whole repository and its index, so one that says
// only what is already there is not sent at all.
func (g *gitloomBackend) skipped(path string) {
	g.log.Debug("gitloom: write skipped; the store already holds it", zap.String("path", path))
}

// forget removes a document, or ONE dated fact in one ("file.md#slug"). It
// returns once the store has accepted the forget, not once it has applied it.
//
// The store cuts a section itself. KARMAX used to cut it here — read the
// document, find the header whose slug matched, write the rest back — but its
// slugs were not the store's ("19:47" became "19-47" here and "1947" there),
// so three in four section forgets matched nothing and reported success.
//
// A handle that names nothing is ErrNotStored, never a quiet success, and so
// is one KARMAX has already forgotten that the store has yet to apply.
//
// It used to wait — up to the thirty-second call timeout — for the store to
// apply the forget, so that the next write to the document, which folds onto
// what is stored, could not carry the fact straight back. Live ingest takes
// about two minutes, so every forget paid the whole thirty seconds and the
// fact could come back anyway. The forget is now recorded as KARMAX's own
// (see forgotten) until the store shows it: the next write folds onto the
// document without it, and recall leaves it out (see asKnown). Only a section
// the store names without a header, which cannot be cut by header, still
// waits.
func (g *gitloomBackend) forget(ctx context.Context, handle string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	file, _, isSection := strings.Cut(handle, "#")
	defer g.lockPath(file)()
	if g.alreadyForgotten(handle) {
		return fmt.Errorf("%w: %s (already forgotten)", ErrNotStored, handle)
	}
	opts := &gitloom.RecallOptions{Namespace: g.cfg.Namespace}
	// What is there now, as KARMAX knows it: a document just written may not
	// have reached every reader yet, and one just forgotten may still be served.
	var (
		base   *gitloom.StoredMemory
		header string
		err    error
	)
	if isSection {
		var sec *gitloom.StoredMemory
		if sec, err = g.client.Get(cctx, handle, opts); err == nil {
			header = sec.Title
			base, _ = g.current(cctx, file)
		}
	} else {
		_, err = g.current(cctx, handle)
	}
	if err != nil {
		if isAbsent(err) {
			return fmt.Errorf("%w: %s", ErrNotStored, handle)
		}
		g.setHealth(false, err)
		return err
	}
	if err := g.client.Forget(cctx, []string{handle}, &gitloom.WriteOptions{Namespace: g.cfg.Namespace}); err != nil {
		g.setHealth(false, err)
		return err
	}
	g.setHealth(true, nil)
	if !isSection || header != "" {
		g.forgotten(file, base, header, handle)
		return nil
	}

	// No header to cut it by, so nothing of KARMAX's own can stand for the
	// forget: the store's word alone, and a wait for it.
	g.dropPending(file)
	for {
		if _, err := g.client.Get(cctx, handle, opts); isAbsent(err) {
			return nil
		}
		select {
		case <-cctx.Done():
			// Accepted and queued, so it will land; only a write to this
			// document in the meantime can undo it.
			g.log.Warn("gitloom: forget accepted but not yet applied", zap.String("handle", handle))
			return nil
		case <-time.After(forgetPoll):
		}
	}
}

// forgetPoll is how often a forget with no header to record checks whether
// the store has applied it.
const forgetPoll = 100 * time.Millisecond

// foldOntoStored carries everything already at a path into the memory about to
// replace it.
//
// A failure here refuses the write rather than proceeding. Both failure modes
// would otherwise destroy history: a read error degrades to overwrite-with-
// append-of-nothing, and a memory that exists but reads back EMPTY is treated
// as "nothing to preserve" — which is exactly what happened when an older API
// returned only the text before the first ## header for a file written
// entirely as sections.
//
// changed is false when the result would say nothing the store does not
// already hold — the fact is there, and so is every tag, cue and link — and
// then nothing should be sent. The comparison is with the document as KARMAX
// knows it (see current), so a fact just written counts as held before the
// store has caught up, and a fact just forgotten does not.
func (g *gitloomBackend) foldOntoStored(ctx context.Context, m gitloom.NewMemory, at time.Time) (merged gitloom.NewMemory, occurred time.Time, changed bool, err error) {
	existing, err := g.current(ctx, m.Path)
	switch {
	case isAbsent(err):
		return m, at, true, nil // first memory about this subject
	case err != nil:
		return m, at, false, fmt.Errorf("gitloom: could not read %s to preserve it: %w", m.Path, err)
	case existing == nil || strings.TrimSpace(existing.Content) == "":
		return m, at, false, fmt.Errorf("gitloom: %s read back empty; refusing to overwrite what is there", m.Path)
	}
	ownTTL := m.TTL
	merged = AppendSection(existing.Content, m, at, g.loc)
	merged.Tags = unionTags(userTags(existing), merged.Tags)
	merged.Cues = unionStrings(existing.Cues, merged.Cues, 5)
	merged.Related = unionStrings(existing.Related, merged.Related, 32)
	keepExpiry(&merged, existing)
	if sameText(merged.Content, existing.Content) {
		// No fact was added, so the document is no newer than it was, and no
		// less certain: saying a fact again is not news, and dating the file by
		// the repeat made a loop's every run look like fresh information.
		merged.Confidence = max(merged.Confidence, existing.Confidence)
		merged.OccurredAt = gitloom.When{}
		if !existing.OccurredAt.IsZero() {
			merged.OccurredAt = gitloom.At(existing.OccurredAt)
		}
		return merged, existing.OccurredAt, !holds(existing, merged, ownTTL), nil
	}
	// The file is dated by its newest fact.
	latest := laterOf(existing.OccurredAt, at)
	if !latest.IsZero() {
		merged.OccurredAt = gitloom.At(latest)
	}
	return merged, latest, true, nil
}

// sameText reports two bodies that differ at most in surrounding whitespace.
func sameText(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// holds reports whether the store already holds everything a write of m would
// set. ownTTL is the expiry the write itself asks for, as opposed to one
// restated from what is stored.
func holds(existing *gitloom.StoredMemory, m gitloom.NewMemory, ownTTL string) bool {
	return sameText(existing.Content, m.Content) &&
		sameSet(userTags(existing), m.Tags, cleanTag) &&
		sameSet(existing.Cues, m.Cues, foldKey) &&
		sameSet(existing.Related, m.Related, foldKey) &&
		(m.Confidence == 0 || m.Confidence == existing.Confidence) &&
		(ownTTL == "" || ownTTL == ttlLeft(existing.ExpiresAt))
}

func foldKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// sameSet compares two lists as sets of their keys, ignoring empty ones.
func sameSet(a, b []string, key func(string) string) bool {
	set := func(l []string) map[string]bool {
		out := map[string]bool{}
		for _, s := range l {
			if k := key(s); k != "" {
				out[k] = true
			}
		}
		return out
	}
	x, y := set(a), set(b)
	if len(x) != len(y) {
		return false
	}
	for k := range x {
		if !y[k] {
			return false
		}
	}
	return true
}

// keepExpiry carries an incident's remaining time into a rewrite of it. The
// write replaces the whole file, frontmatter included, so a fold that does not
// restate the TTL makes a time-bound task permanent.
func keepExpiry(m *gitloom.NewMemory, existing *gitloom.StoredMemory) {
	if m.TTL != "" || !strings.HasPrefix(m.Path, "incidents/") {
		return
	}
	m.TTL = ttlLeft(existing.ExpiresAt)
}

// ttlLeft restates an expiry as a TTL: the whole days left, rounded up. Empty
// for none, or one already past.
func ttlLeft(expires time.Time) string {
	if expires.IsZero() {
		return ""
	}
	left := time.Until(expires)
	if left <= 0 {
		return ""
	}
	return fmt.Sprintf("%dd", int(left.Hours()/24)+1)
}

// ttlExpiry is when a TTL written now runs out; zero for one KARMAX does not write.
func ttlExpiry(ttl string, from time.Time) time.Time {
	days, ok := strings.CutSuffix(strings.TrimSpace(ttl), "d")
	if !ok {
		return time.Time{}
	}
	n, err := strconv.Atoi(days)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return from.Add(time.Duration(n) * 24 * time.Hour)
}

// isNotFound reports the API's "no memory at that path", which is the normal
// first-write case rather than a failure. A missing namespace is not this.
func isNotFound(err error) bool {
	var apiErr *gitloom.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code != codeNamespaceNotFound
}

const codeNamespaceNotFound = "namespace_not_found"

// ErrNotStored is a forget whose handle names nothing in the store.
var ErrNotStored = errors.New("memory: nothing is stored at that handle")

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
		updated := firstTime(stamp(m.UpdatedAt, m.Updated), stamp(m.CreatedAt, m.Created))
		content, ok := g.asKnown(m.Path, strings.TrimSpace(firstNonEmpty(m.Content, m.Snippet)), updated)
		if !ok {
			continue
		}
		out = append(out, MemoryEntry{
			ID: m.Path, Namespace: g.cfg.Namespace, Role: RoleGitLoom,
			Content:   content,
			Category:  firstNonEmpty(m.Tier, "facts"),
			CreatedAt: updated, OccurredAt: m.OccurredAt,
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
		// Not what KARMAX has forgotten, though the store may still say it.
		known, ok := g.asKnown(m.Path, body, firstTime(stamp(m.UpdatedAt, m.Updated), stamp(m.CreatedAt, m.Created)))
		if !ok {
			continue
		}
		if known != body {
			body = known
			m.Snippet = "" // it may quote what was cut
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
				if _, ok := g.asKnown(r.Path, r.Snippet, time.Time{}); !ok {
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
		if _, ok := g.asKnown(v.Path, v.Definition, time.Time{}); !ok {
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
