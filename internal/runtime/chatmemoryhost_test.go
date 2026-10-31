package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/chatmemory"
	"github.com/MelloB1989/karmax/internal/sources/googleacct"
)

// wa-memory's settings mean what they always meant.
func TestWaMemoryConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	deny := filepath.Join(dir, "deny.txt")
	if err := os.WriteFile(deny, []byte("# chats never read\nMemes\n\n120363000000000001@g.us  # spam\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KARMAX_WA_MEMORY_SINCE":               "2026-07-31",
		"KARMAX_WA_MEMORY_DENY_FILE":           deny,
		"KARMAX_WA_MEMORY_BATCHES":             "5",
		"KARMAX_LOOP_WA_MONITOR_OPERATOR_NAME": "Quinn",
	}
	cfg, ok := waMemory.config(func(k string) string { return env[k] }, dir)
	if !ok {
		t.Fatal("configured syncer reported off")
	}
	if cfg.Since.Format("2006-01-02") != "2026-07-31" {
		t.Errorf("since = %v", cfg.Since)
	}
	if len(cfg.Deny) != 2 || cfg.Deny[0] != "Memes" || cfg.Deny[1] != "120363000000000001@g.us" {
		t.Errorf("deny = %q", cfg.Deny)
	}
	if cfg.BatchesPerTick != 5 || cfg.Operator != "Quinn" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Name != "wa-memory" || cfg.Service != "WhatsApp" || cfg.Tag != "whatsapp" {
		t.Errorf("named %q / %q / %q", cfg.Name, cfg.Service, cfg.Tag)
	}

	env = map[string]string{}
	cfg, ok = waMemory.config(func(k string) string { return env[k] }, dir)
	if !ok || !cfg.Since.IsZero() || cfg.Lookback != 7*24*time.Hour {
		t.Errorf("default: ok=%v since=%v lookback=%v; want on, a week before the first look", ok, cfg.Since, cfg.Lookback)
	}

	env["KARMAX_WA_MEMORY"] = "off"
	if _, ok := waMemory.config(func(k string) string { return env[k] }, dir); ok {
		t.Error("KARMAX_WA_MEMORY=off did not turn it off")
	}
}

// Every chat service has its own switch and settings, named after its loop,
// and its own deny file beside wa-memory's.
func TestEachChatMemoryLoopReadsItsOwnSettings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gchat-memory-deny.txt"), []byte("spaces/AAAA\nWatercooler\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KARMAX_GCHAT_MEMORY_SINCE":   "2026-10-01",
		"KARMAX_GCHAT_MEMORY_BATCHES": "3",
		"KARMAX_OPERATOR_NAME":        "Quinn",
	}
	cfg, ok := gchatMemory.config(func(k string) string { return env[k] }, dir)
	if !ok {
		t.Fatal("gchat-memory reported off")
	}
	if cfg.Name != "gchat-memory" || cfg.Service != "Google Chat" || cfg.Tag != "gchat" || cfg.Operator != "Quinn" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Since.Format("2006-01-02") != "2026-10-01" || cfg.BatchesPerTick != 3 {
		t.Errorf("since %v, batches %d", cfg.Since, cfg.BatchesPerTick)
	}
	if len(cfg.Deny) != 2 || cfg.Deny[0] != "spaces/AAAA" {
		t.Errorf("deny = %q; want the loop's own deny file", cfg.Deny)
	}

	env["KARMAX_GCHAT_MEMORY"] = "off"
	if _, ok := gchatMemory.config(func(k string) string { return env[k] }, dir); ok {
		t.Error("KARMAX_GCHAT_MEMORY=off did not turn it off")
	}
	if _, ok := waMemory.config(func(k string) string { return env[k] }, dir); !ok {
		t.Error("switching Google Chat off switched WhatsApp off too")
	}
}

// Each loop keeps its cursors in a group of its own, and wa-memory's are the
// ones it always kept.
func TestChatMemoryCursorsAreKeptPerLoop(t *testing.T) {
	s := testStore(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if err := s.KVSet("wa-memory-cursor", "1@g.us", at.Format(time.RFC3339Nano)+"|ABC", 0); err != nil {
		t.Fatal(err)
	}
	wa, gc := waMemory.cursors(s), gchatMemory.cursors(s)
	if c, ok, err := wa.Get("1@g.us"); err != nil || !ok || !c.At.Equal(at) || c.ID != "ABC" {
		t.Fatalf("wa-memory lost its cursor: %+v %v %v", c, ok, err)
	}
	if _, ok, _ := gc.Get("1@g.us"); ok {
		t.Error("gchat-memory read wa-memory's cursor")
	}
	if err := gc.Set("1@g.us", chatmemory.Cursor{At: at.Add(time.Hour), ID: "Z"}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := wa.Get("1@g.us"); c.ID != "ABC" {
		t.Error("gchat-memory overwrote wa-memory's cursor")
	}
}

// An account's first look is recorded once and kept, so a restart does not
// move where its chats start.
func TestChatMemoryFirstLookIsKeptPerAccount(t *testing.T) {
	s := testStore(t)
	first := gchatMemory.firstLook(s, func() time.Time { return time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC) })
	a, err := first("quinn@example.org")
	if err != nil {
		t.Fatal(err)
	}
	later := gchatMemory.firstLook(s, func() time.Time { return time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC) })
	b, err := later("quinn@example.org")
	if err != nil || !a.Equal(b) {
		t.Errorf("first look moved from %v to %v (%v)", a, b, err)
	}
	c, _ := later("quinn@example.net")
	if !c.Equal(time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("a new account's first look = %v, want now", c)
	}
	if _, err := first(""); err != nil {
		t.Errorf("a single-account service has no first look: %v", err)
	}
}

// Google Chat memory reads the accounts the Chat source reads: Workspace
// accounts with a login granted chat.
func TestChatMemoryReadsTheAccountsChatServes(t *testing.T) {
	d := googleacct.Discovery{Accounts: []googleacct.Account{
		{Email: "quinn@example.org", Logins: []googleacct.Login{{Email: "quinn@example.org", Path: googleacct.Gws}}},
		{Email: "quinn.personal@gmail.com", Logins: []googleacct.Login{{Email: "quinn.personal@gmail.com", Path: googleacct.Gog}}},
		{Email: "ops@example.net", Logins: []googleacct.Login{{Email: "ops@example.net", Path: googleacct.Gog, Services: []string{"calendar"}}}},
	}}
	got := chatAccounts(d)
	if len(got) != 1 || got[0] != "quinn@example.org" {
		t.Errorf("accounts = %v", got)
	}
}
