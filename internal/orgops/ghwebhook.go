package orgops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"

	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
)

// WebhookPath is where the office takes GitHub deliveries directly.
const WebhookPath = "/orgops/github"

// GitHubWebhookHandler verifies and decodes an org webhook delivery and puts
// each event on the bus under its own kind (github.pr.opened, ...), the same
// kinds the GitHub connector publishes, so the PR loop cannot tell them apart.
func (s *Service) GitHubWebhookHandler() http.HandlerFunc {
	secret := s.cfg.GitHub.WebhookSecret
	if secret == "" && s.cfg.GitHub.WebhookSecretEnv != "" {
		secret = os.Getenv(s.cfg.GitHub.WebhookSecretEnv)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// An unsigned endpoint that merges PRs is an open door; refuse to run without a secret.
		if secret == "" || !validSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		headers := map[string]string{}
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}
		events, err := ghconn.DecodeDelivery(headers, body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n := 0
		for _, e := range events {
			kind, _ := e["kind"].(string)
			if kind == "" {
				continue
			}
			if err := s.publish(kind, e); err != nil {
				http.Error(w, "could not queue", http.StatusServiceUnavailable)
				return
			}
			n++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": n})
	}
}

func validSignature(secret string, body []byte, header string) bool {
	sig := strings.TrimPrefix(header, "sha256=")
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig))
}
