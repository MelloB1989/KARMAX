package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gitloom "github.com/GitLoomHQ/gitloom-go/gitloom"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

// Measuring what the move actually bought.
//
// The interesting axis is not latency — a local SQLite LIKE scan will always
// win a race against a network call, and it is not the reason to move. It is
// whether a question phrased the way a person would ask it finds the memory
// that answers it. KARMAX's local retrieval matches keywords; a question that
// shares no words with the memory it needs returns nothing, and the agent
// answers "I don't have that information" about something it was told in June.
//
// So the probes below are deliberately written in the operator's words rather
// than the memory's.

// probe is one question, with the substring that proves the right memory came
// back. The marker is what makes this a measurement rather than a demo: without
// it, "returned five results" reads as success even when all five are wrong.
type probe struct {
	question string
	marker   string
}

var probes = []probe{
	{"who is my main contact for the CampX work", "siva"},
	{"what did we agree the VAPT engagement would cost", "campx"},
	{"who knows that I run an AI agent on my WhatsApp", "karmax"},
	{"which client has been chasing me about a late app build", "rishwak"},
	{"am I looking for a new job", "job"},
	{"what is the state of my driving licence", "licence"},
	{"who is the CTO at the college product company", "srikanth"},
	{"what is TrustStrike built on", "truststrike"},
	{"which of my projects is written in Go", "karmax"},
	{"who complained about my assistant messaging them", "shiva"},
}

func compareCmd() *cobra.Command {
	var (
		namespace string
		limit     int
		verbose   bool
	)
	cmd := &cobra.Command{
		Use:   "compare",
		Short: "Compare local SQLite retrieval against GitLoom on the same questions",
		RunE: func(c *cobra.Command, _ []string) error {
			return runCompare(c.Context(), namespace, limit, verbose)
		},
	}
	cmd.Flags().StringVar(&namespace, "namespace", "", "GitLoom namespace")
	cmd.Flags().IntVar(&limit, "limit", 8, "results per query")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print the top hit from each side")
	return cmd
}

func runCompare(ctx context.Context, namespace string, limit int, verbose bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := config.Load(findConfig())
	if err != nil {
		return err
	}
	db, err := store.New(filepath.Join(cfg.Karmax.DataDir, "db", "karmax.db"), zap.NewNop())
	if err != nil {
		return err
	}

	localNS := "nexus"
	if len(cfg.Agents) > 0 {
		if localNS = cfg.Agents[0].Memory.Namespace; localNS == "" {
			localNS = cfg.Agents[0].ID
		}
	}
	if namespace == "" {
		namespace = strings.TrimSpace(os.Getenv("GITLOOM_NAMESPACE"))
	}
	if namespace == "" {
		namespace = localNS
	}

	key := strings.TrimSpace(os.Getenv("GITLOOM_API_KEY"))
	if key == "" {
		return fmt.Errorf("GITLOOM_API_KEY is not set")
	}
	opts := []gitloom.Option{gitloom.WithNamespace(namespace)}
	if u := strings.TrimSpace(os.Getenv("GITLOOM_BASE_URL")); u != "" {
		opts = append(opts, gitloom.WithBaseURL(u))
	}
	client := gitloom.New(key, opts...)

	// The LOCAL manager, explicitly without a remote: this has to measure the
	// old path, not the new one wearing its name.
	local := memory.NewManager("compare", localNS, filepath.Join(cfg.Karmax.DataDir, "memory"), db, zap.NewNop())
	defer local.Stop()

	var (
		localHits, remoteHits   int
		localFound, remoteFound int
		localTime, remoteTime   time.Duration
	)

	fmt.Printf("%-52s %-18s %-18s\n", "QUESTION", "LOCAL (sqlite)", "GITLOOM")
	fmt.Println(strings.Repeat("─", 90))

	for _, p := range probes {
		t0 := time.Now()
		lr, lerr := local.SearchSemantic(p.question, limit)
		ld := time.Since(t0)
		localTime += ld

		t1 := time.Now()
		rr, rerr := client.Recall(ctx, p.question, &gitloom.RecallOptions{Namespace: namespace, Limit: limit, NoProvenance: true})
		rd := time.Since(t1)
		remoteTime += rd

		// Scored on the TOP 3, not on everything returned. Both sides return
		// eight results for every question, so "it was in there somewhere" is
		// satisfied by chance — and an agent composing a reply reads the first
		// few, not the eighth.
		const topN = 3
		lok, rok := false, false
		if lerr == nil {
			localHits += len(lr)
			for i, r := range lr {
				if i >= topN {
					break
				}
				if strings.Contains(strings.ToLower(r.Entry.Content), p.marker) {
					lok = true
					break
				}
			}
		}
		if rerr == nil {
			remoteHits += len(rr.Hits)
			for i, h := range rr.Hits {
				if i >= topN {
					break
				}
				if strings.Contains(strings.ToLower(h.Snippet+" "+h.Path), p.marker) {
					rok = true
					break
				}
			}
		}
		if lok {
			localFound++
		}
		if rok {
			remoteFound++
		}

		fmt.Printf("%-52s %-18s %-18s\n",
			truncQ(p.question, 50),
			fmt.Sprintf("%s %d hits", mark(lok), lenOf(lr, lerr)),
			fmt.Sprintf("%s %d hits", mark(rok), hitsOf(rr, rerr)))

		if verbose {
			fmt.Printf("    local  : %s\n", topLocal(lr, lerr))
			fmt.Printf("    gitloom: %s\n", topRemote(rr, rerr))
		}
	}

	n := len(probes)
	fmt.Println(strings.Repeat("─", 90))
	fmt.Printf("%-52s %-18s %-18s\n", "right memory in the top 3",
		fmt.Sprintf("%d/%d (%.0f%%)", localFound, n, 100*float64(localFound)/float64(n)),
		fmt.Sprintf("%d/%d (%.0f%%)", remoteFound, n, 100*float64(remoteFound)/float64(n)))
	fmt.Printf("%-52s %-18s %-18s\n", "results returned",
		fmt.Sprint(localHits), fmt.Sprint(remoteHits))
	fmt.Printf("%-52s %-18s %-18s\n", "mean latency",
		(localTime / time.Duration(n)).Round(time.Microsecond).String(),
		(remoteTime / time.Duration(n)).Round(time.Millisecond).String())
	fmt.Println("\nLatency is not the trade being made: the local scan is in-process and the")
	fmt.Println("remote call crosses a network. What is being bought is the first row.")
	return nil
}

func mark(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func lenOf(r []memory.SearchResult, err error) int {
	if err != nil {
		return 0
	}
	return len(r)
}

func hitsOf(r *gitloom.RecallResult, err error) int {
	if err != nil || r == nil {
		return 0
	}
	return len(r.Hits)
}

func topLocal(r []memory.SearchResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if len(r) == 0 {
		return "(nothing)"
	}
	return truncQ(strings.ReplaceAll(r[0].Entry.Content, "\n", " "), 100)
}

func topRemote(r *gitloom.RecallResult, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	if r == nil || len(r.Hits) == 0 {
		return "(nothing)"
	}
	return fmt.Sprintf("%s — %s", r.Hits[0].Path,
		truncQ(strings.ReplaceAll(r.Hits[0].Snippet, "\n", " "), 80))
}

func truncQ(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
