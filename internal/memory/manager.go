package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// defaultMaxEntries caps how many memories a namespace keeps before the
// forgetting curve prunes the least-valuable non-pinned ones.
const defaultMaxEntries = 5000

type Manager struct {
	agentID    string
	namespace  string
	baseDir    string
	db         *store.Store
	tree       *TreeNode
	log        *zap.Logger
	dirty      bool
	maxEntries int
	mu         sync.Mutex
	stopCh     chan struct{}
}

func NewManager(agentID, namespace, baseDir string, db *store.Store, log *zap.Logger) *Manager {
	dir := filepath.Join(baseDir, namespace)
	os.MkdirAll(dir, 0755)

	m := &Manager{
		agentID:    agentID,
		namespace:  namespace,
		baseDir:    dir,
		db:         db,
		log:        log,
		maxEntries: defaultMaxEntries,
		stopCh:     make(chan struct{}),
	}

	m.loadTree()
	go m.reindexWorker()

	return m
}

// SetMaxEntries overrides the capacity cap for the forgetting curve.
func (m *Manager) SetMaxEntries(n int) {
	if n > 0 {
		m.mu.Lock()
		m.maxEntries = n
		m.mu.Unlock()
	}
}

// Namespace returns the manager's namespace identifier.
func (m *Manager) Namespace() string {
	return m.namespace
}

// ProfilePath returns the path to the curated "about me" profile document.
// This is a single living Markdown file the agent rewrites over time, distinct
// from the append-only session log.
func (m *Manager) ProfilePath() string {
	return filepath.Join(m.baseDir, "ABOUT_ME.md")
}

// ReadProfile returns the current curated profile document, or an empty string
// if it has not been created yet.
func (m *Manager) ReadProfile() (string, error) {
	data, err := os.ReadFile(m.ProfilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// WriteProfile overwrites the curated profile document with content.
func (m *Manager) WriteProfile(content string) error {
	return os.WriteFile(m.ProfilePath(), []byte(content), 0644)
}

func (m *Manager) Write(entry MemoryEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.Namespace == "" {
		entry.Namespace = m.namespace
	}
	if entry.AgentID == "" {
		entry.AgentID = m.agentID
	}
	entry.CreatedAt = time.Now()

	if err := m.appendToMarkdown(entry); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}

	tagsJSON, _ := json.Marshal(entry.Tags)
	if err := m.db.InsertMemoryEntry(store.StoredMemoryEntry{
		ID:         entry.ID,
		AgentID:    entry.AgentID,
		Namespace:  entry.Namespace,
		Role:       entry.Role,
		Content:    entry.Content,
		Tags:       string(tagsJSON),
		Category:   entry.Category,
		Importance: entry.Importance,
		Pinned:     entry.Pinned,
		ExpiresAt:  entry.ExpiresAt,
	}); err != nil {
		return fmt.Errorf("insert memory entry: %w", err)
	}

	m.dirty = true
	return nil
}

func (m *Manager) Search(query string, topK int) ([]SearchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if topK <= 0 {
		topK = 5
	}

	// DB-level search
	entries, err := m.db.SearchMemoryEntries(m.namespace, query, topK)
	if err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0, len(entries))
	for _, e := range entries {
		var tags []string
		json.Unmarshal([]byte(e.Tags), &tags)

		excerpt := e.Content
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "..."
		}

		results = append(results, SearchResult{
			Entry: MemoryEntry{
				ID:        e.ID,
				AgentID:   e.AgentID,
				Namespace: e.Namespace,
				Role:      e.Role,
				Content:   e.Content,
				Tags:      tags,
				CreatedAt: e.CreatedAt,
			},
			Excerpt: excerpt,
			Score:   1.0,
		})
	}

	// Also search the tree if available
	if m.tree != nil {
		treeResults := searchTree(m.tree, query, topK)
		for _, tr := range treeResults {
			results = append(results, SearchResult{
				Entry: MemoryEntry{
					Namespace: m.namespace,
					Role:      "memory",
					Content:   tr.Content,
				},
				Excerpt: tr.Summary,
				Score:   0.8,
			})
		}
	}

	return results, nil
}

