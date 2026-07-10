package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

type StoredMemoryEntry struct {
	ID             string     `json:"id"`
	AgentID        string     `json:"agent_id"`
	Namespace      string     `json:"namespace"`
	Role           string     `json:"role"`
	Content        string     `json:"content"`
	Tags           string     `json:"tags"`
	Category       string     `json:"category"`
	Importance     int        `json:"importance"` // 1=low, 2=medium, 3=high, 4=critical
	Pinned         bool       `json:"pinned"`
	AccessCount    int        `json:"access_count"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// memColumns is the canonical column list for memory_entries SELECTs, matched by
// scanMemoryRows.
const memColumns = "id, agent_id, namespace, role, content, tags, category, importance, pinned, access_count, last_accessed_at, expires_at, created_at"

func scanMemoryRows(rows *sql.Rows) ([]StoredMemoryEntry, error) {
	var entries []StoredMemoryEntry
	for rows.Next() {
		var e StoredMemoryEntry
		var pinned int
		var last, exp sql.NullTime
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Namespace, &e.Role, &e.Content, &e.Tags,
			&e.Category, &e.Importance, &pinned, &e.AccessCount, &last, &exp, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Pinned = pinned != 0
		if last.Valid {
			t := last.Time
			e.LastAccessedAt = &t
		}
		if exp.Valid {
			t := exp.Time
			e.ExpiresAt = &t
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *Store) InsertMemoryEntry(e StoredMemoryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e.Importance == 0 {
		e.Importance = 2 // medium
	}
	_, err := s.db.Exec(`INSERT INTO memory_entries
		(id, agent_id, namespace, role, content, tags, category, importance, pinned, access_count, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AgentID, e.Namespace, e.Role, e.Content, e.Tags, e.Category,
		e.Importance, boolToInt(e.Pinned), e.AccessCount, nullTime(e.ExpiresAt))
	return err
}

func (s *Store) ListMemoryEntries(namespace string, limit int) ([]StoredMemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT `+memColumns+` FROM memory_entries WHERE namespace = ? ORDER BY created_at DESC LIMIT ?`, namespace, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemoryRows(rows)
}

func (s *Store) SearchMemoryEntries(namespace, query string, limit int) ([]StoredMemoryEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	pattern := "%" + query + "%"
	rows, err := s.db.Query(`SELECT `+memColumns+` FROM memory_entries WHERE namespace = ? AND (content LIKE ? OR tags LIKE ? OR category LIKE ?) ORDER BY created_at DESC LIMIT ?`,
		namespace, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemoryRows(rows)
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

// TouchMemoryEntries reinforces recalled memories: it bumps access_count and
// sets last_accessed_at to now, so frequently/recently used facts rank higher
// and survive forgetting. Best-effort per id.
func (s *Store) TouchMemoryEntries(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		_, _ = s.db.Exec(`UPDATE memory_entries SET access_count = access_count + 1, last_accessed_at = datetime('now') WHERE id = ?`, id)
	}
	return nil
}

// SetMemoryPinned pins/unpins an entry. Pinned entries are never auto-forgotten.
func (s *Store) SetMemoryPinned(id string, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE memory_entries SET pinned = ? WHERE id = ?`, boolToInt(pinned), id)
	return err
}

// UpdateMemoryImportance sets an entry's importance (1=low..4=critical).
func (s *Store) UpdateMemoryImportance(id string, importance int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE memory_entries SET importance = ? WHERE id = ?`, importance, id)
	return err
}

// PruneExpiredMemories deletes entries whose expires_at has passed. Returns the
// number removed.
func (s *Store) PruneExpiredMemories(namespace string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM memory_entries WHERE namespace = ? AND expires_at IS NOT NULL AND expires_at <= datetime('now')`, namespace)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ForgetLeastValuable deletes up to `count` non-pinned entries in the namespace,
// choosing the lowest-value ones first: lowest importance, then least-accessed,
// then least-recently used/created. This enforces a capacity cap — the memory's
// forgetting curve. Returns the number removed.
func (s *Store) ForgetLeastValuable(namespace string, count int) (int, error) {
	if count <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM memory_entries WHERE id IN (
		SELECT id FROM memory_entries
		WHERE namespace = ? AND pinned = 0
		ORDER BY importance ASC, access_count ASC, COALESCE(last_accessed_at, created_at) ASC
		LIMIT ?)`, namespace, count)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
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

// PageIndexChildren returns the direct children of a tree node (parent_id =
// parentID), ordered by title. Used for LLM tree traversal: pass "root" for
// the top-level categories, "cat:<category>" for a category's memory leaves.
func (s *Store) PageIndexChildren(namespace, parentID string) ([]PageIndexNode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id, namespace, node_id, parent_id, path, title, summary, search_text, raw_json FROM pageindex_nodes WHERE namespace = ? AND parent_id = ? ORDER BY title`,
		namespace, parentID)
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
	return nodes, rows.Err()
}

// GetPageIndexNode fetches a single tree node by its node_id. found=false when
// there is no such node.
func (s *Store) GetPageIndexNode(namespace, nodeID string) (node PageIndexNode, found bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`SELECT id, namespace, node_id, parent_id, path, title, summary, search_text, raw_json FROM pageindex_nodes WHERE namespace = ? AND node_id = ? LIMIT 1`,
		namespace, nodeID)
	if err := row.Scan(&node.ID, &node.Namespace, &node.NodeID, &node.ParentID, &node.Path, &node.Title, &node.Summary, &node.SearchText, &node.RawJSON); err != nil {
		if err == sql.ErrNoRows {
			return PageIndexNode{}, false, nil
		}
		return PageIndexNode{}, false, err
	}
	return node, true, nil
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
