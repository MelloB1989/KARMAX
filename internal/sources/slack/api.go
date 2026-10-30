package slack

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

// apiError is Slack's {"ok":false,"error":...}, which arrives with HTTP 200.
type apiError struct {
	Method, Code, Needed string
}

func (e *apiError) Error() string {
	if e.Needed != "" {
		return fmt.Sprintf("slack %s: %s (needs %s)", e.Method, e.Code, e.Needed)
	}
	return fmt.Sprintf("slack %s: %s", e.Method, e.Code)
}

// rateLimited is HTTP 429: wait, then carry on.
type rateLimited struct{ wait time.Duration }

func (e *rateLimited) Error() string { return fmt.Sprintf("slack rate limit: retry in %s", e.wait) }

// call makes one Web API request with token. The token is never part of an
// error, so errors can be logged and shown as they are.
func (s *Source) call(ctx context.Context, token, method string, form url.Values, out any) error {
	if form == nil {
		form = url.Values{}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(s.API, "/")+"/"+method,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	res, err := s.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		wait := time.Minute
		if n, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && n > 0 {
			wait = time.Duration(n) * time.Second
		}
		return &rateLimited{wait: wait}
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("slack %s: HTTP %d", method, res.StatusCode)
	}
	var env struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack %s: the answer was not JSON", method)
	}
	if !env.OK {
		return &apiError{Method: method, Code: env.Error, Needed: env.Needed}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// tsParts splits a Slack timestamp ("1793440000.000100") into seconds and
// the sequence after the dot.
func tsParts(ts string) (int64, int64) {
	sec, frac, _ := strings.Cut(strings.TrimSpace(ts), ".")
	s, _ := strconv.ParseInt(sec, 10, 64)
	frac = (frac + "000000")[:6]
	f, _ := strconv.ParseInt(frac, 10, 64)
	return s, f
}

// tsAfter reports whether Slack timestamp a is later than b.
func tsAfter(a, b string) bool {
	as, af := tsParts(a)
	bs, bf := tsParts(b)
	return as > bs || (as == bs && af > bf)
}

func tsTime(ts string) time.Time {
	s, f := tsParts(ts)
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, f*1000).UTC()
}

func timeTS(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }

// permalink builds a message's link without a chat.getPermalink call.
func permalink(base, channel, ts, threadTS string) string {
	if base == "" || channel == "" || ts == "" {
		return ""
	}
	u := base + "archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
	if threadTS != "" && threadTS != ts {
		u += "?thread_ts=" + threadTS + "&cid=" + channel
	}
	return u
}
