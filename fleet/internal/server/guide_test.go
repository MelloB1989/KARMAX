package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The routes table in docs/FLEET-CONSOLE.md is the API index (GET /v1, `fleetctl api`) written out:
// this fails when a route or its scopes change and the guide does not.
// FLEET_PRINT_ROUTES=1 prints the index's rows, to paste into the guide.
func TestGuideRoutesMatchIndex(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", "/v1", "full", "")
	var idx []Route
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("FLEET_PRINT_ROUTES") != "" {
		for _, r := range idx {
			fmt.Printf("%s|%s\n", r.Route, strings.Join(r.Scopes, ","))
		}
		return
	}
	doc, err := os.ReadFile("../../../docs/FLEET-CONSOLE.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	a, b := strings.Index(s, "<!-- routes:begin -->"), strings.Index(s, "<!-- routes:end -->")
	if a < 0 || b < a {
		t.Fatal("docs/FLEET-CONSOLE.md has no routes:begin / routes:end markers")
	}
	have := map[string]string{}
	for _, line := range strings.Split(s[a:b], "\n") {
		// | `GET /v1/status` | full, relay, console | what it does |
		cells := strings.Split(line, "|")
		if len(cells) < 4 || !strings.HasPrefix(strings.TrimSpace(cells[1]), "`") {
			continue
		}
		route := strings.Trim(strings.TrimSpace(cells[1]), "`")
		scopes := strings.ReplaceAll(strings.TrimSpace(cells[2]), " ", "")
		have[route] = scopes
	}
	want := map[string]string{}
	for _, r := range idx {
		want[r.Route] = strings.Join(r.Scopes, ",")
		got, ok := have[r.Route]
		if !ok {
			t.Errorf("the guide lacks route %s (%s)", r.Route, want[r.Route])
		} else if got != want[r.Route] {
			t.Errorf("route %s: the guide says scopes %q, the API says %q", r.Route, got, want[r.Route])
		}
	}
	for r := range have {
		if _, ok := want[r]; !ok {
			t.Errorf("the guide lists %s, which the API does not serve", r)
		}
	}
}
