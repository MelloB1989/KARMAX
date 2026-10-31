package store

import (
	"database/sql"
	"strings"
	"time"
)

// Requests: something somebody asked for, owned until it is done and its
// answer has reached them.
//
// A request starts as a turn — a row in agent_turns, written before the work
// begins — and most finish there. One that cannot finish in its turn (it ran
// out of time, or is plainly long) continues as a task. The two rows change
// together, in one transaction, so a restart never finds a request in
// neither place or in both: TakeOverRequest closes the turn as handed off and
// creates the task, or does neither.

// TaskIDForRequest is the id of the task that continues a request. Derived
// rather than minted, so taking a request over twice — the model created its
// task and the turn then ran out of time, or a restart repeats a take-over
// that had already committed — finds the same task instead of starting a
// second one on the same work.
func TaskIDForRequest(requestID string) string { return "req-" + requestID }

// TakeOverRequest makes t (t.RequestID must be set) the owner of its request
// and closes the request's turn as handed off, in one transaction. created
// is false when a task already owned the request; that task is returned
// unchanged. why is recorded on the turn row.
func (s *Store) TakeOverRequest(t Task, why string) (task Task, created bool, err error) {
	t.ID = TaskIDForRequest(t.RequestID)
	t = prepareTask(t)

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin()
	if err != nil {
		return Task{}, false, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	existing, err := scanTask(tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, t.ID))
	switch {
	case err == nil:
		task = existing
	case err == sql.ErrNoRows:
		if _, err = tx.Exec(insertTaskSQL, taskArgs(t)...); err != nil {
			return Task{}, false, err
		}
		task, created = t, true
	default:
		return Task{}, false, err
	}
	note := "continued as task " + task.ID
	if why = strings.TrimSpace(why); why != "" {
		note += ": " + why
	}
	if _, err = tx.Exec(`
UPDATE agent_turns SET status = 'handed_off', finished_at = ?, error = ? WHERE event_id = ?`,
		time.Now(), note, t.RequestID); err != nil {
		return Task{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Task{}, false, err
	}
	return task, created, nil
}

// GetTaskForRequest is the task that took the request over, if one has.
func (s *Store) GetTaskForRequest(requestID string) (Task, bool, error) {
	if strings.TrimSpace(requestID) == "" {
		return Task{}, false, nil
	}
	return s.GetTask(TaskIDForRequest(requestID))
}

// AgentTurnByEvent reads the turn journal's row for a request, if it has one.
func (s *Store) AgentTurnByEvent(eventID string) (AgentTurn, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var t AgentTurn
	var fin sql.NullTime
	err := s.queryRow(`
SELECT id, agent_id, event_id, event_kind, event_json, status, attempt, started_at, finished_at, error
FROM agent_turns WHERE event_id = ?`, eventID).Scan(&t.ID, &t.AgentID, &t.EventID, &t.EventKind,
		&t.EventJSON, &t.Status, &t.Attempt, &t.StartedAt, &fin, &t.Error)
	if err == sql.ErrNoRows {
		return AgentTurn{}, false, nil
	}
	if err != nil {
		return AgentTurn{}, false, err
	}
	if fin.Valid {
		t.FinishedAt = &fin.Time
	}
	return t, true, nil
}

// Delivery states, as BeginDelivery reports them.
const (
	// DeliveryNew: nothing was attempted before; send it.
	DeliveryNew = "new"
	// DeliveryPending: an earlier attempt began and never recorded whether it
	// sent. Look at the chat before sending again (SentSince).
	DeliveryPending = "pending"
	// DeliverySent: it was delivered. Do not send it again.
	DeliverySent = "sent"
)

// Delivery is one message owed to one place, under a key the caller derives
// from what it is (the result of a task, the acknowledgement of a request).
// An empty ChannelID and Target is the operator, in the app. FilePath, when
// set, is a file to send; Content is then how the chat records it.
type Delivery struct {
	Key, ChannelID, Target, Content, FilePath string
	CreatedAt                                 time.Time
}

// CompleteTaskRound records a round's outcome and what it owes its requester
// in one transaction: the deliveries are written as claimed-but-not-sent, so
// a crash after the round was recorded still finds them (OwedDeliveries).
func (s *Store) CompleteTaskRound(id string, u TaskUpdate, owed []Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.begin()
	if err != nil {
		return err
	}
	q, args := updateTaskSQL(id, u)
	if _, err := tx.Exec(q, args...); err != nil {
		_ = tx.Rollback()
		return err
	}
	now := time.Now()
	for _, d := range owed {
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO deliveries (delivery_key, channel_id, target, content, file_path, status, created_at)
VALUES (?, ?, ?, ?, ?, 'sending', ?)`, d.Key, d.ChannelID, d.Target, d.Content, d.FilePath, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// OwedDeliveries are deliveries claimed and never recorded as sent, under a
// key prefix, oldest first: what a restart still owes.
func (s *Store) OwedDeliveries(prefix string) ([]Delivery, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.query(`
SELECT delivery_key, channel_id, target, content, file_path, created_at FROM deliveries
WHERE status = 'sending' AND delivery_key LIKE ? ORDER BY created_at ASC, delivery_key ASC`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.Key, &d.ChannelID, &d.Target, &d.Content, &d.FilePath, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// BeginDelivery claims a delivery before it is sent. It reports what an
// earlier attempt left: nothing (new), a claim with no outcome (pending,
// with when that attempt began), or a finished send.
func (s *Store) BeginDelivery(d Delivery) (state string, since time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var status string
	err = s.queryRow(`SELECT status, created_at FROM deliveries WHERE delivery_key = ?`, d.Key).Scan(&status, &since)
	switch {
	case err == nil:
		if status == "sent" {
			return DeliverySent, since, nil
		}
		return DeliveryPending, since, nil
	case err != sql.ErrNoRows:
		return "", time.Time{}, err
	}
	now := time.Now()
	if _, err := s.exec(`
INSERT OR IGNORE INTO deliveries (delivery_key, channel_id, target, content, file_path, status, created_at)
VALUES (?, ?, ?, ?, ?, 'sending', ?)`, d.Key, d.ChannelID, d.Target, d.Content, d.FilePath, now); err != nil {
		return "", time.Time{}, err
	}
	return DeliveryNew, now, nil
}

// FinishDelivery records that a claimed delivery reached its target.
func (s *Store) FinishDelivery(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`UPDATE deliveries SET status = 'sent', sent_at = ? WHERE delivery_key = ?`, time.Now(), key)
	return err
}

// SentSince reports whether text went out to target at or after since — how
// a delivery whose attempt never recorded its outcome finds out it did send.
// Every send through comms is stored right after it succeeds.
func (s *Store) SentSince(target, text string, since time.Time) bool {
	text = strings.TrimSpace(text)
	if strings.TrimSpace(target) == "" || text == "" {
		return false
	}
	msgs, err := s.ListChannelMessages(target, 50)
	if err != nil {
		return false
	}
	floor := since.Truncate(time.Second)
	for _, m := range msgs {
		if m.Direction != "outbound" || m.CreatedAt.Before(floor) {
			continue
		}
		if strings.TrimSpace(m.Content) == text {
			return true
		}
	}
	return false
}
