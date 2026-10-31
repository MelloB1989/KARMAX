package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWaMemoryConfigFromEnv(t *testing.T) {
	now := time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)
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
	cfg, ok := waMemoryConfig(func(k string) string { return env[k] }, dir, now, []string{"5790000000001@lid"})
	if !ok {
		t.Fatal("configured syncer reported off")
	}
	if cfg.Since.Format("2006-01-02") != "2026-07-31" {
		t.Errorf("since = %v", cfg.Since)
	}
	if len(cfg.Deny) != 2 || cfg.Deny[0] != "Memes" || cfg.Deny[1] != "120363000000000001@g.us" {
		t.Errorf("deny = %q", cfg.Deny)
	}
	if cfg.BatchesPerTick != 5 || cfg.Operator != "Quinn" || len(cfg.OperatorIDs) != 1 {
		t.Errorf("cfg = %+v", cfg)
	}

	env = map[string]string{}
	cfg, ok = waMemoryConfig(func(k string) string { return env[k] }, dir, now, nil)
	if !ok || !cfg.Since.Equal(now.AddDate(0, 0, -7)) {
		t.Errorf("default: ok=%v since=%v; want on, reading the last week", ok, cfg.Since)
	}

	env["KARMAX_WA_MEMORY"] = "off"
	if _, ok := waMemoryConfig(func(k string) string { return env[k] }, dir, now, nil); ok {
		t.Error("KARMAX_WA_MEMORY=off did not turn it off")
	}
}
