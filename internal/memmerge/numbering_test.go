package memmerge

import (
	"encoding/json"
	"testing"
)

// The model was asked to echo store ids back. With a local table those were
// UUIDs and it abbreviated them; with GitLoom they are paths and it invented
// hex strings — eleven clusters proposed, none matched, and the pass reported
// success having changed nothing. Numbers are what a model can carry without
// transcription error, so the contract must parse them as numbers.
func TestMergeResultParsesNumberedReplaces(t *testing.T) {
	var res mergeResult
	body := `{"merges":[{"fact":"Cold outreach to SNITCH, RetainIQ and Omneky decided 21 Jul 2026, not started.","importance":"medium","replaces":[3,7,12]}]}`
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("the documented reply shape must parse: %v", err)
	}
	if len(res.Merges) != 1 || len(res.Merges[0].Replaces) != 3 {
		t.Fatalf("parsed %+v", res.Merges)
	}
	if res.Merges[0].Replaces[0] != 3 {
		t.Errorf("replaces should carry the numbers shown, got %v", res.Merges[0].Replaces)
	}
	// An empty answer is a valid answer and must not error.
	if err := json.Unmarshal([]byte(`{"merges":[]}`), &res); err != nil {
		t.Errorf("the no-op reply must parse: %v", err)
	}
}
