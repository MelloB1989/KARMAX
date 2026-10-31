//go:build live

package chatmemory

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/sources/googleacct"
	"go.uber.org/zap"
)

// Against the Google accounts signed in on this host, read-only: what a
// gchat-memory backfill of the last week would read. Only counts are logged —
// no account, space, name or message text.
//
//	KARMAX_LIVE_GCHAT=1 go test -tags live -run TestLiveGChat ./internal/chatmemory/
func TestLiveGChatReadsSpacesAndTellsTheOperatorApart(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_GCHAT") != "1" {
		t.Skip("KARMAX_LIVE_GCHAT is not 1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := googleacct.NewClient(googleacct.NewHost(hostpaths.Gog(), os.Getenv), time.Local)
	accounts := func(ctx context.Context) []string {
		var out []string
		for _, a := range client.Host().Accounts(ctx).Accounts {
			if strings.HasSuffix(a.Email, "@gmail.com") || strings.HasSuffix(a.Email, "@googlemail.com") {
				continue // Chat's API serves Workspace accounts only
			}
			for _, l := range a.Logins {
				if l.Problem == "" && l.Can("chat") {
					out = append(out, a.Email)
					break
				}
			}
		}
		return out
	}
	src := NewGChat(client, accounts, zap.NewNop())
	t.Logf("workspace accounts with chat: %d", len(accounts(ctx)))

	start := time.Now()
	chats, err := src.Chats(ctx)
	if err != nil {
		t.Fatalf("chats: %v", err)
	}
	since := time.Now().AddDate(0, 0, -7)
	kinds := map[string]int{}
	var active []Chat
	for _, c := range chats {
		kinds[c.Kind]++
		if c.LastMessageAt.After(since) {
			active = append(active, c)
		}
	}
	t.Logf("spaces: %d (%v); active in the last 7 days: %d; listed in %v",
		len(chats), kinds, len(active), time.Since(start).Round(time.Millisecond))

	var total, mine, others, apps, empty, full, carried int
	senders := map[string]bool{}
	for _, c := range active {
		msgs, err := src.Messages(ctx, c, since, 151)
		if err != nil {
			t.Errorf("messages of one space: %v", err)
			continue
		}
		if len(msgs) > 150 {
			full++
		}
		for _, m := range msgs {
			total++
			switch {
			case strings.TrimSpace(m.Content) == "":
				empty++
			}
			switch {
			case m.FromMe:
				mine++
			case strings.HasSuffix(m.SenderName, "(an app)") || m.SenderName == "an app":
				apps++
			default:
				others++
				if m.SenderName != "" {
					carried++
				}
				if m.Sender != "" {
					senders[m.Sender] = true
				}
			}
		}
	}
	t.Logf("messages in the last 7 days: %d — the operator's own %d, others' %d, apps' %d; without text %d; spaces with more than one batch: %d",
		total, mine, others, apps, empty, full)

	named, lookups := 0, 0
	for s := range senders {
		if lookups >= 30 {
			break
		}
		lookups++
		if src.Name(ctx, s) != "" {
			named++
		}
	}
	t.Logf("others' messages carrying the sender's name: %d of %d; distinct other senders: %d, named %d of %d",
		carried, others, len(senders), named, lookups)
	if total > 0 && mine == 0 && others > 0 {
		t.Log("note: no message was attributed to the operator")
	}
}
