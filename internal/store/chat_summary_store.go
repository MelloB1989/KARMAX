package store

import (
	"database/sql"
	"time"
)

// ChatSummary is the "cold" per-chat context produced by the background
// summarization worker for chats the operator is no longer actively using.
type ChatSummary struct {
	ChatJID         string    `json:"chat_jid"`
	ChatName        string    `json:"chat_name"`
	IsGroup         bool      `json:"is_group"`
	Summary         string    `json:"summary"`
	MessageCount    int       `json:"message_count"`
	OwnMessageCount int       `json:"own_message_count"`
	LastMessageAt   time.Time `json:"last_message_at"`
	SummarizedAt    time.Time `json:"summarized_at"`
	Status          string    `json:"status"` // pending | summarized | skipped
}

// UpsertChatSummary inserts or replaces a chat's cold summary.
func (s *Store) UpsertChatSummary(c ChatSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(`
		INSERT INTO chat_summaries
			(chat_jid, chat_name, is_group, summary, message_count, own_message_count, last_message_at, summarized_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid) DO UPDATE SET
			chat_name=excluded.chat_name,
			is_group=excluded.is_group,
			summary=excluded.summary,
			message_count=excluded.message_count,
			own_message_count=excluded.own_message_count,
			last_message_at=excluded.last_message_at,
			summarized_at=excluded.summarized_at,
			status=excluded.status`,
		c.ChatJID, c.ChatName, boolToInt(c.IsGroup), c.Summary, c.MessageCount,
		c.OwnMessageCount, c.LastMessageAt, c.SummarizedAt, c.Status)
	return err
}

// GetChatSummary returns the stored summary for a chat, or nil if none.
func (s *Store) GetChatSummary(jid string) (*ChatSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	row := s.db.QueryRow(`
		SELECT chat_jid, chat_name, is_group, summary, message_count, own_message_count,
		       last_message_at, summarized_at, status
		FROM chat_summaries WHERE chat_jid = ?`, jid)
	c, err := scanChatSummary(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// SearchChatSummaries does a simple LIKE search over summaries and chat names.
func (s *Store) SearchChatSummaries(query string, limit int) ([]ChatSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	like := "%" + query + "%"
	rows, err := s.db.Query(`
		SELECT chat_jid, chat_name, is_group, summary, message_count, own_message_count,
		       last_message_at, summarized_at, status
		FROM chat_summaries
		WHERE status = 'summarized' AND (summary LIKE ? OR chat_name LIKE ?)
		ORDER BY last_message_at DESC LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChatSummaries(rows)
}

// ListChatSummaries returns recent summarized chats.
func (s *Store) ListChatSummaries(limit int) ([]ChatSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT chat_jid, chat_name, is_group, summary, message_count, own_message_count,
		       last_message_at, summarized_at, status
		FROM chat_summaries WHERE status = 'summarized'
		ORDER BY last_message_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChatSummaries(rows)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanChatSummary(r rowScanner) (*ChatSummary, error) {
	var c ChatSummary
	var isGroup int
	var lastMsg, summarized sql.NullTime
	if err := r.Scan(&c.ChatJID, &c.ChatName, &isGroup, &c.Summary, &c.MessageCount,
		&c.OwnMessageCount, &lastMsg, &summarized, &c.Status); err != nil {
		return nil, err
	}
	c.IsGroup = isGroup != 0
	if lastMsg.Valid {
		c.LastMessageAt = lastMsg.Time
	}
	if summarized.Valid {
		c.SummarizedAt = summarized.Time
	}
	return &c, nil
}

func scanChatSummaries(rows *sql.Rows) ([]ChatSummary, error) {
	var out []ChatSummary
	for rows.Next() {
		c, err := scanChatSummary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}
