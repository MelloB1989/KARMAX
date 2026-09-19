package instagram

// The passthrough's whole safety property is that writes are unreachable, so
// that is what these check — against the real helper and the real instagrapi
// installed on this machine, not against a list copied into the test.
//
// None of these sign in. The refusal and the discovery both happen before any
// account is touched, which is deliberate: an agent must learn the boundary
// without anybody's credentials being involved.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// writeMethods are the ones that would cost somebody their account. If the
// allowlist ever grows one of these — by a careless edit, or by someone
// "fixing" it into a denylist — this fails.
var writeMethods = []string{
	"direct_send", "direct_send_photo", "direct_answer",
	"media_comment", "media_like", "media_unlike", "media_delete", "media_archive",
	"user_follow", "user_unfollow", "user_block", "user_unblock",
	"photo_upload", "video_upload", "album_upload", "story_seen",
	"comment_like", "account_edit", "account_change_picture",
}

func TestNoWriteMethodIsReachable(t *testing.T) {
	h := testHelper(t)
	var out struct {
		Reads []struct {
			Method string `json:"method"`
		} `json:"reads"`
	}
	if err := h.call(context.Background(), "reads", nil, &out); err != nil {
		t.Fatalf("reads: %v", err)
	}
	if len(out.Reads) == 0 {
		t.Fatal("the allowlist came back empty, which would make the passthrough useless")
	}
	allowed := map[string]bool{}
	for _, r := range out.Reads {
		allowed[r.Method] = true
	}
	for _, w := range writeMethods {
		if allowed[w] {
			t.Errorf("%s is reachable through the read passthrough — it must not be", w)
		}
	}
}

func TestRefusingAWriteSaysWhyRatherThanJustNo(t *testing.T) {
	h := testHelper(t)
	err := h.call(context.Background(), "call",
		map[string]any{"method": "direct_send", "args": map[string]any{}}, nil)
	if err == nil {
		t.Fatal("direct_send must be refused")
	}
	low := strings.ToLower(err.Error())
	// The agent has to learn this is a boundary, not a typo or an outage.
	if !strings.Contains(low, "read-only") {
		t.Errorf("the refusal must say the passthrough is read-only, got: %v", err)
	}
	var he *Error
	if errors.As(err, &he) && he.HardStop {
		t.Error("a refused method is not one of Instagram's stop signals")
	}
}

func TestRefusalHappensBeforeSignIn(t *testing.T) {
	// A fresh helper has no session. If the allowlist were checked after
	// login, this would fail with "not signed in" and the agent would go
	// looking for credentials instead of reading the boundary.
	h := testHelper(t)
	err := h.call(context.Background(), "call",
		map[string]any{"method": "media_like", "args": map[string]any{}}, nil)
	if err == nil {
		t.Fatal("media_like must be refused")
	}
	if strings.Contains(strings.ToLower(err.Error()), "not signed in") {
		t.Errorf("refusal must not depend on being signed in, got: %v", err)
	}
}

func TestAnUnknownMethodSuggestsRealOnes(t *testing.T) {
	h := testHelper(t)
	err := h.call(context.Background(), "call",
		map[string]any{"method": "media_comment", "args": map[string]any{}}, nil)
	if err == nil {
		t.Fatal("media_comment (singular — it posts) must be refused")
	}
	// The singular/plural trap: the agent probably wanted media_comments.
	if !strings.Contains(err.Error(), "media_comments") {
		t.Errorf("want a pointer to the read that was probably meant, got: %v", err)
	}
}

func TestReadsCarryTheirSignatures(t *testing.T) {
	h := testHelper(t)
	var out struct {
		Count int `json:"count"`
		Reads []struct {
			Method    string `json:"method"`
			Signature string `json:"signature"`
		} `json:"reads"`
	}
	if err := h.call(context.Background(), "reads", nil, &out); err != nil {
		t.Fatalf("reads: %v", err)
	}
	if out.Count != len(out.Reads) || out.Count == 0 {
		t.Fatalf("count %d does not match %d entries", out.Count, len(out.Reads))
	}
	for _, r := range out.Reads {
		// Without arguments the agent has to guess, and guessing at an API
		// this large is how it ends up calling the wrong thing.
		if r.Signature == "" {
			t.Errorf("%s has no signature", r.Method)
		}
		if strings.Contains(r.Signature, "self") {
			t.Errorf("%s leaks the bound receiver into its signature: %s", r.Method, r.Signature)
		}
	}
}

func TestBadArgumentsNameTheRealSignature(t *testing.T) {
	h := testHelper(t)
	// Allowlisted, so it gets past the boundary check and fails on binding —
	// which is the error that should teach the caller the right argument name.
	err := h.call(context.Background(), "call",
		map[string]any{"method": "media_info", "args": map[string]any{"nonsense_arg": 1}}, nil)
	if err == nil {
		t.Fatal("a bogus argument must fail")
	}
	if !strings.Contains(err.Error(), "media_info(") {
		t.Errorf("want the real signature in the error, got: %v", err)
	}
}
