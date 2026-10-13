package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
