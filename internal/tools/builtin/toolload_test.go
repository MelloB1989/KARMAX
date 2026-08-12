package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

func loader() *LoadToolTool {
	return &LoadToolTool{Available: []string{"google_workspace", "whatsapp.read", "comms.send"}}
}

func TestLoadResolvesKnownTools(t *testing.T) {
	res, err := loader().Execute(context.Background(), map[string]any{
		"names": []any{"google_workspace", "whatsapp.read"},
	})
	if err != nil || res.IsError {
		t.Fatalf("unexpected failure: %v %s", err, res.Error)
	}
	out := res.Output.(map[string]any)
	loaded := out["loaded"].([]string)
	if len(loaded) != 2 {
		t.Fatalf("loaded = %v", loaded)
	}
}

// A model that invents a tool name must be told the real ones rather than
// handed an empty success it will read as "it worked".
func TestUnknownToolFailsWithTheRealNames(t *testing.T) {
	res, _ := loader().Execute(context.Background(), map[string]any{"names": []any{"nonexistent.tool"}})
	if !res.IsError {
		t.Fatal("an unknown tool should be an error")
	}
	if !strings.Contains(res.Error, "google_workspace") {
		t.Errorf("the error should list what is available, got: %s", res.Error)
	}
}

// A partially-valid request still loads what it can, and says what it could not.
func TestPartialLoadReportsBoth(t *testing.T) {
	res, _ := loader().Execute(context.Background(), map[string]any{
		"names": []any{"comms.send", "made.up"},
	})
	if res.IsError {
		t.Fatalf("a partially valid request should succeed: %s", res.Error)
	}
	out := res.Output.(map[string]any)
	if len(out["loaded"].([]string)) != 1 {
		t.Errorf("loaded = %v", out["loaded"])
	}
	if len(out["unknown"].([]string)) != 1 {
		t.Errorf("unknown = %v", out["unknown"])
	}
}

// Models emit array arguments in several shapes; all of them must work.
func TestArgumentShapes(t *testing.T) {
	for _, in := range []map[string]any{
		{"names": []any{"comms.send"}},
		{"names": []string{"comms.send"}},
		{"names": "comms.send"},
		{"names": `["comms.send"]`},
		{"name": "comms.send"}, // singular, a common model slip
	} {
		res, err := loader().Execute(context.Background(), in)
		if err != nil || res.IsError {
			t.Errorf("input %#v failed: %v %s", in, err, res.Error)
		}
	}
}

func TestEmptyRequestIsRejected(t *testing.T) {
	res, _ := loader().Execute(context.Background(), map[string]any{"names": []any{}})
	if !res.IsError {
		t.Error("an empty request should be an error")
	}
}

// Canonical (underscored) names must resolve to the same tool as dotted ones.
func TestCanonicalNamesResolve(t *testing.T) {
	res, _ := loader().Execute(context.Background(), map[string]any{
		"names": []any{tools.CanonicalName("whatsapp.read")},
	})
	if res.IsError {
		t.Errorf("canonical name did not resolve: %s", res.Error)
	}
}
