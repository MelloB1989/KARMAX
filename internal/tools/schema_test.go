package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

const incidentParams = `{"type":"object","properties":{"title":{"type":"string"},"detail":{"type":"string"},"service":{"type":"string"},"repo":{"type":"string","description":"org/repo if known"},"severity":{"type":"string","enum":["info","warning","critical"]}},"required":["title"]}`

func manifest(params string) ToolManifest {
	return ToolManifest{Name: "orgops_report_incident", Parameters: json.RawMessage(params)}
}

func TestValidateInputUnknownKeyNamesParams(t *testing.T) {
	err := ValidateInput(manifest(incidentParams), map[string]any{"description": "x"})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`unknown argument "description"`, "title (required, string)", "detail", "repo (org/repo if known)", "severity (info|warning|critical)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestValidateInputMissingRequired(t *testing.T) {
	err := ValidateInput(manifest(incidentParams), map[string]any{"detail": "x"})
	if err == nil || !strings.Contains(err.Error(), `missing required argument "title"`) || !strings.Contains(err.Error(), "takes title") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateInputValid(t *testing.T) {
	if err := ValidateInput(manifest(incidentParams), map[string]any{"title": "t", "severity": "info"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateInputAdditionalPropertiesAndUnchecked(t *testing.T) {
	loose := `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":true}`
	if err := ValidateInput(manifest(loose), map[string]any{"zzz": 1}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{``, `{"type":"object"}`, `{"type":"object","properties":{}}`, `not json`} {
		if err := ValidateInput(manifest(p), map[string]any{"zzz": 1}); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	req := `{"type":"object","properties":{"a":{}},"required":["a"],"additionalProperties":true}`
	if err := ValidateInput(manifest(req), map[string]any{"zzz": 1}); err == nil {
		t.Fatal("required must still be enforced")
	}
}
