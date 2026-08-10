package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Comparing retrieval backends.
//
// Moving long-term memory to GitLoom made local a fallback, and a fallback
// nobody measures decays into a degraded mode. That matters more than it
// sounds: "runs fully offline, no account needed" is what the open-source path
// is sold on, and it stops being true the day local retrieval is too weak to
// use.
//
// `karmax compare` already measures both against the operator's real corpus,
// which is the right tool for judging a change. It needs a network, an account
// and real data, so it cannot run on every commit — and a check that only runs
// when someone remembers is not a line being held. This is the other half: a
// synthetic corpus, a floor, and a test.

// Case is one labelled retrieval question.
type Case struct {
	Query string
	// Want lists the entry ids that answer it, best first.
	Want []string
	// Why explains what the case is testing, so a regression report says
	// something more useful than a number going down.
	Why string
}

// Score is how a backend did against a case set.
type Score struct {
	Backend string
	Queries int
	Hit1    int
	Hit3    int
	Hit5    int
	MRR     float64
	P50     time.Duration
	P95     time.Duration
	// Missed names the cases that returned nothing relevant at all.
	Missed []string
}

// Recall1 and friends are the fractions the floor is expressed in.
func (s Score) Recall1() float64 { return frac(s.Hit1, s.Queries) }
func (s Score) Recall3() float64 { return frac(s.Hit3, s.Queries) }
func (s Score) Recall5() float64 { return frac(s.Hit5, s.Queries) }

func frac(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Searcher is the one method a backend has to offer to be compared.
type Searcher func(query string, topK int) ([]SearchResult, error)

// Compare runs a case set through a backend.
func Compare(backend string, search Searcher, cases []Case) Score {
	s := Score{Backend: backend, Queries: len(cases)}
	var latencies []time.Duration

	for _, c := range cases {
		start := time.Now()
		results, err := search(c.Query, 5)
		latencies = append(latencies, time.Since(start))
		if err != nil {
			s.Missed = append(s.Missed, c.Query+" (error: "+err.Error()+")")
			continue
		}

		rank := rankOfAny(results, c.Want)
		switch {
		case rank == 0:
			s.Missed = append(s.Missed, c.Query)
			continue
		case rank <= 1:
			s.Hit1++
			s.Hit3++
			s.Hit5++
		case rank <= 3:
			s.Hit3++
			s.Hit5++
		case rank <= 5:
			s.Hit5++
		}
		s.MRR += 1 / float64(rank)
	}
	if len(cases) > 0 {
		s.MRR /= float64(len(cases))
	}
	s.P50, s.P95 = percentiles(latencies)
	return s
}

// rankOfAny returns the 1-based position of the first wanted entry, or 0.
func rankOfAny(results []SearchResult, want []string) int {
	for i, r := range results {
		for _, w := range want {
			// Matched on id where there is one and on content otherwise: a
			// remote backend returns its own identifiers, and requiring them to
			// agree would compare plumbing rather than retrieval.
			if r.Entry.ID == w || strings.Contains(r.Entry.Content, w) || strings.Contains(r.Excerpt, w) {
				return i + 1
			}
		}
	}
	return 0
}

func percentiles(d []time.Duration) (p50, p95 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)*50/100], d[min(len(d)*95/100, len(d)-1)]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Report renders a comparison for a terminal.
func Report(scores ...Score) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s %8s %8s %8s %8s %10s %10s\n",
		"BACKEND", "top-1", "top-3", "top-5", "MRR", "p50", "p95")
	for _, s := range scores {
		fmt.Fprintf(&b, "%-12s %7.0f%% %7.0f%% %7.0f%% %8.2f %10s %10s\n",
			s.Backend, s.Recall1()*100, s.Recall3()*100, s.Recall5()*100, s.MRR,
			s.P50.Round(time.Microsecond), s.P95.Round(time.Microsecond))
	}
	for _, s := range scores {
		if len(s.Missed) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s found nothing for:\n", s.Backend)
		for _, m := range s.Missed {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
	}
	return b.String()
}
