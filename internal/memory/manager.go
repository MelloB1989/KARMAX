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
