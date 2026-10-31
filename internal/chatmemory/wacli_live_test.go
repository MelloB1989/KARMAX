//go:build live

package chatmemory

import (
	"context"
	"os"
	"testing"
	"time"
)

// Against the real wacli: reading forward from a point a month back returns
// that point's messages, not today's.
func TestLiveWacliReadsForwardFromTheCursor(t *testing.T) {
	base := os.Getenv("WACLI_API")
	if base == "" {
		t.Skip("no WACLI_API")
	}
	ctx := context.Background()
	src := NewWacli(base)
	chats, err := src.Chats(ctx)
	if err != nil || len(chats) == 0 {
		t.Fatalf("chats: %d, %v", len(chats), err)
	}
	var busiest Chat
	for _, c := range chats {
		if c.IsGroup && c.LastMessageAt.After(busiest.LastMessageAt) {
			busiest = c
		}
	}
	from := time.Now().AddDate(0, -1, 0)
	got, err := src.Messages(ctx, busiest, Cursor{At: from}, 50)
	if err != nil || len(got) == 0 {
		t.Fatalf("messages: %d, %v", len(got), err)
	}
	if got[0].At.Before(from) || got[0].At.After(from.Add(7*24*time.Hour)) {
		t.Errorf("first message %v is not the first after %v", got[0].At, from)
	}
	t.Logf("%s: %d messages from %v to %v", busiest.ID, len(got), got[0].At, got[len(got)-1].At)
}
