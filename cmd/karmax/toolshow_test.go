package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A wrong memory can only be corrected by its id, so search can show it.
func TestMemorySearchCanShowIDs(t *testing.T) {
	e := map[string]any{"id": "facts/people/omar.md#role", "content": "Omar is the CFO\nat Contoso."}
	if got := formatMemoryHit(e, false); got != "• Omar is the CFO at Contoso." {
		t.Errorf("plain hit = %q", got)
	}
	if got := formatMemoryHit(e, true); got != "• [facts/people/omar.md#role] Omar is the CFO at Contoso." {
		t.Errorf("hit with id = %q", got)
	}
	long := map[string]any{"id": "a.md", "content": strings.Repeat("x", 500)}
	if got := formatMemoryHitFull(long); !strings.Contains(got, strings.Repeat("x", 500)) || !strings.HasPrefix(got, "• [a.md]\n") {
		t.Errorf("full hit truncated or unlabelled: %q", got)
	}
}

func TestFormatToolShow(t *testing.T) {
	out := formatToolShow("orgops_report_incident", "Report a problem.",
		json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"},"severity":{"type":"string","enum":["info","warning","critical"],"description":"how bad"}},"required":["title"]}`))
	for _, want := range []string{"orgops_report_incident", "title  string, required", "severity  string, optional, one of: info|warning|critical — how bad"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(formatToolShow("x", "d", nil), "none declared") {
		t.Error("tool without schema should say so")
	}
}
