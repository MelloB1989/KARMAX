package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func noSleep(time.Duration) {}

// fleetd answers: the message goes, nothing is queued.
func TestTellSendsWhenFleetdAnswers(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	box := t.TempDir()
	said, err := tell(&client{base: srv.URL, token: "t"}, "agent-03", "operator", "DONE", box, noSleep)
	if err != nil || said != "ok" || got.Load() != 1 {
		t.Fatalf("said %q err %v calls %d", said, err, got.Load())
	}
	if ents, _ := os.ReadDir(box); len(ents) != 0 {
		t.Fatalf("outbox: %v", ents)
	}
}

// fleetd is restarting, or Kali is rebooting: a report is not lost. After a
// few tries it goes into the outbox, where fleetd picks it up.
func TestTellQueuesInTheOutboxWhenFleetdIsAway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nobody listening
	box := filepath.Join(t.TempDir(), "outbox")
	before := time.Now()
	said, err := tell(&client{base: url, token: "t"}, "agent-03", "karmax", "DONE T1: branch x", box, noSleep)
	if err != nil || said != queuedLocally {
		t.Fatalf("said %q err %v", said, err)
	}
	ents, _ := os.ReadDir(box)
	if len(ents) != 1 || !strings.HasSuffix(ents[0].Name(), ".json") {
		t.Fatalf("outbox: %v", ents)
	}
	b, _ := os.ReadFile(filepath.Join(box, ents[0].Name()))
	var m struct {
		From, To, Text string
		At             time.Time
	}
	if err := json.Unmarshal(b, &m); err != nil || m.From != "agent-03" || m.To != "karmax" || m.Text != "DONE T1: branch x" || m.At.Before(before.Add(-time.Second)) {
		t.Fatalf("queued %s (%v)", b, err)
	}
	if strings.Contains(string(b), "\n") {
		t.Fatalf("one line, for fleetd's snapshot: %q", b)
	}
}

// An answer fleetd gave (no such agent; a refused token) is not queued: it
// would never go.
func TestTellDoesNotQueueWhatFleetdRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"no agent named \"agent-99\""}`))
	}))
	defer srv.Close()
	box := t.TempDir()
	said, err := tell(&client{base: srv.URL, token: "t"}, "agent-03", "agent-99", "hi", box, noSleep)
	if err == nil || said != "" {
		t.Fatalf("said %q err %v", said, err)
	}
	if ents, _ := os.ReadDir(box); len(ents) != 0 {
		t.Fatalf("outbox: %v", ents)
	}
}

// Outside a fleet container (the operator's machine) there is no outbox: the
// error is the caller's.
func TestTellWithoutAnOutboxFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if said, err := tell(&client{base: url, token: "t"}, "operator", "agent-03", "hi", "", noSleep); err == nil || said != "" {
		t.Fatalf("said %q err %v", said, err)
	}
}

// fleetd took the message but holds it for a receiver out of reach: that is
// said, once, and nothing is queued here — fleetd has it.
func TestTellSaysWhenFleetdHoldsTheMessage(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true,"queued":true,"why":"queued: agent-05 cannot take it now"}`))
	}))
	defer srv.Close()
	box := t.TempDir()
	said, err := tell(&client{base: srv.URL, token: "t"}, "agent-03", "agent-05", "hi", box, noSleep)
	if err != nil || said != "queued: agent-05 cannot take it now" || got.Load() != 1 {
		t.Fatalf("said %q err %v calls %d", said, err, got.Load())
	}
	if ents, _ := os.ReadDir(box); len(ents) != 0 {
		t.Fatalf("outbox: %v", ents)
	}
}

func TestASlotQueuesInItsOwnOutbox(t *testing.T) {
	t.Setenv("FLEET_AGENT", "agent-03:2")
	t.Setenv("FLEET_WORK", "/w")
	t.Setenv("FLEET_STATE", "/w/.fleet/slot-2")
	if got := outboxDir(); got != "/w/.fleet/slot-2/outbox" {
		t.Errorf("slot 2's outbox = %q", got)
	}
	t.Setenv("FLEET_STATE", "")
	if got := outboxDir(); got != "/w/.fleet/outbox" {
		t.Errorf("without FLEET_STATE = %q", got)
	}
}
