// Package quotaprobe reads an account's live 5-hour and 7-day windows. A
// status line has them only as of a session's last turn, so an idle agent's
// are old; the API tells them on every answer, so one token of Haiku asked
// of the account's own token (the cheapest call there is) reads them now.
package quotaprobe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Reading is an account's windows at a moment.
type Reading struct {
	FiveHour, SevenDay      float64   // used, percent
	FiveResets, SevenResets time.Time // zero when no window is open
}

// Prober asks the API.
type Prober struct {
	URL    string       // default https://api.anthropic.com/v1/messages
	Model  string       // default claude-haiku-4-5
	Client *http.Client // default one with a 20s timeout
}

const body = `{"model":%q,"max_tokens":1,"system":"You are Claude Code, Anthropic's official CLI for Claude.","messages":[{"role":"user","content":"."}]}`

// Read is the account's windows, by its OAuth token. Neither the token nor
// the answer's body is ever in an error.
func (p Prober) Read(ctx context.Context, token string) (Reading, error) {
	url, model, cl := p.URL, p.Model, p.Client
	if url == "" {
		url = "https://api.anthropic.com/v1/messages"
	}
	if model == "" {
		model = "claude-haiku-4-5"
	}
	if cl == nil {
		cl = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(fmt.Sprintf(body, model)))
	if err != nil {
		return Reading{}, errors.New("bad request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return Reading{}, errors.New("unreachable")
	}
	resp.Body.Close()
	return parse(resp.Header, resp.StatusCode)
}

// parse reads the unified rate-limit headers; a 429 carries them too.
func parse(h http.Header, status int) (Reading, error) {
	var r Reading
	var got int
	pct := func(k string) float64 {
		v, err := strconv.ParseFloat(h.Get("anthropic-ratelimit-unified-"+k+"-utilization"), 64)
		if err != nil {
			return 0
		}
		got++
		return v * 100
	}
	at := func(k string) time.Time {
		n, err := strconv.ParseInt(h.Get("anthropic-ratelimit-unified-"+k+"-reset"), 10, 64)
		if err != nil || n <= 0 {
			return time.Time{}
		}
		return time.Unix(n, 0)
	}
	r.FiveHour, r.SevenDay = pct("5h"), pct("7d")
	r.FiveResets, r.SevenResets = at("5h"), at("7d")
	if got == 0 {
		return Reading{}, fmt.Errorf("no rate limits in the answer (HTTP %d)", status)
	}
	return r, nil
}
