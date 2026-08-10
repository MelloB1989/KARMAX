package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/MelloB1989/karmax/internal/store"
)

// ErrNoRemote means the store has no relationship graph of its own to read.
// The caller builds one instead, which is what the local tier has always done.
var ErrNoRemote = errors.New("memory: GitLoom is not configured for this namespace")

// The read and maintenance views the app and the reviewer need.
//
// These exist because several features read memory in ways Search does not
// cover: the 3D graph wants the relationship structure, the cleanup flow wants
// a batch to ask a question about, the reviewer wants the oldest time-sensitive
// entries. All three used to query SQLite directly, which was the same thing as
// asking the store — until GitLoom became the store and it silently stopped
// being the same thing.
//
// Everything here answers from whichever store is actually holding memories,
// and each method says what it costs, because "list everything" is cheap
// against a table and is not against a hosted memory.

// Graph is the relationship structure between memories.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Links []GraphLink `json:"links"`
	// Total is how many memories exist, which can exceed the nodes rendered.
	Total int `json:"total"`
	// Truncated reports that the store had more than the limit allowed.
	Truncated bool `json:"truncated"`
}

// GraphNode is one memory in the graph.
type GraphNode struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Content  string `json:"content"`
	Category string `json:"category"`
}

// GraphLink is one relationship.
//
// The json names match what the app already reads for a link, so the graph
// changing where it comes from is invisible to the client.
type GraphLink struct {
	FromID   string `json:"from"`
	ToID     string `json:"to"`
	Relation string `json:"relation"`
}

// HasRemote reports whether GitLoom is the store for this namespace.
//
// Callers use it to choose between asking the store for something it answers
// natively and doing the work themselves — the relationship graph being the
// case that matters, since GitLoom has one and SQLite does not.
func (m *Manager) HasRemote() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remote != nil
}

// Graph returns the relationship structure between memories.
//
// From GitLoom when it is the store: it builds the graph during ingestion, from
// the Related links a memory declares and the cross-mentions in its prose. That
// replaces an LLM pass KARMAX used to run every two hours over the local rows —
// a model call, on a timer, to reconstruct something the store already knew.
//
// Without GitLoom there is nothing to read, and the caller falls back to
// generating links itself.
func (m *Manager) Graph(ctx context.Context, limit int) (*Graph, error) {
	m.mu.Lock()
	remote := m.remote
	m.mu.Unlock()
	if remote == nil {
		return nil, ErrNoRemote
	}
	return remote.graph(ctx, limit)
}

// Survey returns what the store holds, cheaply: paths and their summaries, in
// ONE call, without the bodies.
//
// The bodies are the expensive part — one request each against a hosted store —
// and every caller here filters before it needs them. Fetch the survivors with
// Load rather than asking for everything up front.
func (m *Manager) Survey(ctx context.Context, limit int) ([]MemoryEntry, error) {
	m.mu.Lock()
	remote := m.remote
	m.mu.Unlock()
	if remote != nil {
		return remote.survey(ctx, limit)
	}

	entries, err := m.db.ListMemoryEntries(m.namespace, limit)
	if err != nil {
		return nil, err
	}
	return fromStored(entries), nil
}

// Load returns one memory in full.
func (m *Manager) Load(ctx context.Context, handle string) (*MemoryEntry, error) {
	m.mu.Lock()
	remote := m.remote
	m.mu.Unlock()
	if remote != nil {
		return remote.load(ctx, handle)
	}

	entries, err := m.db.ListMemoryEntries(m.namespace, 5000)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.ID == handle {
			out := fromStored([]store.StoredMemoryEntry{e})
			return &out[0], nil
		}
	}
	return nil, nil
}

// Update rewrites a memory's content, preserving everything else about it.
//
// The metadata is carried over deliberately: a GitLoom write replaces the whole
// file, so writing just the corrected text would silently drop the tags, cues
// and relationships that make the memory findable — leaving a memory that is
// accurate and unreachable.
func (m *Manager) Update(ctx context.Context, handle, content string) error {
	if err := safety.CheckWrite(content); err != nil {
		return err
	}
	m.mu.Lock()
	remote := m.remote
	m.mu.Unlock()
	if remote != nil {
		return remote.update(ctx, handle, content)
	}
	return m.db.UpdateMemoryEntry(handle, content)
}

