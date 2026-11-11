package quotaprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadsTheWindowsFromTheHeaders(t *testing.T) {
	var auth, beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, beta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
		h := w.Header()
		h.Set("anthropic-ratelimit-unified-5h-utilization", "0.07")
		h.Set("anthropic-ratelimit-unified-5h-reset", "1794413400")
		h.Set("anthropic-ratelimit-unified-7d-utilization", "0.77")
		h.Set("anthropic-ratelimit-unified-7d-reset", "1794517200")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	got, err := Prober{URL: srv.URL}.Read(context.Background(), "tok-fake")
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour < 6.9 || got.FiveHour > 7.1 || got.SevenDay < 76.9 || got.SevenDay > 77.1 ||
		!got.SevenResets.Equal(time.Unix(1794517200, 0)) || !got.FiveResets.Equal(time.Unix(1794413400, 0)) {
		t.Errorf("reading %+v", got)
	}
	if auth != "Bearer tok-fake" || !strings.Contains(beta, "oauth") {
		t.Errorf("auth %q beta %q", auth, beta)
	}
}

func TestAnAnswerWithoutLimitsIsAnErrorThatNamesNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token tok-fake"}`))
	}))
	defer srv.Close()
	_, err := Prober{URL: srv.URL}.Read(context.Background(), "tok-fake")
	if err == nil || strings.Contains(err.Error(), "tok-fake") || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}
