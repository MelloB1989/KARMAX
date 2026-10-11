package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/MelloB1989/karmax/fleet/internal/console"
)

func fakeConsole(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ctok" {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FLEET_CONSOLE_URL", srv.URL)
	t.Setenv("FLEET_CONSOLE_TOKEN", "ctok")
}

func capture(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := run(args[0], args[1:])
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out), err
}

func TestConsoleSnapshotPrintsATable(t *testing.T) {
	at := time.Date(2026, 10, 11, 14, 0, 10, 0, time.UTC)
	snap := console.Snapshot{At: at,
		Sources: map[string]console.Source{
			"host":   {OK: true, At: at.Add(-2 * time.Second)},
			"fleetd": {OK: false, At: at.Add(-3 * time.Minute), Error: "status: 503"}},
		Agents: []console.Agent{
			{ID: "agent-01", Live: true, Phase: "working", ContextPct: 8, FiveHourPct: 3, SevenDayPct: 11,
				CurrentTool: &console.Tool{Name: "Bash", Since: at.Add(-42 * time.Second)},
				Container:   &console.Container{Running: true, CPUPct: 12.4, Mem: 300 << 20}},
			{ID: "agent-02", Phase: "standby"}}}
	fakeConsole(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/snapshot" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(snap)
	})
	out, err := capture(t, "console", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AGENT", "PHASE", "agent-01", "working", "Bash 42s", "12", "300M", "agent-02", "remote",
		"SOURCE", "fleetd", "DOWN", "3m", "status: 503", "host", "2s"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	js, err := capture(t, "console", "snapshot", "--json")
	var back console.Snapshot
	if err != nil || json.Unmarshal([]byte(js), &back) != nil || len(back.Agents) != 2 {
		t.Fatalf("--json: %v %s", err, js)
	}
}

func TestConsoleHealth(t *testing.T) {
	fakeConsole(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"sources":{"host":{"ok":true,"at":"2026-10-11T14:00:00Z"}},"version":"v9"}`)
	})
	out, err := capture(t, "console", "health")
	if err != nil || !strings.Contains(out, "v9") || !strings.Contains(out, "host") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestConsoleWatchPrintsNDJSON(t *testing.T) {
	fakeConsole(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		c.Write(ctx, websocket.MessageText, []byte("{\n  \"type\": \"snapshot\",\n  \"seq\": 1\n}"))
		c.Write(ctx, websocket.MessageText, []byte(`{"type":"patch","seq":2,"ops":[{"op":"replace","path":"/at","value":"x"}]}`))
		c.Close(websocket.StatusNormalClosure, "")
	})
	out, err := capture(t, "console", "watch", "--json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	for i, l := range lines {
		var m console.Message
		if err := json.Unmarshal([]byte(l), &m); err != nil || m.Seq != uint64(i+1) {
			t.Fatalf("line %d: %v %q", i, err, l)
		}
	}
}

func TestConsoleTermWritesRawBytes(t *testing.T) {
	fakeConsole(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/term/agent-01" || r.URL.Query().Get("control") != "" {
			http.NotFound(w, r)
			return
		}
		c, _ := websocket.Accept(w, r, nil)
		c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello"}`))
		c.Write(r.Context(), websocket.MessageBinary, []byte("\x1b[2Jhello"))
		c.Close(websocket.StatusNormalClosure, "")
	})
	out, err := capture(t, "console", "term", "agent-01")
	if err != nil || out != "\x1b[2Jhello" {
		t.Fatalf("%v %q", err, out)
	}
}

func TestAttachReportsWhoHoldsControl(t *testing.T) {
	fakeConsole(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("control") != "1" {
			t.Errorf("attach must ask for control: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		io.WriteString(w, `{"error":"held by alice","holder":"alice"}`)
	})
	_, err := capture(t, "attach", "agent-01")
	if err == nil || err.Error() != "held by alice" {
		t.Fatalf("err = %v, want %q", err, "held by alice")
	}
}

func TestConsoleRefusesWithoutAToken(t *testing.T) {
	t.Setenv("FLEET_CONSOLE_URL", "http://127.0.0.1:1")
	t.Setenv("FLEET_CONSOLE_TOKEN", "")
	if _, err := capture(t, "console", "health"); err == nil || !strings.Contains(err.Error(), "FLEET_CONSOLE_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}
