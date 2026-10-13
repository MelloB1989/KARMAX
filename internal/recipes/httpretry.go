package recipes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// maxRetryWait caps any single wait between attempts, whether it came from
// exponential backoff or from a Retry-After header.
const maxRetryWait = 60 * time.Second

// defaultRetryBackoff is the first wait when retry: gives attempts only.
const defaultRetryBackoff = time.Second

// retrySleep waits d or until ctx ends. A variable so tests do not wait.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryPolicy is an http step's retry: block. Attempts is the TOTAL number of
// tries including the first; 1 means no retry.
type retryPolicy struct {
	Attempts int
	Backoff  time.Duration
}

func retryPolicyFor(s Step, b Bindings) (retryPolicy, error) {
	p := retryPolicy{Attempts: 1, Backoff: defaultRetryBackoff}
	n, err := countArg("retry.attempts", s.Args["retry.attempts"], b)
	if err != nil {
		return p, err
	}
	if n > MaxRetryAttempts {
		return p, fmt.Errorf("retry.attempts %d is more than the %d allowed", n, MaxRetryAttempts)
	}
	if n > 0 {
		p.Attempts = n
	}
	d, err := renderDuration("retry.backoff", s.Args["retry.backoff"], b)
	if err != nil {
		return p, err
	}
	if d < 0 {
		return p, fmt.Errorf("retry.backoff must be positive")
	}
	if d > 0 {
		p.Backoff = d
	}
	return p, nil
}

// headerHTTP is implemented by kits that can also hand back response headers,
// which is how Retry-After is honoured. Kits that cannot still retry; they
// just back off exponentially.
type headerHTTP interface {
	HTTPWithHeaders(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, http.Header, error)
}

// doHTTP makes the request, retrying per policy on network errors, 429 and 5xx
// only. Any other status — including every other 4xx — is final.
//
// The request is repeated verbatim, so with a non-idempotent method the
// recipe author is asserting the endpoint tolerates it.
func doHTTP(ctx context.Context, k loopkit.Kit, p retryPolicy, method, url string, headers map[string]string, body string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		var (
			out    string
			status int
			hdr    http.Header
			err    error
		)
		if hk, ok := k.(headerHTTP); ok {
			out, status, hdr, err = hk.HTTPWithHeaders(ctx, method, url, headers, body)
		} else {
			out, status, err = k.HTTP(ctx, method, url, headers, body)
		}

		var retryAfter time.Duration
		switch {
		case err != nil:
			lastErr = err
			if !retryableNetErr(ctx, err) {
				return "", err
			}
		case status >= 200 && status <= 299:
			return out, nil
		default:
			lastErr = fmt.Errorf("%s %s answered %d", method, url, status)
			if status != http.StatusTooManyRequests && status < 500 {
				return "", lastErr
			}
			retryAfter = parseRetryAfter(hdr.Get("Retry-After"), time.Now())
		}

		if attempt == p.Attempts {
			break
		}
		wait := p.Backoff << (attempt - 1)
		if wait <= 0 || wait > maxRetryWait {
			wait = maxRetryWait
		}
		if retryAfter > 0 {
			wait = retryAfter
			if wait > maxRetryWait {
				wait = maxRetryWait
			}
		}
		// No point sleeping past the run's own deadline: give up with the
		// real error rather than a context error that hides it.
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= wait {
			break
		}
		k.Logf("http: %v; retrying in %s (attempt %d of %d)", lastErr, wait, attempt+1, p.Attempts)
		if err := retrySleep(ctx, wait); err != nil {
			break
		}
	}
	if p.Attempts > 1 {
		return "", fmt.Errorf("%w (after %d attempts)", lastErr, p.Attempts)
	}
	return "", lastErr
}

// retryableNetErr is true for a transport failure (refused, reset, timeout,
// DNS) and false for a request the host refused to make — a blocked URL or a
// missing grant — and for the run's own cancellation.
func retryableNetErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// parseRetryAfter reads a Retry-After header: delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
