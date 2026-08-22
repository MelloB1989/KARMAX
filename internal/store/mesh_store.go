package store

import (
	"database/sql"
	"errors"
	"time"
)

// Persistent trust state for the mesh.
//
// The peer table IS the access-control list. A signature proves who sent an
// envelope; this table decides whether that sender is allowed to say anything
// at all. Keeping the two separate is deliberate — a system that treats "the
// signature verified" as "the request is permitted" authenticates strangers
// flawlessly and then does whatever they ask.

// Peer states. A peer only reaches "accepted" through an explicit human
// decision, so the default for an unknown instance is silence.
const (
	PeerPending  = "pending"  // asked to connect; awaiting the operator
	PeerAccepted = "accepted" // may exchange messages
	PeerBlocked  = "blocked"  // refused; further requests are dropped on sight
	PeerRevoked  = "revoked"  // was accepted, no longer is
)

// ErrNoPeer reports an instance this one has never heard of.
var ErrNoPeer = errors.New("store: no such peer")

// MeshPeer is another KARMAX instance and this instance's stance toward it.
type MeshPeer struct {
	ID     string // the peer's Ed25519 signing key — the real identity
	Name   string // the peer's self-declared label; display only, never trusted
	BoxPub string
	// Endpoint is where to reach it. Mutable, unlike ID: an instance can move
	// hosts without becoming a different instance.
	Endpoint    string
	State       string
	Fingerprint string
	// Scopes bound what an accepted peer may do. Empty means messages only —
	// admitting someone is not the same as letting them query memory.
	Scopes     []string
	Direction  string // "in" if they asked us, "out" if we asked them
	Note       string
	CreatedAt  time.Time
	DecidedAt  *time.Time
	LastSeenAt *time.Time
}

// UpsertMeshPeer records or updates a peer.
//
// State is deliberately NOT overwritten on conflict. A repeated connection
// request from a blocked instance must not quietly return it to pending, which
// would turn "no" into a decision an attacker can retry until someone clicks
// the wrong button.
func (s *Store) UpsertMeshPeer(p MeshPeer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO mesh_peers (id, name, box_pub, endpoint, state, fingerprint, scopes, direction, note, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  box_pub = excluded.box_pub,
  endpoint = CASE WHEN excluded.endpoint != '' THEN excluded.endpoint ELSE mesh_peers.endpoint END,
  fingerprint = excluded.fingerprint`,
		p.ID, p.Name, p.BoxPub, p.Endpoint, p.State, p.Fingerprint,
		encodeCSV(p.Scopes), p.Direction, p.Note, time.Now())
	return err
}

// SetMeshPeerState records the operator's decision about a peer.
func (s *Store) SetMeshPeerState(id, state string, scopes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	res, err := s.exec(
		`UPDATE mesh_peers SET state = ?, scopes = ?, decided_at = ? WHERE id = ?`,
		state, encodeCSV(scopes), now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoPeer
	}
	return nil
}

// MeshPeerByID returns one peer.
func (s *Store) MeshPeerByID(id string) (*MeshPeer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.queryRow(`
SELECT id, name, box_pub, endpoint, state, fingerprint, scopes, direction, note,
       created_at, decided_at, last_seen_at
FROM mesh_peers WHERE id = ?`, id)
	p, err := scanPeer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoPeer
	}
	return p, err
}

// ListMeshPeers returns peers, optionally filtered by state.
func (s *Store) ListMeshPeers(state string) ([]MeshPeer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := `SELECT id, name, box_pub, endpoint, state, fingerprint, scopes, direction, note,
	             created_at, decided_at, last_seen_at FROM mesh_peers`
	args := []any{}
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC`

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MeshPeer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// TouchMeshPeer records that a peer was heard from.
func (s *Store) TouchMeshPeer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`UPDATE mesh_peers SET last_seen_at = ? WHERE id = ?`, time.Now(), id)
	return err
}

func scanPeer(r rowScanner) (*MeshPeer, error) {
	var p MeshPeer
	var scopes string
	if err := r.Scan(&p.ID, &p.Name, &p.BoxPub, &p.Endpoint, &p.State, &p.Fingerprint,
		&scopes, &p.Direction, &p.Note, &p.CreatedAt, &p.DecidedAt, &p.LastSeenAt); err != nil {
		return nil, err
	}
	p.Scopes = decodeCSV(scopes)
	return &p, nil
}

// MeshMessage is one delivered mesh message, kept so the agent and the operator
// can see what other instances have said.
type MeshMessage struct {
	ID         string
	PeerID     string
	PeerName   string
	Kind       string
	Direction  string // in | out
	Body       string
	Via        string // org key, when delivered under an org certificate
	Origin     string // the instance the work traces back to, when delegated
	ReceivedAt time.Time
}

// RecordMeshMessage stores a message. Bounded elsewhere by pruning; this is a
// log, not a queue.
func (s *Store) RecordMeshMessage(m MeshMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO mesh_messages (id, peer_id, peer_name, kind, direction, body, via, origin, received_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`,
		m.ID, m.PeerID, m.PeerName, m.Kind, m.Direction, m.Body, m.Via, m.Origin, time.Now())
	return err
}

// RecentMeshMessages returns the latest mesh traffic.
func (s *Store) RecentMeshMessages(limit int) ([]MeshMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.query(`
SELECT id, peer_id, peer_name, kind, direction, body, COALESCE(via,''), COALESCE(origin,''), received_at
FROM mesh_messages ORDER BY received_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MeshMessage
	for rows.Next() {
		var m MeshMessage
		if err := rows.Scan(&m.ID, &m.PeerID, &m.PeerName, &m.Kind, &m.Direction,
			&m.Body, &m.Via, &m.Origin, &m.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func encodeCSV(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func decodeCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
