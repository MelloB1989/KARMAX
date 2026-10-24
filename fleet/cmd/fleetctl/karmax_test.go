package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCallArgumentsAreStringsUnlessGivenAsJSON(t *testing.T) {
	got, err := toolArgs(`{"to":"x","n":1}`, []string{"to=+15555550100", "n:=5", "urgent:=true", "body=a=b"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"body":"a=b","n":5,"to":"+15555550100","urgent":true}` {
		t.Fatalf("args = %s", b)
	}
	for _, bad := range []string{"novalue", "=x", "n:=nope"} {
		if _, err := toolArgs("", []string{bad}); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

func TestAWriteCallWaitsOnItsApprovalThroughARestart(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	waits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/v1/karmax/call":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["reason"] != "reply to Ram" || in["from"] != "agent-03" || in["timeout"] != "30m0s" {
				t.Errorf("call body = %v", in)
			}
			_, _ = w.Write([]byte(`{"tool":"google.mail.send","class":"write","approval":{"id":7,"state":"pending","expires":"2026-10-24T12:30:00Z"}}`))
		case r.URL.Path == "/v1/approvals/7":
			waits++
			switch waits {
			case 1: // the wait ran out: still pending
				_, _ = w.Write([]byte(`{"tool":"google.mail.send","class":"write","approval":{"id":7,"state":"pending"}}`))
			case 2: // fleetd restarting
				w.WriteHeader(http.StatusBadGateway)
			default:
				_, _ = w.Write([]byte(`{"tool":"google.mail.send","class":"write","result":{"tool":"google.mail.send","ok":true,"output":{"id":"m1"}}}`))
			}
		}
	}))
	defer srv.Close()
	var errOut strings.Builder
	rep, err := karmaxCall(context.Background(), &client{base: srv.URL, token: "relay"}, "agent-03", "google.mail.send",
		map[string]any{"to": "ram@example.com"}, "reply to Ram", 30*time.Minute, &errOut, func(context.Context, time.Duration) error { return nil })
	if err != nil || rep.Result == nil || !rep.Result.OK {
		t.Fatalf("call = %+v, %v", rep, err)
	}
	if !strings.Contains(errOut.String(), "approval 7 is with the orchestrator") || !strings.Contains(errOut.String(), "retrying") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if waits != 3 || !strings.Contains(paths[1], "/v1/approvals/7?wait=5m&from=agent-03") {
		t.Fatalf("requests = %q", paths)
	}
}

func TestADeniedWriteCallFailsWithWhy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/karmax/call" {
			_, _ = w.Write([]byte(`{"tool":"google.mail.send","class":"write","approval":{"id":8,"state":"pending"}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"refused: karmax denied approval 8: the operator said no"}`))
	}))
	defer srv.Close()
	_, err := karmaxCall(context.Background(), &client{base: srv.URL, token: "relay"}, "agent-03", "google.mail.send",
		nil, "x", time.Minute, &strings.Builder{}, func(context.Context, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "the operator said no") {
		t.Fatalf("err = %v", err)
	}
}
