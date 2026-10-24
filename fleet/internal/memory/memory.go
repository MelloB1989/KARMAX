// Package memory reads the operator's long-term memory: the gitloom
// namespace the orchestrator keeps. fleetd holds the key and only ever reads
// with it — a gitloom key cannot be made read-only, so the client refuses
// to send it anywhere but the read routes (readOnly), and nothing it returns
// or fails with carries it. Saving is the orchestrator's: everyone else
// suggests (daemon.Suggest).
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client searches one gitloom namespace.
type Client struct {
	URL       string // gitloom, e.g. http://127.0.0.1:8787
	Key       string // its Bearer key
	Namespace string
	// HTTP, when set, is the transport under readOnly; tests replace it.
	HTTP http.RoundTripper
}

// Memory is one memory a search found.
type Memory struct {
	Path    string    `json:"path"`
	Title   string    `json:"title,omitempty"`
	Topic   string    `json:"topic,omitempty"`
	Tier    string    `json:"tier,omitempty"`
	Content string    `json:"content"`
	Score   float64   `json:"score,omitempty"`
	Updated time.Time `json:"updated_at,omitzero"`
}

// Result is a search's answer.
type Result struct {
	Namespace string   `json:"namespace"`
	Query     string   `json:"query"`
	Memories  []Memory `json:"memories"`
}

// DefaultLimit and MaxLimit bound how many memories one search returns.
const DefaultLimit, MaxLimit = 5, 20

// Recall searches the namespace (GET /v1/retrieve).
func (c *Client) Recall(ctx context.Context, query string, limit int) (*Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("memory: recall needs a query")
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	q := url.Values{"namespace": {c.Namespace}, "q": {query}, "limit": {strconv.Itoa(limit)}}
	var res Result
	if err := c.get(ctx, "/v1/retrieve?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	if res.Memories == nil {
		res.Memories = []Memory{}
	}
	res.Query, res.Namespace = query, c.Namespace
	return &res, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+path, nil)
	if err != nil {
		return c.scrub(fmt.Errorf("memory: %w", err))
	}
	next := c.HTTP
	if next == nil {
		next = http.DefaultTransport
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: readOnly{key: c.Key, next: next}}).Do(req)
	if err != nil {
		return c.scrub(fmt.Errorf("memory: gitloom unreachable: %w", err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return c.scrub(fmt.Errorf("memory: reading gitloom's answer: %w", err))
	}
	// Whatever gitloom says goes to agents: not the key, wherever it is.
	raw = bytes.ReplaceAll(raw, []byte(c.Key), []byte(redacted))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		msg := resp.Status
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = fmt.Sprintf("%d %s", resp.StatusCode, e.Error.Message)
		}
		return c.scrub(fmt.Errorf("memory: gitloom said %s", msg))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("memory: gitloom's answer is not JSON (%d bytes)", len(raw))
	}
	return nil
}

const redacted = "[redacted]"

// scrub takes the key out of an error, wherever a library put it.
func (c *Client) scrub(err error) error {
	if c.Key == "" || !strings.Contains(err.Error(), c.Key) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.Key, redacted))
}

// readRoutes are the only requests the key goes out with.
var readRoutes = map[string]bool{"/v1/retrieve": true, "/v1/memories": true}

// readOnly sets the key on a GET of a read route and refuses every other
// request: whatever calls the client, fleetd never writes to gitloom.
type readOnly struct {
	key  string
	next http.RoundTripper
}

func (t readOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || !readRoutes[r.URL.Path] {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, fmt.Errorf("memory: %s %s is not a read route; fleetd only reads gitloom", r.Method, r.URL.Path)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.key)
	return t.next.RoundTrip(r)
}
