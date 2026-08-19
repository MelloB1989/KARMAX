// memingest writes durable facts into KARMAX's long-term memory without the
// daemon running.
//
// It exists for bulk imports — a meeting transcript, a backfill from chat
// history — where the daemon may deliberately be down (the operator can order
// KARMAX offline) but memory still has to be writable. It builds the SAME
// memory manager the daemon uses, so every entry goes through the production
// path: the same GitLoom folding, the same dedupe, the same safety checks.
//
// Input is JSON lines on stdin:
//
//	{"content":"...","category":"project","importance":"high","tags":["campx"],"pinned":false}
//
// Category and importance take the same values as `karmax memory add`.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/joho/godotenv"
	"go.uber.org/zap"
)

func main() {
	_ = godotenv.Load()
	home, _ := os.UserHomeDir()
	_ = godotenv.Load(filepath.Join(home, ".karmax", ".env"))

	ns := strings.TrimSpace(os.Getenv("KARMAX_MEMORY_NAMESPACE"))
	if ns == "" {
		ns = "nexus"
	}
	log, _ := zap.NewDevelopment()
	db, err := store.New(filepath.Join(home, ".karmax", "db", "karmax.db"), log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	defer db.Close()
	mgr := memory.NewFactory(filepath.Join(home, ".karmax"), db, log).For("nexus", ns)

	type line struct {
		Content    string   `json:"content"`
		Category   string   `json:"category"`
		Importance string   `json:"importance"`
		Tags       []string `json:"tags"`
		Pinned     bool     `json:"pinned"`
	}
	imp := map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	ok, failed := 0, 0
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			fmt.Fprintf(os.Stderr, "bad line: %v: %.80s\n", err, raw)
			failed++
			continue
		}
		if strings.TrimSpace(l.Content) == "" {
			continue
		}
		if l.Category == "" {
			l.Category = "context"
		}
		importance := imp[strings.ToLower(l.Importance)]
		if importance == 0 {
			importance = 2
		}
		entry := memory.MemoryEntry{
			Namespace: ns, Role: "system",
			Content:  fmt.Sprintf("[%s][%s] %s", l.Category, strings.ToLower(nonEmpty(l.Importance, "medium")), l.Content),
			Category: l.Category, Importance: importance, Tags: l.Tags, Pinned: l.Pinned,
		}
		if err := mgr.Write(entry); err != nil {
			fmt.Fprintf(os.Stderr, "write failed: %v: %.80s\n", err, l.Content)
			failed++
			continue
		}
		ok++
		fmt.Printf("  + %.90s\n", l.Content)
	}
	fmt.Printf("ingested %d, failed %d\n", ok, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