func (m *Manager) Recent(n int) ([]MemoryEntry, error) {
	entries, err := m.db.ListMemoryEntries(m.namespace, n)
	if err != nil {
		return nil, err
	}

	result := make([]MemoryEntry, 0, len(entries))
	for _, e := range entries {
		var tags []string
		json.Unmarshal([]byte(e.Tags), &tags)

		result = append(result, MemoryEntry{
			ID:        e.ID,
			AgentID:   e.AgentID,
			Namespace: e.Namespace,
			Role:      e.Role,
			Content:   e.Content,
			Tags:      tags,
			CreatedAt: e.CreatedAt,
		})
	}

	// Reverse to chronological order (DB returns DESC)
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return result, nil
}

func (m *Manager) Export(path string) error {
	entries, err := m.db.ListMemoryEntries(m.namespace, 10000)
	if err != nil {
		return err
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Memory Export: %s\n\n", m.namespace))

	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("## [%s] %s — %s\n\n%s\n\n---\n\n",
			e.CreatedAt.Format(time.RFC3339), e.Role, e.AgentID, e.Content))
	}

	return os.WriteFile(path, []byte(sb.String()), 0644)
}

// CountEntries returns the total number of memory entries in this namespace.
func (m *Manager) CountEntries() (int, error) {
	return m.db.CountMemoryEntries(m.namespace)
}

// TreeNodeCount returns the number of nodes in the in-memory tree index.
func (m *Manager) TreeNodeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tree == nil {
		return 0
	}
	return countTreeNodes(m.tree)
}

func countTreeNodes(node *TreeNode) int {
	count := 1
	for _, child := range node.Children {
		count += countTreeNodes(child)
	}
	return count
}

func (m *Manager) Stop() {
	close(m.stopCh)
}

func (m *Manager) appendToMarkdown(entry MemoryEntry) error {
	mdPath := filepath.Join(m.baseDir, "session.md")

	f, err := os.OpenFile(mdPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	line := fmt.Sprintf("\n### [%s] %s\n\n%s\n\n---\n",
		entry.CreatedAt.Format(time.RFC3339), entry.Role, entry.Content)
	_, err = f.WriteString(line)
	return err
}

func (m *Manager) loadTree() {
	treeJSON, _, err := m.db.LoadPageIndexTree(m.namespace)
	if err != nil || treeJSON == "" {
		m.tree = &TreeNode{
			NodeID: "root",
			Title:  m.namespace,
		}
		return
	}

	var tree TreeNode
	if err := json.Unmarshal([]byte(treeJSON), &tree); err != nil {
		m.log.Warn("failed to load pageindex tree", zap.Error(err))
		m.tree = &TreeNode{NodeID: "root", Title: m.namespace}
		return
	}
	m.tree = &tree
}

func (m *Manager) reindexWorker() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			if !m.dirty {
				continue
			}
			m.mu.Lock()
			m.rebuildTree()
			m.dirty = false
			m.mu.Unlock()
		}
	}
}

// Maintain performs one forgetting pass: prune expired memories, then enforce
// the capacity cap by forgetting the least-valuable non-pinned entries. Safe to
// call on demand. Returns the number of memories removed.
func (m *Manager) Maintain() int {
	removed := 0

	if n, err := m.db.PruneExpiredMemories(m.namespace); err != nil {
		m.log.Warn("prune expired memories failed", zap.Error(err))
	} else if n > 0 {
		removed += n
		m.log.Info("pruned expired memories", zap.String("namespace", m.namespace), zap.Int("count", n))
	}

	m.mu.Lock()
	limit := m.maxEntries
	m.mu.Unlock()
	if limit <= 0 {
		limit = defaultMaxEntries
	}

	if total, err := m.db.CountMemoryEntries(m.namespace); err == nil && total > limit {
		over := total - limit
		if n, err := m.db.ForgetLeastValuable(m.namespace, over); err != nil {
			m.log.Warn("forget least-valuable memories failed", zap.Error(err))
		} else if n > 0 {
			removed += n
			m.log.Info("forgot least-valuable memories over capacity",
				zap.String("namespace", m.namespace),
				zap.Int("count", n),
				zap.Int("cap", limit),
			)
		}
	}

	if removed > 0 {
		m.mu.Lock()
		m.dirty = true // trigger a tree rebuild
		m.mu.Unlock()
	}
	return removed
}

