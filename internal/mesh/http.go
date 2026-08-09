package mesh

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// The inbound surface. Two routes, deliberately small: everything a hostile
// instance can reach is here, so it is worth being able to read in one screen.
//
//	GET  /mesh/hello  — who this instance is (public keys only)
//	POST /mesh        — one envelope
//
// Note what is NOT here: no unauthenticated way to list peers, enumerate
// messages, or learn anything about the operator. An attacker who finds the
// endpoint learns this instance's public identity and nothing else.

// maxEnvelopeBytes caps a request body before any parsing. Applied at the
// reader, not after: checking Content-Length trusts the sender about its own
// size.
const maxEnvelopeBytes = 512 << 10

// Handler serves the mesh routes.
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	limiter := newRateLimiter(60, time.Minute)

	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET required", http.StatusMethodNotAllowed)
			return
		}
		if !limiter.allow(clientKey(r)) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		writeJSON(w, http.StatusOK, n.Public())
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		// Rate limited before the crypto: signature verification is the
		// expensive part, so letting an unauthenticated flood reach it is a
		// free way to burn this instance's CPU.
		if !limiter.allow(clientKey(r)) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}

		var e Envelope
		body := io.LimitReader(r.Body, maxEnvelopeBytes)
		if err := json.NewDecoder(body).Decode(&e); err != nil {
			http.Error(w, "malformed envelope", http.StatusBadRequest)
			return
		}

		if err := n.Receive(r.Context(), &e); err != nil {
			// One status and one generic message for every refusal. Telling a
			// caller WHY it was refused distinguishes "unknown instance" from
			// "blocked" from "bad signature", which is a map of this
			// instance's trust state handed to whoever asks for it.
			n.log.Warn("mesh: refused an envelope",
				zap.String("from", short(e.From)), zap.String("kind", string(e.Kind)),
				zap.Error(err))
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "refused"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
	})

	// StripPrefix turns "/mesh" into "" and "/mesh/hello" into "/hello". An
	// empty path makes ServeMux answer 307 to "/" — and because the transport
	// deliberately does not follow redirects (a peer that can redirect us can
	// aim our authenticated POST at a third party), that 307 would break every
	// delivery. Normalising here keeps both properties.
	return http.StripPrefix("/mesh", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" {
			r2 := *r
			u := *r.URL
			u.Path = "/"
			r2.URL = &u
			mux.ServeHTTP(w, &r2)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// rateLimiter is a fixed-window counter per client.
//
// Crude on purpose: the job is to stop a flood from reaching the crypto, not to
// shape traffic. A token bucket would be nicer and is not worth the code here.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string]*window
}

type window struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, per time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: per, seen: map[string]*window{}}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if len(l.seen) > 8192 {
		for k, w := range l.seen {
			if now.Sub(w.start) > l.window {
				delete(l.seen, k)
			}
		}
	}
	w, ok := l.seen[key]
	if !ok || now.Sub(w.start) > l.window {
		l.seen[key] = &window{count: 1, start: now}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// clientKey identifies a caller for rate limiting.
//
// The socket address, NOT a header. X-Forwarded-For is set by the client on a
// direct connection, so limiting on it lets an attacker reset their own budget
// by changing one header.
func clientKey(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// short renders a key for logs. Full keys in logs are noise, and logs travel.
func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}
