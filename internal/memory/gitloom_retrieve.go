package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Recall, spoken to the retrieve API directly.
//
// On 2026-09-15 GitLoom's retrieve endpoint started answering with
// `memories` — whole memories with their content and a calibrated score — in
// place of `hits`. The SDK KARMAX is pinned to reads `hits`: it decoded every
// response as empty and reported no error, so from that day every recall in
// KARMAX came back with nothing while the service was answering correctly.
// Calls said "nothing in memory" about things memory plainly held; the agent,
// the loops and memory.retrieve all worked from an empty memory.
//
// The SDK release that reads `memories` (v0.4.0) dropped the write API this
// backend also needs — Write, Get, Forget and Tree live only on an unmerged
// SDK branch — so no single SDK version serves both. Recall is therefore
// decoded here, against the server's own schema, and everything else stays on
// the pinned client, whose endpoints did not change.

const defaultGitLoomBaseURL = "https://api.gitloom.cloud"

// retrieveResponse is the part of /v1/retrieve this backend reads.
type retrieveResponse struct {
	Namespace   string              `json:"namespace"`
	Memories    []retrievedMemory   `json:"memories"`
	Defined     []retrievedVocabHit `json:"defined"`
	Candidates  int                 `json:"candidates"`
	FilteredOut int                 `json:"filtered_out"`
	Millis      int64               `json:"millis"`
	// Hits is the pre-2026-09-15 shape, read only to report a server that
	// has gone back to it rather than decode it as empty.
	Hits json.RawMessage `json:"hits"`
}

type retrievedMemory struct {
	Path    string              `json:"path"`
	Title   string              `json:"title"`
	Content string              `json:"content"`
	Snippet string              `json:"snippet"`
	Score   float64             `json:"score"`
	Created string              `json:"created"`
	Related []retrievedRelation `json:"related"`
}

type retrievedRelation struct {
	Label   string `json:"label"`
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

type retrievedVocabHit struct {
	Path       string `json:"path"`
	Term       string `json:"term"`
	Definition string `json:"definition"`
}

// retrieve runs one recall against the retrieve API.
func (g *gitloomBackend) retrieve(ctx context.Context, query string, topK int) (*retrieveResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(g.cfg.BaseURL), "/")
	if base == "" {
		base = defaultGitLoomBaseURL
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("namespace", g.cfg.Namespace)
	if topK > 0 {
		q.Set("limit", strconv.Itoa(topK))
	}
	// Provenance is a git-log walk per hit and was the whole cost of the call —
	// 3.29s with it, 0.40s without, measured on this namespace. Relations stay:
	// they are free, and they surface a person's whole cluster in one call.
	q.Set("no_provenance", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/retrieve?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.APIKey)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitloom: retrieve: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("gitloom: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("gitloom: retrieve returned %s: %.300s", resp.Status, body)
	}
	var out retrieveResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gitloom: decode retrieve: %w", err)
	}
	// A response carrying the old field and none of the new one is a server
	// shape change in the other direction. Said loudly: silence is what hid
	// the last one for two weeks.
	if len(out.Memories) == 0 && len(out.Hits) > 2 {
		return nil, fmt.Errorf("gitloom: retrieve answered in the old `hits` shape, which this client no longer reads")
	}
	return &out, nil
}

func (g *gitloomBackend) httpClient() *http.Client {
	return &http.Client{Timeout: g.cfg.Timeout}
}
