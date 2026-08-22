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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	dsn := filepath.Join(home, ".karmax", "db", "karmax.db")
	if u := strings.TrimSpace(os.Getenv("KARMAX_DB_URL")); u != "" {
		dsn = u
	}
	db, err := store.New(dsn, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	defer db.Close()
	factory := memory.NewFactory(filepath.Join(home, ".karmax"), db, log)
	// Attach GitLoom exactly as the daemon does. Without this the factory hands
	// back a manager whose remote is nil, and Write falls through to the local
	// SQLite table — which stopped being the memory in August. The first cut of
	// this tool did precisely that: thirty-five facts reported "ingested",
	// every write returned nil, and not one of them was in the store the daemon
	// reads. A silent write to the wrong store is worse than a failure.
	if glCfg, ok := memory.GitLoomConfigFromEnv(ns); ok {
		factory.UseGitLoom(glCfg)
	} else if os.Getenv("MEMINGEST_ALLOW_LOCAL") == "" {
		fmt.Fprintln(os.Stderr,
			"refusing to ingest: GITLOOM_API_KEY is not set, so this would write to the local\n"+
				"table rather than the store KARMAX reads. Run from the KARMAX directory (its .env\n"+
				"carries the key), or set MEMINGEST_ALLOW_LOCAL=1 if a local write is genuinely wanted.")
		os.Exit(1)
	}
	mgr := factory.For("nexus", ns)
	configured, healthy, lastErr := mgr.RemoteStatus()
	fmt.Fprintf(os.Stderr, "memory backend: remote=%v healthy=%v %s\n", configured, healthy, lastErr)

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
		// Confirm the fact is readable before writing the next one.
		//
		// GitLoom keeps ONE document per subject and the fold is a client-side
		// read-modify-write: read the document, append a section, write it back
		// whole. Writes are also eventually consistent — a written section takes
		// ~6-8s to appear in a read. Back-to-back writes about one subject
		// therefore fold onto a stale document and silently overwrite each
		// other: measured, 35 facts written with no error, 6 survived. Waiting
		// for each to read back is what makes the next fold see it.
		if !confirm(mgr, memory.PathFor(entry, nil), l.Content) {
			fmt.Fprintf(os.Stderr, "NOT CONFIRMED (likely overwritten): %.80s\n", l.Content)
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

// confirm polls until the fact is readable at its path, or gives up.
func confirm(mgr *memory.Manager, path, content string) bool {
	probe := strings.ToLower(firstWords(content, 9))
	for attempt := 0; attempt < 45; attempt++ {
		if e, err := mgr.Load(context.Background(), path); err == nil && e != nil {
			if strings.Contains(strings.ToLower(e.Content), probe) {
				return true
			}
		}
		time.Sleep(time.Second)
	}
	return false
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = w[:n]
	}
	return strings.Join(w, " ")
}
