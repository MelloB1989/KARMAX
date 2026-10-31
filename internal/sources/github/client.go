package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// client is one account's view of the REST API. It holds the account's token
// for one tick and sends it nowhere but the account's own API.
type client struct {
	base  string
	token string
	http  *http.Client
}

// page is one page of notifications.
type page struct {
	items        []Notification
	next         string
	lastModified string
	pollInterval time.Duration
	notModified  bool
}

// RateLimitError is GitHub saying to wait until a time.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "rate limit exhausted until " + e.Reset.UTC().Format(time.RFC3339)
}

// notifications reads one page. ifModifiedSince is only ever sent for the
// first page, which is what makes an unchanged inbox a 304 that costs nothing
// against the rate limit.
func (c client) notifications(ctx context.Context, rawURL, ifModifiedSince string) (page, error) {
	var p page
	resp, err := c.do(ctx, rawURL, func(r *http.Request) {
		if ifModifiedSince != "" {
			r.Header.Set("If-Modified-Since", ifModifiedSince)
		}
	})
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	if v, err := strconv.Atoi(resp.Header.Get("X-Poll-Interval")); err == nil && v > 0 {
		p.pollInterval = time.Duration(v) * time.Second
	}
	p.lastModified = resp.Header.Get("Last-Modified")
	if resp.StatusCode == http.StatusNotModified {
		p.notModified = true
		return p, nil
	}
	if err := statusError(resp); err != nil {
		return p, err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&p.items); err != nil {
		return p, fmt.Errorf("unreadable notifications: %w", err)
	}
	p.next = nextLink(resp.Header.Get("Link"))
	return p, nil
}

// get reads one API resource into out.
func (c client) get(ctx context.Context, rawURL string, out any) error {
	resp, err := c.do(ctx, rawURL, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := statusError(resp); err != nil {
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func (c client) do(ctx context.Context, rawURL string, edit func(*http.Request)) (*http.Response, error) {
	if !c.owns(rawURL) {
		return nil, fmt.Errorf("%s is not this account's API; not sending its token there", hostOf(rawURL))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "karmax-github-source/1")
	if edit != nil {
		edit(req)
	}
	hc := c.http
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error repeats the URL, never a header; the token stays out.
		return nil, err
	}
	return resp, nil
}

var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

// owns reports whether rawURL is under this account's API base. Every URL the
// API hands back is checked, because a payload is not a trusted place to
// learn where to send a token.
func (c client) owns(rawURL string) bool {
	base := strings.TrimSuffix(c.base, "/")
	return rawURL == base || strings.HasPrefix(rawURL, base+"/") || strings.HasPrefix(rawURL, base+"?")
}

func hostOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "that address"
}

// statusError says what a non-2xx answer means for this account, in words an
// operator can act on. Nothing from the request is repeated.
func statusError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &msg)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("GitHub rejected the token (401): log in again with `gh auth login`")
	case (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0":
		reset := time.Now().Add(time.Minute)
		if sec, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset = time.Unix(sec, 0)
		}
		return &RateLimitError{Reset: reset}
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("GitHub refused (403): %s — the token may lack the notifications or repo scope",
			strings.TrimSpace(msg.Message))
	}
	text := strings.TrimSpace(msg.Message)
	if text == "" {
		text = strings.TrimSpace(string(body))
		if len(text) > 200 {
			text = text[:200] + "…"
		}
	}
	return fmt.Errorf("GitHub answered %d: %s", resp.StatusCode, text)
}

// nextLink reads rel="next" from a Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 {
			continue
		}
		for _, attr := range seg[1:] {
			if strings.TrimSpace(attr) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(seg[0]), "<>")
			}
		}
	}
	return ""
}

// Probe checks that one account's notifications can be read — the token is
// accepted and carries the scope. It is the account's health check.
func Probe(ctx context.Context, hc *http.Client, apiBase, token string) error {
	c := client{base: apiBase, token: token, http: hc}
	_, err := c.notifications(ctx, strings.TrimSuffix(apiBase, "/")+"/notifications?per_page=1", "")
	return err
}