// ForgetFact removes a memory only when the handle addresses ONE fact, and
// reports whether it did.
//
// The distinction matters because a GitLoom path is a SUBJECT, not a fact:
// every memory about a subject folds into one file as ## sections, and one file
// here holds 166 of them. A staleness review asks about a single item — "that
// June deadline, still open?" — so answering "it's done" must not be read as
// permission to erase everything KARMAX knows about that subject.
//
// Deleting the wrong thing here is unrecoverable from the operator's side: they
// answered a one-line question and would have no idea what went with it. So the
// coarse case keeps the memory and says so, and the caller tells them.
func (m *Manager) ForgetFact(ctx context.Context, handle string) (bool, error) {
	m.mu.Lock()
	remote := m.remote
	m.mu.Unlock()
	if remote == nil {
		// A local row is one fact, so there is nothing to be coarse about.
		return true, m.db.DeleteMemoryEntry(handle)
	}

	stored, err := remote.load(ctx, handle)
	if err != nil {
		return false, err
	}
	if stored == nil {
		return false, nil // already gone
	}
	if CountFacts(stored.Content) > 1 {
		return false, nil
	}
	return true, remote.forget(ctx, handle)
}

// Correct records a correction to a memory, and reports whether it replaced
// the whole thing or was added alongside what was already there.
//
// Replacing is right when the handle names ONE fact and wrong when it names a
// subject: overwriting a 166-section file with a single corrected sentence
// would destroy everything KARMAX knows about that subject, silently, in
// response to the operator fixing one detail. In that case the correction is
// appended as a new section, which is how every other fact reaches the file and
// what makes the newer statement win over the older one it contradicts.
func (m *Manager) Correct(ctx context.Context, handle, content string) (replaced bool, err error) {
	if err := safety.CheckWrite(content); err != nil {
		return false, err
	}
	m.mu.Lock()
	remote := m.remote
	m.mu.Unlock()
	if remote == nil {
		return true, m.db.UpdateMemoryEntry(handle, content)
	}

	stored, err := remote.load(ctx, handle)
	if err != nil {
		return false, err
	}
	if stored == nil || CountFacts(stored.Content) <= 1 {
		return true, remote.update(ctx, handle, content)
	}
	return false, remote.append(ctx, handle, content)
}

