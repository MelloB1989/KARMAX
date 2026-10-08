package builtin

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func mirrorStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "karmax.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func captureMirror(t *testing.T) *[]MirrorEvent {
	var mu sync.Mutex
	var got []MirrorEvent
	SetMirrorHook(func(ev MirrorEvent) { mu.Lock(); got = append(got, ev); mu.Unlock() })
	t.Cleanup(func() { SetMirrorHook(nil) })
	return &got
}

func TestMirrorFiresOnAllThreePaths(t *testing.T) {
	s := mirrorStore(t)
	got := captureMirror(t)

	if _, err := (&AppPushTool{Store: s}).Execute(context.Background(), map[string]any{"title": "T1", "body": "b1"}); err != nil {
		t.Fatal(err)
	}
	PushAppNotification(s, "", "reminder", "T2", "b2")
	id, err := CreateProposal(s, "", "task", "T3", "why", "do it", "normal")
	if err != nil || id == "" {
		t.Fatalf("proposal: %q %v", id, err)
	}

	if len(*got) != 3 {
		t.Fatalf("mirror calls = %d, want 3: %+v", len(*got), *got)
	}
	if (*got)[0].Kind != MirrorNotification || (*got)[0].Title != "T1" || (*got)[0].Source != "app.push" {
		t.Errorf("app.push event wrong: %+v", (*got)[0])
	}
	if (*got)[1].Kind != MirrorNotification || (*got)[1].Title != "T2" {
		t.Errorf("PushAppNotification event wrong: %+v", (*got)[1])
	}
	if e := (*got)[2]; e.Kind != MirrorApproval || e.ProposalID != id || e.Title != "T3" {
		t.Errorf("proposal event wrong: %+v", e)
	}
}

func TestQuietPushAndSuppressedRepeatsAreNotMirrored(t *testing.T) {
	s := mirrorStore(t)
	got := captureMirror(t)

	PushAppNotificationQuiet(s, "", "update", "Sent to x", "body")
	if len(*got) != 0 {
		t.Fatalf("quiet push was mirrored: %+v", *got)
	}
	PushAppNotification(s, "", "alert", "same", "b")
	PushAppNotification(s, "", "alert", "same", "b")
	if len(*got) != 1 {
		t.Fatalf("repeat alert mirrored %d times, want 1", len(*got))
	}
}