func (m *Manager) rebuildTree() {
	entries, err := m.db.ListMemoryEntries(m.namespace, 1000)
	if err != nil {
		m.log.Error("failed to list entries for reindex", zap.Error(err))
		return
	}

	root := &TreeNode{
		NodeID: "root",
		Title:  m.namespace,
	}

	// Group entries under category nodes (root → category → entries) so the
	// tree has real structure for the 2D view and the 3D graph.
	byCat := map[string]*TreeNode{}
	var order []string
	for _, e := range entries {
		cat := categoryOf(e)
		node := byCat[cat]
		if node == nil {
			node = &TreeNode{NodeID: "cat:" + cat, Title: cat}
			byCat[cat] = node
			order = append(order, cat)
		}
		title := cleanEntryTitle(e.Content)
		node.Children = append(node.Children, &TreeNode{
			NodeID:  e.ID,
			Title:   truncate(title, 70),
			Content: e.Content,
			Summary: truncate(title, 120),
		})
	}
	for _, cat := range order {
		n := byCat[cat]
		n.Summary = fmt.Sprintf("%d memories", len(n.Children))
		n.Title = fmt.Sprintf("%s (%d)", cat, len(n.Children))
		root.Children = append(root.Children, n)
	}

	m.tree = root

	treeJSON, _ := json.Marshal(root)
	tocJSON, _ := json.Marshal(toTOC(root))

	if err := m.db.SavePageIndexTree(m.namespace, string(treeJSON), string(tocJSON)); err != nil {
		m.log.Error("failed to save pageindex tree", zap.Error(err))
	}

	nodes := flattenTree(m.namespace, root, "")
	var storeNodes []store.PageIndexNode
	for _, n := range nodes {
		storeNodes = append(storeNodes, store.PageIndexNode{
			ID:         n.NodeID,
			Namespace:  m.namespace,
			NodeID:     n.NodeID,
			ParentID:   n.ParentID,
			Path:       n.Path,
			Title:      n.Title,
			Summary:    n.Summary,
			SearchText: n.SearchText,
		})
	}

	if err := m.db.SavePageIndexNodes(m.namespace, storeNodes); err != nil {
		m.log.Error("failed to save pageindex nodes", zap.Error(err))
	}
}

type flatNode struct {
	NodeID     string
	ParentID   string
	Path       string
	Title      string
	Summary    string
	SearchText string
}

func flattenTree(namespace string, node *TreeNode, parentPath string) []flatNode {
	path := parentPath + "/" + node.NodeID
	result := []flatNode{{
		NodeID:     node.NodeID,
		ParentID:   "",
		Path:       path,
		Title:      node.Title,
		Summary:    node.Summary,
		SearchText: node.Title + " " + node.Summary + " " + node.Content,
	}}

	for _, child := range node.Children {
		childNodes := flattenTree(namespace, child, path)
		for i := range childNodes {
			if childNodes[i].ParentID == "" {
				childNodes[i].ParentID = node.NodeID
			}
		}
		result = append(result, childNodes...)
	}

	return result
}

func toTOC(node *TreeNode) *TreeNode {
	toc := &TreeNode{
		NodeID:  node.NodeID,
		Title:   node.Title,
		Summary: node.Summary,
	}
	for _, child := range node.Children {
		toc.Children = append(toc.Children, toTOC(child))
	}
	return toc
}

