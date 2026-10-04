package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPlaceBrainCallBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calls" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := placeBrainCall(context.Background(), srv.URL, "j@x", "why", "ws://127.0.0.1:1/voice"); err != nil {
		t.Fatal(err)
	}
	if got["to"] != "j@x" || got["brain"] != true || got["brief"] != "why" || got["brain_url"] != "ws://127.0.0.1:1/voice" {
		t.Fatalf("body = %v", got)
	}
}