// CountFacts reports how many separate facts a memory's markdown holds.
//
// Sections are what KARMAX writes one fact into, so counting the ## headers is
// counting the facts. A file with none is a single fact written as prose.
func CountFacts(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "## ") {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

// graph reads GitLoom's relationship graph.
func (g *gitloomBackend) graph(ctx context.Context, limit int) (*Graph, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	res, err := g.client.Graph(cctx, &gitloom.RecallOptions{Namespace: g.cfg.Namespace, Limit: limit})
	if err != nil {
		g.setHealth(false, err)
		return nil, err
	}
	g.setHealth(true, nil)

	// The graph call returns structure, not text — so the summaries come from
	// one extra call rather than none. Without it the app renders several
	// hundred unlabelled dots, which is a picture of the memory rather than a
	// way into it.
	summaries := map[string]string{}
	if surveyed, err := g.survey(ctx, 0); err == nil {
		for _, s := range surveyed {
			summaries[s.ID] = s.Content
		}
	}

	out := &Graph{Truncated: res.Truncated}
	files := map[string]bool{}
	for _, n := range res.Nodes {
		// Directories are structure, not memories. Rendering them as nodes puts
		// "facts" and "facts/context" in the graph as though they were things
		// the operator was told.
		if n.Kind == "dir" {
			continue
		}
		files[n.Path] = true
		summary := summaries[n.Path]
		out.Nodes = append(out.Nodes, GraphNode{
			ID:       n.Path,
			Title:    firstNonEmpty(n.Title, summaryTitle(summary), graphTitleFor("", n.Path)),
			Content:  firstNonEmpty(summary, n.Title),
			Category: firstNonEmpty(n.Tier, "facts"),
		})
	}
	for _, e := range res.Edges {
		// An edge to a node that was truncated away would render as a link into
		// nothing.
		if !files[e.Src] || !files[e.Dst] {
			continue
		}
		out.Links = append(out.Links, GraphLink{
			FromID: e.Src, ToID: e.Dst, Relation: firstNonEmpty(e.Label, "related"),
		})
	}
	out.Total = len(out.Nodes)
	return out, nil
}

// survey lists every memory with its summary, in one call.
func (g *gitloomBackend) survey(ctx context.Context, limit int) ([]MemoryEntry, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	res, err := g.client.Tree(cctx, &gitloom.TreeOptions{Namespace: g.cfg.Namespace, Depth: 8})
	if err != nil {
		g.setHealth(false, err)
		return nil, err
	}
	g.setHealth(true, nil)

	leaves := leafNodes(&res.Tree, nil)
	if limit > 0 && len(leaves) > limit {
		leaves = leaves[:limit]
	}
	out := make([]MemoryEntry, 0, len(leaves))
	for _, l := range leaves {
		content := strings.TrimSpace(l.Summary)
		if content == "" {
			content = graphTitleFor(l.Title, l.Path)
		}
		out = append(out, MemoryEntry{
			ID: l.Path, Namespace: g.cfg.Namespace, Role: RoleGitLoom,
			Content: content, Category: firstNonEmpty(l.Tier, "facts"),
		})
	}
	return out, nil
}

// load fetches one memory in full, with the date it was last written.
func (g *gitloomBackend) load(ctx context.Context, path string) (*MemoryEntry, error) {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	m, err := g.client.Get(cctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		g.setHealth(false, err)
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	g.setHealth(true, nil)

	e := &MemoryEntry{
		ID: m.Path, Namespace: g.cfg.Namespace, Role: RoleGitLoom,
		Content: strings.TrimSpace(m.Content), Tags: m.Tags,
		Category: firstNonEmpty(m.Tier, "facts"),
	}
	// Updated over Created: staleness is about when a fact was last confirmed,
	// not when it was first written down.
	for _, stamp := range []string{m.Updated, m.Created} {
		if t, err := time.Parse(time.RFC3339, stamp); err == nil {
			e.CreatedAt = t
			break
		}
	}
	return e, nil
}

// append adds a correction to a memory as a new section, keeping the rest.
func (g *gitloomBackend) append(ctx context.Context, path, content string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	existing, err := g.client.Get(cctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	if err != nil {
		g.setHealth(false, err)
		return err
	}
	if existing == nil || strings.TrimSpace(existing.Content) == "" {
		return fmt.Errorf("gitloom: %s read back empty; refusing to overwrite what is there", path)
	}
	merged := AppendSection(existing.Content, gitloom.Memory{Path: path, Content: content})
	merged.Tags = unionStrings(existing.Tags, merged.Tags, 24)
	merged.Cues = unionStrings(existing.Cues, merged.Cues, 5)
	merged.Related = unionStrings(existing.Related, merged.Related, 32)
	if err := g.client.Write(cctx, []gitloom.Memory{merged}, nil); err != nil {
		g.setHealth(false, err)
		return err
	}
	g.setHealth(true, nil)
	return nil
}

// update rewrites one memory's text, keeping its metadata.
func (g *gitloomBackend) update(ctx context.Context, path, content string) error {
	cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	existing, err := g.client.Get(cctx, path, &gitloom.RecallOptions{Namespace: g.cfg.Namespace})
	if err != nil {
		g.setHealth(false, err)
		return err
	}
	m := gitloom.Memory{Path: path, Content: content}
	if existing != nil {
		m.Tags, m.Cues, m.Related = existing.Tags, existing.Cues, existing.Related
		m.Confidence = existing.Confidence
	}
	if err := g.client.Write(cctx, []gitloom.Memory{m}, nil); err != nil {
		g.setHealth(false, err)
		return err
	}
	g.setHealth(true, nil)
	return nil
}

// fromStored converts local rows to the shared entry shape.
func fromStored(entries []store.StoredMemoryEntry) []MemoryEntry {
	out := make([]MemoryEntry, 0, len(entries))
	for _, e := range entries {
		var tags []string
		_ = json.Unmarshal([]byte(e.Tags), &tags)
		out = append(out, MemoryEntry{
			ID: e.ID, AgentID: e.AgentID, Namespace: e.Namespace, Role: e.Role,
			Content: e.Content, Tags: tags, Category: e.Category,
			Importance: e.Importance, Pinned: e.Pinned, CreatedAt: e.CreatedAt,
		})
	}
	return out
}

// summaryTitle takes the first clause of a summary as a node label.
func summaryTitle(summary string) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, ".\n"); i > 12 {
		s = s[:i]
	}
	if len(s) > 70 {
		s = s[:70] + "…"
	}
	return s
}

// graphTitleFor names a node, falling back to its filename.
func graphTitleFor(title, path string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	name := path
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".md")
	return strings.ReplaceAll(name, "-", " ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// SortByOldest orders entries stalest-first, which is what a review wants.
func SortByOldest(entries []MemoryEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt.Before(entries[j].CreatedAt) })
}