func searchTree(node *TreeNode, query string, limit int) []*TreeNode {
	var results []*TreeNode
	q := strings.ToLower(query)

	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		if len(results) >= limit {
			return
		}
		text := strings.ToLower(n.Title + " " + n.Summary + " " + n.Content)
		if strings.Contains(text, q) {
			results = append(results, n)
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	walk(node)

	return results
}

// categoryOf derives an entry's category from its "[category][importance] …"
// content prefix, falling back to its first tag.
func categoryOf(e store.StoredMemoryEntry) string {
	c := strings.TrimSpace(e.Content)
	if strings.HasPrefix(c, "[") {
		if i := strings.Index(c, "]"); i > 1 {
			if cat := strings.TrimSpace(c[1:i]); cat != "" {
				return cat
			}
		}
	}
	var tags []string
	_ = json.Unmarshal([]byte(e.Tags), &tags)
	if len(tags) > 0 && strings.TrimSpace(tags[0]) != "" {
		return strings.TrimSpace(tags[0])
	}
	return "context"
}

// cleanEntryTitle strips the leading "[category][importance]" tags and newlines.
func cleanEntryTitle(content string) string {
	s := strings.TrimSpace(content)
	for strings.HasPrefix(s, "[") {
		i := strings.Index(s, "]")
		if i < 0 {
			break
		}
		s = strings.TrimSpace(s[i+1:])
	}
	return strings.ReplaceAll(s, "\n", " ")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// stopwords is the set of common English words to ignore during keyword search.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "in": true, "on": true, "at": true,
	"to": true, "for": true, "of": true, "with": true, "and": true,
	"or": true, "but": true,
}

// SearchSemantic performs a multi-keyword search that is better than a single
// LIKE query. It splits the query into keywords (dropping stopwords), searches
// each keyword individually, then scores results by how many keywords matched.
// It also searches pageindex nodes and merges results.
func (m *Manager) SearchSemantic(query string, topK int) ([]SearchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if topK <= 0 {
		topK = 10
	}

	keywords := extractKeywords(query)
	if len(keywords) == 0 {
		// Fall back to raw query if everything was a stopword.
		keywords = []string{strings.ToLower(strings.TrimSpace(query))}
	}

	type scored struct {
		entry MemoryEntry
		hits  int
	}

	seen := make(map[string]*scored) // keyed by entry ID

	for _, kw := range keywords {
		entries, err := m.db.SearchMemoryEntries(m.namespace, kw, 50)
		if err != nil {
			continue
		}
		for _, e := range entries {
			var tags []string
			json.Unmarshal([]byte(e.Tags), &tags)

			if s, ok := seen[e.ID]; ok {
				s.hits++
			} else {
				seen[e.ID] = &scored{
					entry: MemoryEntry{
						ID:          e.ID,
						AgentID:     e.AgentID,
						Namespace:   e.Namespace,
						Role:        e.Role,
						Content:     e.Content,
						Tags:        tags,
						Category:    e.Category,
						Importance:  e.Importance,
						Pinned:      e.Pinned,
						AccessCount: e.AccessCount,
						ExpiresAt:   e.ExpiresAt,
						CreatedAt:   e.CreatedAt,
					},
					hits: 1,
				}
			}
		}
	}

	// Search pageindex nodes too.
	for _, kw := range keywords {
		nodes, err := m.db.SearchPageIndexNodes(m.namespace, kw, 50)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			nodeKey := "pi:" + n.NodeID
			if s, ok := seen[nodeKey]; ok {
				s.hits++
			} else {
				seen[nodeKey] = &scored{
					entry: MemoryEntry{
						ID:        n.NodeID,
						Namespace: m.namespace,
						Role:      "memory",
						Content:   n.SearchText,
					},
					hits: 1,
				}
			}
		}
	}

	// Collect and score. The final score blends keyword relevance with what a
	// personal assistant should surface first: important, pinned, recent, and
	// frequently-recalled memories.
	results := make([]SearchResult, 0, len(seen))
	for _, s := range seen {
		excerpt := s.entry.Content
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "..."
		}
		base := float64(s.hits) / float64(len(keywords))
		results = append(results, SearchResult{
			Entry:   s.entry,
			Excerpt: excerpt,
			Score:   base + relevanceBoost(s.entry),
		})
	}

	// Sort descending by score.
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Score > results[i].Score {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if len(results) > topK {
		results = results[:topK]
	}

	// Reinforcement: surfacing a memory counts as using it, so bump its
	// access_count/last_accessed so useful facts rank higher and survive
	// forgetting. Only real entries (not pageindex nodes) carry an importance.
	var touch []string
	for _, r := range results {
		if r.Entry.ID != "" && r.Entry.Role != "memory" {
			touch = append(touch, r.Entry.ID)
		}
	}
	if len(touch) > 0 {
		_ = m.db.TouchMemoryEntries(touch)
	}

	return results, nil
}

