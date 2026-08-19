// memverify checks that facts are actually present in the live memory store.
//
// Presence is checked by LOADING the subject documents and searching their full
// text — not by scanning a survey, which returns per-document summaries, and
// not by trusting a nil from Write. A write can return nil and still be lost:
// GitLoom stores one document per subject and the fold is a client-side
// read-modify-write, so two quick writes to the same subject can race and the
// second silently discards the first.
package main

import (
	"bufio"
	"context"
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
	log := zap.NewNop()
	db, err := store.New(filepath.Join(home, ".karmax", "db", "karmax.db"), log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store:", err)
		os.Exit(1)
	}
	defer db.Close()
	factory := memory.NewFactory(filepath.Join(home, ".karmax"), db, log)
	if glCfg, ok := memory.GitLoomConfigFromEnv(ns); ok {
		factory.UseGitLoom(glCfg)
	}
	m := factory.For("nexus", ns)

	type line struct {
		Content    string   `json:"content"`
		Category   string   `json:"category"`
		Importance string   `json:"importance"`
		Tags       []string `json:"tags"`
	}
	imp := map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}
	docs := map[string]string{}
	loadDoc := func(path string) string {
		if v, ok := docs[path]; ok {
			return v
		}
		body := ""
		if e, err := m.Load(context.Background(), path); err == nil && e != nil {
			body = strings.ToLower(e.Content)
		}
		docs[path] = body
		return body
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var present, missingList []string
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var l line
		if json.Unmarshal([]byte(raw), &l) != nil || l.Content == "" {
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
			Content:  fmt.Sprintf("[%s][%s] %s", l.Category, strings.ToLower(l.Importance), l.Content),
			Category: l.Category, Importance: importance, Tags: l.Tags,
		}
		path := memory.PathFor(entry, nil)
		probe := strings.ToLower(firstWords(l.Content, 9))
		if strings.Contains(loadDoc(path), probe) {
			present = append(present, l.Content)
		} else {
			missingList = append(missingList, l.Content)
		}
	}
	fmt.Printf("PRESENT: %d    MISSING: %d\n", len(present), len(missingList))
	for _, s := range missingList {
		fmt.Println("   ✗", firstWords(s, 12))
	}
	if len(missingList) > 0 {
		os.Exit(2)
	}
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = w[:n]
	}
	return strings.Join(w, " ")
}
