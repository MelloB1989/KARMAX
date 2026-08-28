package store

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Webhook endpoints an operator can create, edit and delete while the daemon
// runs, plus a record of what has arrived at them.

// WebhookEndpoint is one custom endpoint.
type WebhookEndpoint struct {
	ID          string
	Slug        string
	Name        string
	Description string
	// Secret verifies a delivery. Compared as an HMAC when SignatureHeader is
	// set, and as a shared token otherwise.
	Secret          string
	SignatureHeader string
	// EventKind is what gets published when a delivery verifies, so a recipe
	// can trigger on it. This is the whole point of a custom webhook: the
	// payload becomes an event the workflow layer already understands.
	EventKind string
	// AgentID hands the payload to one agent instead of leaving it to recipes.
	AgentID   string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
}

// WebhookDelivery is one thing that arrived.
type WebhookDelivery struct {
	ID         string
	Endpoint   string
	Source     string
	Status     string // accepted | rejected | disabled | error
	Detail     string
	BodySample string
	ReceivedAt time.Time
}

// slugPattern is what may become a URL path segment. Anything else is refused
// rather than sanitised: quietly turning "a/../b" into "a-b" hides an attempt
// instead of reporting it.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidWebhookSlug reports whether slug is usable in a path.
func ValidWebhookSlug(slug string) bool { return slugPattern.MatchString(slug) }

// SaveWebhookEndpoint creates or updates an endpoint.
func (s *Store) SaveWebhookEndpoint(e WebhookEndpoint) (WebhookEndpoint, error) {
	e.Slug = strings.ToLower(strings.TrimSpace(e.Slug))
	if !ValidWebhookSlug(e.Slug) {
		return e, errors.New("the path must be lowercase letters, digits and dashes")
	}
	if strings.TrimSpace(e.EventKind) == "" {
		return e, errors.New("an event kind is required — it is what a recipe triggers on")
	}
	if e.ID == "" {
		e.ID = uuid.New().String()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// One slug, one endpoint: two sharing a path would make delivery ambiguous.
	var other string
	err := s.queryRow(`SELECT id FROM webhook_endpoints WHERE slug = ? AND id <> ?`, e.Slug, e.ID).Scan(&other)
	if err == nil {
		return e, errors.New("another webhook already uses the path /" + e.Slug)
	} else if err != sql.ErrNoRows {
		return e, err
	}

	enabled := 0
	if e.Enabled {
		enabled = 1
	}
	_, err = s.exec(`
INSERT INTO webhook_endpoints
  (id, slug, name, description, secret, signature_header, event_kind, agent_id, enabled, created_at, updated_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'), ?)
ON CONFLICT(id) DO UPDATE SET
  slug = excluded.slug, name = excluded.name, description = excluded.description,
  secret = excluded.secret, signature_header = excluded.signature_header,
  event_kind = excluded.event_kind, agent_id = excluded.agent_id,
  enabled = excluded.enabled, updated_at = excluded.updated_at`,
		e.ID, e.Slug, e.Name, e.Description, e.Secret, e.SignatureHeader,
		e.EventKind, e.AgentID, enabled, e.CreatedBy)
	return e, err
}

const webhookCols = `id, slug, name, description, secret, COALESCE(signature_header,''), event_kind, COALESCE(agent_id,''), enabled, created_at, updated_at, COALESCE(created_by,'')`

func scanWebhook(sc interface{ Scan(...any) error }) (WebhookEndpoint, error) {
	var e WebhookEndpoint
	var enabled int
	err := sc.Scan(&e.ID, &e.Slug, &e.Name, &e.Description, &e.Secret, &e.SignatureHeader,
		&e.EventKind, &e.AgentID, &enabled, &e.CreatedAt, &e.UpdatedAt, &e.CreatedBy)
	e.Enabled = enabled == 1
	return e, err
}

// WebhookEndpointBySlug finds the endpoint a delivery is for.
func (s *Store) WebhookEndpointBySlug(slug string) (*WebhookEndpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, err := scanWebhook(s.queryRow(`SELECT `+webhookCols+` FROM webhook_endpoints WHERE slug = ?`, slug))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ListWebhookEndpoints returns every custom endpoint.
func (s *Store) ListWebhookEndpoints() ([]WebhookEndpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.query(`SELECT ` + webhookCols + ` FROM webhook_endpoints ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WebhookEndpoint
	for rows.Next() {
		e, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteWebhookEndpoint removes an endpoint and its delivery history.
func (s *Store) DeleteWebhookEndpoint(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var slug string
	if err := s.queryRow(`SELECT slug FROM webhook_endpoints WHERE id = ?`, id).Scan(&slug); err != nil {
		if err == sql.ErrNoRows {
			return errors.New("no such webhook")
		}
		return err
	}
	if _, err := s.exec(`DELETE FROM webhook_deliveries WHERE endpoint = ?`, slug); err != nil {
		return err
	}
	_, err := s.exec(`DELETE FROM webhook_endpoints WHERE id = ?`, id)
	return err
}

// RecordWebhookDelivery notes what arrived.
//
// Rejections are recorded too, and are the more useful half: "nothing is
// happening" and "everything is arriving with a bad signature" look identical
// from the operator's side otherwise.
func (s *Store) RecordWebhookDelivery(d WebhookDelivery) error {
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	// A body sample, not the body. Payloads carry customer data and this table
	// is read in a browser; enough to recognise a delivery is enough.
	d.BodySample = truncateRunes(d.BodySample, 500)
	d.Detail = truncateRunes(d.Detail, 500)

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.exec(`
INSERT INTO webhook_deliveries (id, endpoint, source, status, detail, body_sample, received_at)
VALUES (?, ?, ?, ?, ?, ?, datetime('now'))`,
		d.ID, d.Endpoint, d.Source, d.Status, d.Detail, d.BodySample)
	return err
}

// WebhookDeliveries returns the most recent deliveries, newest first. An empty
// endpoint returns deliveries to everything.
func (s *Store) WebhookDeliveries(endpoint string, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := `SELECT id, endpoint, source, status, detail, body_sample, received_at FROM webhook_deliveries`
	args := []any{}
	if endpoint != "" {
		q += ` WHERE endpoint = ?`
		args = append(args, endpoint)
	}
	q += ` ORDER BY received_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(&d.ID, &d.Endpoint, &d.Source, &d.Status, &d.Detail,
			&d.BodySample, &d.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneWebhookDeliveries keeps the history from growing without bound.
func (s *Store) PruneWebhookDeliveries(keepPerEndpoint int) (int64, error) {
	if keepPerEndpoint <= 0 {
		keepPerEndpoint = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.exec(`
DELETE FROM webhook_deliveries WHERE id NOT IN (
  SELECT id FROM webhook_deliveries d
  WHERE (SELECT COUNT(*) FROM webhook_deliveries n
         WHERE n.endpoint = d.endpoint AND n.received_at > d.received_at) < ?
)`, keepPerEndpoint)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
