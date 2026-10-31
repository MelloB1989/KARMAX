package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/chatmemory"
	"github.com/MelloB1989/karmax/internal/identity"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
)

// waMemoryConfig reads the WhatsApp → memory syncer's settings, and reports
// false when KARMAX_WA_MEMORY=off.
//
//	KARMAX_WA_MEMORY_SINCE      where a chat never read before starts (YYYY-MM-DD; default a week back)
//	KARMAX_WA_MEMORY_DENY_FILE  chats never read, one ID or name per line (default ~/.karmax/wa-memory-deny.txt)
//	KARMAX_WA_MEMORY_BATCHES    model calls one tick may make (default 12)
func waMemoryConfig(getenv func(string) string, karmaxHome string, now time.Time, operatorJIDs []string) (chatmemory.Config, bool) {
	if strings.EqualFold(strings.TrimSpace(getenv("KARMAX_WA_MEMORY")), "off") {
		return chatmemory.Config{}, false
	}
	cfg := chatmemory.Config{Since: now.AddDate(0, 0, -7), OperatorIDs: operatorJIDs, Location: memory.OperatorLocation()}
	if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(getenv("KARMAX_WA_MEMORY_SINCE")), cfg.Location); err == nil {
		cfg.Since = d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv("KARMAX_WA_MEMORY_BATCHES"))); err == nil && n > 0 {
		cfg.BatchesPerTick = n
	}
	cfg.Operator = identity.NameFromEnv(getenv)
	deny := strings.TrimSpace(getenv("KARMAX_WA_MEMORY_DENY_FILE"))
	if deny == "" {
		deny = filepath.Join(karmaxHome, "wa-memory-deny.txt")
	}
	if b, err := os.ReadFile(deny); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			l, _, _ = strings.Cut(l, "#")
			if l = strings.TrimSpace(l); l != "" {
				cfg.Deny = append(cfg.Deny, l)
			}
		}
	}
	return cfg, true
}

// kvCursors keeps the syncer's per-chat cursors in the store.
type kvCursors struct{ s *store.Store }

const waMemoryCursorGroup = "wa-memory-cursor"

func (c kvCursors) Get(chat string) (chatmemory.Cursor, bool, error) {
	v, ok, err := c.s.KVGet(waMemoryCursorGroup, chat)
	if err != nil || !ok {
		return chatmemory.Cursor{}, false, err
	}
	at, id, _ := strings.Cut(v, "|")
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return chatmemory.Cursor{}, false, nil // unreadable: read the chat from Since again
	}
	return chatmemory.Cursor{At: t, ID: id}, true, nil
}

func (c kvCursors) Set(chat string, cur chatmemory.Cursor) error {
	return c.s.KVSet(waMemoryCursorGroup, chat, cur.At.Format(time.RFC3339Nano)+"|"+cur.ID, 0)
}

// harnessExtract runs one extraction on the "extract" kind, in a session of
// its own: each batch is one chat, and a session carried over would set one
// chat's people beside the next one's.
func (rt *KarmaxRuntime) harnessExtract(ctx context.Context, key, prompt string) (string, error) {
	if rt == nil || rt.harness == nil {
		return "", errors.New("the harness is off")
	}
	defer rt.harness.Close(key)
	turn, err := rt.harness.Send(ctx, key, "extract", prompt)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(turn.Text) == "" {
		return "", errors.New("the harness returned an empty turn")
	}
	return turn.Text, nil
}
