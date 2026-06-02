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

type Manager struct {
	agentID   string
	namespace string
	baseDir   string
	db        *store.Store
	tree      *TreeNode
	log       *zap.Logger
	dirty     bool
	mu        sync.Mutex
	stopCh    chan struct{}
}

func NewManager(agentID, namespace, baseDir string, db *store.Store, log *zap.Logger) *Manager {
	dir := filepath.Join(baseDir, namespace)
	os.MkdirAll(dir, 0755)

	m := &Manager{
		agentID:   agentID,
		namespace: namespace,
		baseDir:   dir,
		db:        db,
		log:       log,
		stopCh:    make(chan struct{}),
	}

	m.loadTree()
	go m.reindexWorker()

	return m
}

// Namespace returns the manager's namespace identifier.
func (m *Manager) Namespace() string {
	return m.namespace
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
		ID:        entry.ID,
		AgentID:   entry.AgentID,
		Namespace: entry.Namespace,
		Role:      entry.Role,
		Content:   entry.Content,
		Tags:      string(tagsJSON),
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

	for _, e := range entries {
		child := &TreeNode{
			NodeID:  e.ID,
			Title:   fmt.Sprintf("%s — %s", e.Role, e.CreatedAt.Format("2006-01-02 15:04")),
			Content: e.Content,
			Summary: truncate(e.Content, 100),
		}
		root.Children = append(root.Children, child)
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
						ID:        e.ID,
						AgentID:   e.AgentID,
						Namespace: e.Namespace,
						Role:      e.Role,
						Content:   e.Content,
						Tags:      tags,
						CreatedAt: e.CreatedAt,
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

	// Collect and sort by hit count descending.
	results := make([]SearchResult, 0, len(seen))
	for _, s := range seen {
		excerpt := s.entry.Content
		if len(excerpt) > 200 {
			excerpt = excerpt[:200] + "..."
		}
		score := float64(s.hits) / float64(len(keywords))
		results = append(results, SearchResult{
			Entry:   s.entry,
			Excerpt: excerpt,
			Score:   score,
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

	return results, nil
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

		tagsJSON, _ := json.Marshal([]string{"auto-ingested", "summary-derived"})
		_ = tagsJSON

		if err := m.Write(MemoryEntry{
			AgentID:   agentID,
			Namespace: m.namespace,
			Role:      "system",
			Content:   fmt.Sprintf("[context][medium] %s", chunk),
			Tags:      []string{"auto-ingested", "summary-derived"},
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
