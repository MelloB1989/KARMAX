// livecheck runs the privacy guard against this machine's real data.
//
// The guard's unit tests use a made-up list of names. This runs it against the
// actual address book and the actual memory, which is the only way to find the
// failure that matters: a contact saved as "Hyd T Service" making the word
// "service" unpublishable. Unit tests cannot contain that, because nobody would
// invent it.
//
//	go run ./internal/social/livecheck/
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/social"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func main() {
	home := os.Getenv("HOME")
	dsn := filepath.Join(home, ".karmax", "db", "karmax.db")
	if u := strings.TrimSpace(os.Getenv("KARMAX_DB_URL")); u != "" {
		dsn = u
	}
	db, err := store.New(dsn, zap.NewNop())
	if err != nil {
		panic(err)
	}
	defer db.Close()

	var names []string
	contacts, err := db.ContactNames()
	if err != nil {
		panic(err)
	}
	names = append(names, contacts...)
	fmt.Printf("%d names from contacts\n", len(contacts))

	// The memory half. This is what supplies client and project names, which the
	// address book does not have — nobody saves "CampX" as a phone contact.
	factory := memory.NewFactory(filepath.Join(home, ".karmax", "memory"), db, zap.NewNop())
	if cfg, ok := memory.GitLoomConfigFromEnv("nexus"); ok {
		factory.UseGitLoom(cfg)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		entries, err := factory.For("nexus", "nexus").Survey(ctx, 0)
		if err != nil {
			fmt.Printf("memory unavailable: %v\n", err)
		} else {
			for _, e := range entries {
				if s := subject(e.ID); s != "" {
					names = append(names, s)
				}
			}
			fmt.Printf("%d subjects from memory\n", len(entries))
		}
	} else {
		fmt.Println("GitLoom not configured; contacts only")
	}
	fmt.Println()

	g := social.Guard{Forbidden: names, MaxRunes: 280}
	for _, d := range drafts {
		if err := g.Check(d); err != nil {
			fmt.Printf("REFUSED  %s\n         %v\n\n", d, err)
		} else {
			fmt.Printf("OK       %s\n\n", d)
		}
	}
}

// Ordinary posts must pass and leaks must not. Both halves matter equally:
// a guard that refuses everything gets switched off.
var drafts = []string{
	"Spent the day pulling a reporting service out of a monolith. The extraction was easy; the deployment was not.",
	"Three hours lost to a race condition that turned out to be a typo.",
	"Rewrote the scheduler so it survives a restart. Half the code, twice the confidence.",
	"Finally shipped the credentials retrieval for CampX today.",
	"Sent Shiva Charan the pentester login over WhatsApp.",
	"Closed a deal worth 2L today.",
	"Reach me on +91 98765 43210 if you want the deck.",
	"Long call with Srikanth about the rollout schedule.",
}

// subject mirrors runtime.subjectOf, which is unexported. Kept in step by the
// drafts above: if they start failing, the two have drifted.
func subject(path string) string {
	name := path
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".md")
	if i := strings.IndexByte(name, '#'); i >= 0 {
		name = name[:i]
	}
	name = strings.ReplaceAll(name, "-", " ")

	words := strings.Fields(name)
	if len(words) == 0 || len(words) > 3 {
		return ""
	}
	for _, w := range words {
		if strings.ContainsAny(w, "0123456789") {
			return ""
		}
	}
	ordinary := true
	for _, w := range words {
		if !social.Topic(w) {
			ordinary = false
			break
		}
	}
	if ordinary {
		return ""
	}
	return name
}
