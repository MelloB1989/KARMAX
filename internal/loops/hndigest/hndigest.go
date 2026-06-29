// Package hndigest is a reference loopkit loop: a daily Hacker News digest.
// It doubles as a copy-paste template for third-party loop authors — the only
// difference for an external loop is that it lives in its own Go module instead
// of in-repo under internal/.
package hndigest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "hn-digest",
		Description: "Summarizes the current Hacker News top stories and pushes a digest to the app each morning.",
		Schedule:    loopkit.Cron("0 0 9 * * *"), // 09:00:00 daily
		Run:         run,
	})
}

func run(ctx context.Context, k loopkit.Kit) error {
	// 1. Fetch the current top-story IDs (free, no key).
	body, status, err := k.HTTP(ctx, "GET", "https://hacker-news.firebaseio.com/v0/topstories.json", nil, "")
	if err != nil {
		return fmt.Errorf("fetch topstories: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("topstories returned status %d", status)
	}
	var ids []int
	if err := json.Unmarshal([]byte(body), &ids); err != nil {
		return fmt.Errorf("parse ids: %w", err)
	}

	// 2. Fetch the top 10 titles.
	var titles []string
	for i, id := range ids {
		if i >= 10 {
			break
		}
		ib, _, err := k.HTTP(ctx, "GET", fmt.Sprintf("https://hacker-news.firebaseio.com/v0/item/%d.json", id), nil, "")
		if err != nil {
			continue
		}
		var item struct {
			Title string `json:"title"`
		}
		if json.Unmarshal([]byte(ib), &item) == nil && item.Title != "" {
			titles = append(titles, item.Title)
		}
	}
	if len(titles) == 0 {
		return fmt.Errorf("no stories fetched")
	}

	// 3. Summarize via the Claude harness (web/text, codex-independent). Fall
	//    back to the raw titles if the harness is unavailable.
	prompt := "Summarize these Hacker News top stories into 5 short, punchy bullets for a tech founder. " +
		"Output only the bullets.\nTitles:\n- " + strings.Join(titles, "\n- ")
	digest, err := k.Harness(ctx, prompt)
	if err != nil || strings.TrimSpace(digest) == "" {
		k.Logf("harness unavailable (%v); falling back to raw titles", err)
		digest = "Top Hacker News stories:\n- " + strings.Join(titles, "\n- ")
	}

	// 4. Persist to memory + notify the app.
	_ = k.Remember("Hacker News digest: " + digest)
	return k.Notify("Hacker News digest", digest)
}
