// memcat prints a memory document, for looking at what is actually stored.
package main

import (
	"context"
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
		ns = "karmax"
	}
	log := zap.NewNop()
	db, err := store.New(filepath.Join(home, ".karmax", "db", "karmax.db"), log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	defer db.Close()
	f := memory.NewFactory(filepath.Join(home, ".karmax"), db, log)
	if cfg, ok := memory.GitLoomConfigFromEnv(ns); ok {
		f.UseGitLoom(cfg)
	}
	m := f.For("nexus", ns)

	if len(os.Args) > 1 && os.Args[1] == "--list" {
		entries, err := m.Survey(context.Background(), 2000)
		if err != nil {
			fmt.Fprintln(os.Stderr, "survey:", err)
			os.Exit(1)
		}
		seen := map[string]bool{}
		for _, e := range entries {
			// The ID is the real GitLoom path for a remote-backed entry;
			// PathFor only derives where a NEW fact would be filed.
			path := e.ID
			if !strings.HasSuffix(path, ".md") {
				path = memory.PathFor(e, nil)
			}
			if path == "" || seen[path] {
				continue
			}
			seen[path] = true
			fmt.Println(path)
		}
		return
	}

	for _, path := range os.Args[1:] {
		e, err := m.Load(context.Background(), path)
		if err != nil || e == nil {
			fmt.Printf("=== %s : ERR %v\n", path, err)
			continue
		}
		fmt.Printf("=== %s (%d chars)\n%s\n", path, len(e.Content), e.Content)
	}
}