// relevanceBoost adds priority for important, pinned, recent, and
// frequently-recalled memories on top of the keyword-match base score.
func relevanceBoost(e MemoryEntry) float64 {
	boost := 0.0

	// Importance: low=-0.25, medium=0, high=+0.25, critical=+0.5.
	if e.Importance > 0 {
		boost += float64(e.Importance-2) * 0.25
	}

	// Pinned facts are always kept front-of-mind.
	if e.Pinned {
		boost += 0.5
	}

	// Recency: fresher memories are more likely relevant right now.
	if !e.CreatedAt.IsZero() {
		age := time.Since(e.CreatedAt)
		switch {
		case age < 7*24*time.Hour:
			boost += 0.5
		case age < 30*24*time.Hour:
			boost += 0.25
		case age < 90*24*time.Hour:
			boost += 0.1
		}
	}

	// Usage: repeatedly-recalled memories matter (capped).
	if e.AccessCount > 0 {
		used := float64(e.AccessCount) * 0.03
		if used > 0.3 {
			used = 0.3
		}
		boost += used
	}

	return boost
}

// FindDuplicates returns existing memory entries whose content has word
// overlap similarity with the given content above the specified threshold.
func (m *Manager) FindDuplicates(content string, threshold float64) ([]MemoryEntry, error) {
	candidates, err := m.SearchSemantic(content, 20)
	if err != nil {
		return nil, err
	}

	var duplicates []MemoryEntry
	for _, c := range candidates {
		sim := jaccardSimilarity(content, c.Entry.Content)
		if sim >= threshold {
			duplicates = append(duplicates, c.Entry)
		}
	}
	return duplicates, nil
}

// IngestFromSummary splits a summary text into logical chunks, deduplicates
// each against existing memory, and saves only novel chunks.
func (m *Manager) IngestFromSummary(summary string, agentID string) error {
	chunks := splitIntoChunks(summary)

	for _, chunk := range chunks {
		chunk = strings.TrimSpace(chunk)
		if len(chunk) <= 20 {
			continue
		}

		// Check for duplicates (threshold 0.7 = 70% word overlap).
		dupes, err := m.FindDuplicates(chunk, 0.7)
		if err != nil {
			// Log and continue — don't abort the whole batch.
			m.log.Warn("duplicate check failed during summary ingest", zap.Error(err))
			continue
		}
		if len(dupes) > 0 {
			continue // skip duplicate
		}

		if err := m.Write(MemoryEntry{
			AgentID:    agentID,
			Namespace:  m.namespace,
			Role:       "system",
			Content:    fmt.Sprintf("[context][medium] %s", chunk),
			Tags:       []string{"auto-ingested", "summary-derived"},
			Category:   "context",
			Importance: 2, // medium; auto-derived, so first to be forgotten under pressure
		}); err != nil {
			m.log.Warn("failed to ingest summary chunk", zap.Error(err))
		}
	}

	return nil
}

// extractKeywords splits text into lowercase words, drops stopwords and short tokens.
func extractKeywords(text string) []string {
	var kws []string
	for _, w := range strings.Fields(strings.ToLower(text)) {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if w == "" || len(w) < 2 {
			continue
		}
		if stopwords[w] {
			continue
		}
		kws = append(kws, w)
	}
	return kws
}

// jaccardSimilarity computes the Jaccard index on whitespace-split word sets.
func jaccardSimilarity(a, b string) float64 {
	setA := makeWordSet(strings.ToLower(a))
	setB := makeWordSet(strings.ToLower(b))

	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}

	intersection := 0
	for w := range setA {
		if setB[w] {
			intersection++
		}
	}

	union := len(setA)
	for w := range setB {
		if !setA[w] {
			union++
		}
	}

	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func makeWordSet(s string) map[string]bool {
	set := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if w != "" {
			set[w] = true
		}
	}
	return set
}

// splitIntoChunks splits text at paragraph boundaries (double newlines) and,
// if those are absent, at sentence boundaries (period + space).
func splitIntoChunks(text string) []string {
	// Try paragraph splitting first.
	paragraphs := strings.Split(text, "\n\n")
	if len(paragraphs) > 1 {
		return paragraphs
	}

	// Fall back to sentence splitting.
	var chunks []string
	remaining := text
	for {
		idx := strings.Index(remaining, ". ")
		if idx < 0 {
			if remaining != "" {
				chunks = append(chunks, remaining)
			}
			break
		}
		chunks = append(chunks, remaining[:idx+1])
		remaining = remaining[idx+2:]
	}
	return chunks
}
