package store

import (
	"encoding/json"
	"time"
)

type StoredMemoryEntry struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Namespace string    `json:"namespace"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Tags      string    `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) InsertMemoryEntry(e StoredMemoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`INSERT INTO memory_entries (id, agent_id, namespace, role, content, tags) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.AgentID, e.Namespace, e.Role, e.Content, e.Tags)
	return err
}

func (s *Store) ListMemoryEntries(namespace string, limit int) ([]StoredMemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, agent_id, namespace, role, content, tags, created_at FROM memory_entries WHERE namespace = ? ORDER BY created_at DESC LIMIT ?`, namespace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []StoredMemoryEntry
	for rows.Next() {
		var e StoredMemoryEntry
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Namespace, &e.Role, &e.Content, &e.Tags, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *Store) SearchMemoryEntries(namespace, query string, limit int) ([]StoredMemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pattern := "%" + query + "%"
	rows, err := s.db.Query(`SELECT id, agent_id, namespace, role, content, tags, created_at FROM memory_entries WHERE namespace = ? AND (content LIKE ? OR tags LIKE ?) ORDER BY created_at DESC LIMIT ?`,
		namespace, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []StoredMemoryEntry
	for rows.Next() {
		var e StoredMemoryEntry
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Namespace, &e.Role, &e.Content, &e.Tags, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (s *Store) CountMemoryEntries(namespace string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_entries WHERE namespace = ?`, namespace).Scan(&count)
	return count, err
}

func (s *Store) DeleteMemoryEntry(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM memory_entries WHERE id = ?`, id)
	return err
}

func (s *Store) UpdateMemoryEntry(id, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE memory_entries SET content = ? WHERE id = ?`, content, id)
	return err
}

func (s *Store) DeleteOldMemoryEntries(namespace string, keepLast int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`DELETE FROM memory_entries WHERE namespace = ? AND id NOT IN (SELECT id FROM memory_entries WHERE namespace = ? ORDER BY created_at DESC LIMIT ?)`,
		namespace, namespace, keepLast)
	return err
}

// PageIndex tree persistence

func (s *Store) SavePageIndexTree(namespace, treeJSON, tocJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO pageindex_trees (namespace, tree_blob, toc_blob, updated_at)
		VALUES (?, ?, ?, datetime('now'))
		ON CONFLICT(namespace) DO UPDATE SET tree_blob=excluded.tree_blob, toc_blob=excluded.toc_blob, updated_at=datetime('now')`,
		namespace, treeJSON, tocJSON)
	return err
}

func (s *Store) LoadPageIndexTree(namespace string) (treeJSON, tocJSON string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.db.QueryRow(`SELECT tree_blob, COALESCE(toc_blob, '') FROM pageindex_trees WHERE namespace = ?`, namespace).Scan(&treeJSON, &tocJSON)
	return
}

func (s *Store) SavePageIndexNodes(namespace string, nodes []PageIndexNode) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`DELETE FROM pageindex_nodes WHERE namespace = ?`, namespace)
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`INSERT INTO pageindex_nodes (id, namespace, node_id, parent_id, path, title, summary, search_text, raw_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, n := range nodes {
		_, err = stmt.Exec(n.ID, namespace, n.NodeID, n.ParentID, n.Path, n.Title, n.Summary, n.SearchText, n.RawJSON)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) SearchPageIndexNodes(namespace, query string, limit int) ([]PageIndexNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pattern := "%" + query + "%"
	rows, err := s.db.Query(`SELECT id, namespace, node_id, parent_id, path, title, summary, search_text, raw_json FROM pageindex_nodes WHERE namespace = ? AND (title LIKE ? OR summary LIKE ? OR search_text LIKE ?) LIMIT ?`,
		namespace, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []PageIndexNode
	for rows.Next() {
		var n PageIndexNode
		if err := rows.Scan(&n.ID, &n.Namespace, &n.NodeID, &n.ParentID, &n.Path, &n.Title, &n.Summary, &n.SearchText, &n.RawJSON); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

type PageIndexNode struct {
	ID         string `json:"id"`
	Namespace  string `json:"namespace"`
	NodeID     string `json:"node_id"`
	ParentID   string `json:"parent_id,omitempty"`
	Path       string `json:"path,omitempty"`
	Title      string `json:"title"`
	Summary    string `json:"summary,omitempty"`
	SearchText string `json:"search_text,omitempty"`
	RawJSON    string `json:"raw_json,omitempty"`
}

// Webhook event persistence

type StoredWebhookEvent struct {
	ID         string    `json:"id"`
	Route      string    `json:"route"`
	Method     string    `json:"method"`
	Headers    string    `json:"headers"`
	Body       string    `json:"body"`
	ReceivedAt time.Time `json:"received_at"`
	Dispatched bool      `json:"dispatched"`
}

func (s *Store) SaveWebhookEvent(e StoredWebhookEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dispatched := 0
	if e.Dispatched {
		dispatched = 1
	}

	headersJSON, _ := json.Marshal(e.Headers)
	_, err := s.db.Exec(`INSERT INTO webhook_events (id, route, method, headers, body, dispatched) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, e.Route, e.Method, string(headersJSON), e.Body, dispatched)
	return err
}

func (s *Store) ListWebhookEvents(limit int) ([]StoredWebhookEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, route, method, headers, body, received_at, dispatched FROM webhook_events ORDER BY received_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []StoredWebhookEvent
	for rows.Next() {
		var e StoredWebhookEvent
		var dispatched int
		if err := rows.Scan(&e.ID, &e.Route, &e.Method, &e.Headers, &e.Body, &e.ReceivedAt, &dispatched); err != nil {
			return nil, err
		}
		e.Dispatched = dispatched == 1
		events = append(events, e)
	}
	return events, nil
}
