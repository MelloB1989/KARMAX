package slack

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

const testSigningSecret = "shh-its-a-secret"

// sign reproduces Slack's v0 signing scheme, so tests can build a header a
// real Slack request would send without hitting the network.
func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func signedHeader(secret string, ts time.Time, body []byte) http.Header {
	stamp := strconv.FormatInt(ts.Unix(), 10)
	h := http.Header{}
	h.Set("X-Slack-Request-Timestamp", stamp)
	h.Set("X-Slack-Signature", sign(secret, stamp, body))
	return h
}

// A correctly signed, fresh request is accepted.
func TestVerifyRequestAcceptsAGoodSignature(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	h := signedHeader(testSigningSecret, time.Now(), body)
	if err := verifyRequest(h, body, testSigningSecret); err != nil {
		t.Fatalf("verifyRequest() = %v, want nil", err)
	}
}

// A bad signature is REJECTED, not logged-and-processed.
func TestVerifyRequestRejectsABadSignature(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	h := signedHeader("a-completely-different-secret", time.Now(), body)
	if err := verifyRequest(h, body, testSigningSecret); err == nil {
		t.Fatal("verifyRequest() = nil, want a rejection for a bad signature")
	}
}

// A tampered body (correct headers, different bytes) is REJECTED the same way
// — the signature covers the body, not just the timestamp.
func TestVerifyRequestRejectsATamperedBody(t *testing.T) {
	original := []byte(`{"type":"event_callback"}`)
	h := signedHeader(testSigningSecret, time.Now(), original)
	tampered := []byte(`{"type":"evil_callback"}`)
	if err := verifyRequest(h, tampered, testSigningSecret); err == nil {
		t.Fatal("verifyRequest() = nil, want a rejection for a tampered body")
	}
}

// A signature that is otherwise valid but six minutes old is REJECTED —
// Slack's replay window is five minutes.
func TestVerifyRequestRejectsAStaleTimestamp(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	h := signedHeader(testSigningSecret, time.Now().Add(-6*time.Minute), body)
	if err := verifyRequest(h, body, testSigningSecret); err == nil {
		t.Fatal("verifyRequest() = nil, want a rejection for a stale timestamp")
	}
}

// No signing secret configured fails closed: everything is rejected, rather
// than every HTTP request being waved through unverified.
func TestVerifyRequestRejectsWhenNoSecretConfigured(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	h := signedHeader(testSigningSecret, time.Now(), body)
	if err := verifyRequest(h, body, ""); err == nil {
		t.Fatal("verifyRequest() = nil, want a rejection when no secret is configured")
	}
}

// Missing headers altogether (no signature attempted) is REJECTED, not
// treated as an unsigned-but-trusted request.
func TestVerifyRequestRejectsMissingHeaders(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	if err := verifyRequest(http.Header{}, body, testSigningSecret); err == nil {
		t.Fatal("verifyRequest() = nil, want a rejection for missing headers")
	}
}
