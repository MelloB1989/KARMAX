// Package loopkit is the public SDK for authoring KARMAX loops in Go.
//
// A "loop" is a unit of recurring work — a digest, a sync, a watcher, a
// reminder. Instead of a one-line prompt in karmax.yaml, a loopkit loop is real
// Go code with access to KARMAX's capabilities (the agent, memory, the Claude
// Code harness, push notifications, WhatsApp, HTTP) via the Kit interface.
//
// # Authoring a loop
//
// Create a Go module, import this package, and register your loop(s) in init():
//
//	package hndigest
//
//	import (
//		"context"
//		"github.com/MelloB1989/karmax/pkg/loopkit"
//	)
//
//	func init() {
//		loopkit.Register(loopkit.Loop{
//			Name:        "hn-digest",
//			Description: "Posts a Hacker News top-stories digest to the app every morning.",
//			Schedule:    loopkit.Cron("0 0 8 * * *"), // 08:00 daily
//			Run: func(ctx context.Context, k loopkit.Kit) error {
//				body, _, err := k.HTTP(ctx, "GET", "https://hacker-news.firebaseio.com/v0/topstories.json", nil, "")
//				if err != nil {
//					return err
//				}
//				digest, err := k.Harness(ctx, "Summarize the top items from this Hacker News data into 5 bullets:\n"+body)
//				if err != nil {
//					return err
//				}
//				_ = k.Remember("HN digest: " + digest)
//				return k.Notify("Hacker News digest", digest)
//			},
//		})
//	}
//
// # Installing a loop
//
// KARMAX is statically compiled, so an installed loop is a Go module blank-
// imported into the binary (its init() registers the loop). The `karmax loops`
// TUI automates this: it runs `go get`, adds the import, rebuilds, and restarts.
package loopkit

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Schedule controls when a loop fires. Set exactly one of Cron or Every.
type Schedule struct {
	// Cron is a 6-field cron spec WITH a seconds field (robfig/cron), e.g.
	// "0 0 9 * * *" = 09:00:00 every day, "0 */15 * * * *" = every 15 minutes.
	Cron string
	// Every is a human interval like "2h", "30m", "90s". Translated to "@every".
	Every string
}

// Every builds an interval schedule (e.g. loopkit.Every("2h")).
func Every(interval string) Schedule { return Schedule{Every: interval} }

// Cron builds a cron schedule (6 fields incl. seconds, e.g. "0 0 8 * * *").
func Cron(spec string) Schedule { return Schedule{Cron: spec} }

// CronExpr returns the scheduler expression for this schedule, or "" if unset.
func (s Schedule) CronExpr() string {
	switch {
	case s.Cron != "":
		return s.Cron
	case s.Every != "":
		return "@every " + s.Every
	default:
		return ""
	}
}

// Loop is a unit of recurring work, authored against the Kit capability API.
type Loop struct {
	// Name is a unique, kebab-case identifier (e.g. "hn-digest").
	Name string
	// Description is a one-line summary shown in the installer/activity views.
	Description string
	// Schedule controls when Run fires.
	Schedule Schedule
	// Run does the work. It receives a per-run context (honor cancellation/
	// timeout) and the Kit capability API. Return an error to log a failed run.
	Run func(ctx context.Context, k Kit) error
}

var (
	mu       sync.Mutex
	registry = map[string]Loop{}
)

// Register adds a loop to the global registry. Call it from your package's
// init(). It panics on an invalid or duplicate loop so problems surface at
// startup rather than silently dropping a loop.
func Register(l Loop) {
	mu.Lock()
	defer mu.Unlock()
	switch {
	case l.Name == "":
		panic("loopkit: loop has an empty Name")
	case l.Run == nil:
		panic(fmt.Sprintf("loopkit: loop %q has a nil Run", l.Name))
	case l.Schedule.CronExpr() == "":
		panic(fmt.Sprintf("loopkit: loop %q has no Schedule (set Cron or Every)", l.Name))
	}
	if _, dup := registry[l.Name]; dup {
		panic(fmt.Sprintf("loopkit: duplicate loop name %q", l.Name))
	}
	registry[l.Name] = l
}

// Registered returns every registered loop, sorted by name. KARMAX calls this
// at startup to schedule them.
func Registered() []Loop {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Loop, 0, len(registry))
	for _, l := range registry {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
