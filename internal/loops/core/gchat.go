package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// gchat-watch: event-based monitoring of Google Chat via the gws CLI. The Go
// side polls cheaply (spaces list, no LLM) and only when a space has NEW
// activity does it hand the thread to the Claude harness executor, which acts
// on the operator's behalf: routine dev chores (close/merge a PR a teammate
// asked for, quick acks, calendar) are done immediately; real decisions are
// flagged for approval. First run looks back 24h so pending asks are handled.
const (
	gchatMaxSpaces   = 5 // spaces handled per tick
	gchatFirstRunAge = 24 * time.Hour
)

var gchatMu sync.Mutex

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "gchat-watch",
		Description: "Watches Google Chat (via gws) and proactively acts on new messages: does routine asks (e.g. close a PR a teammate requested) itself, flags real decisions for approval.",
		Schedule:    loopkit.Every("2m"),
		Run:         runGchatWatch,
	})
}

type gchatSpace struct {
	Name           string `json:"name"` // "spaces/XXXX"
	DisplayName    string `json:"displayName"`
	SpaceType      string `json:"spaceType"`
	LastActiveTime string `json:"lastActiveTime"`
}

func runGchatWatch(ctx context.Context, k loopkit.Kit) error {
	if !gchatMu.TryLock() {
		return nil // previous tick still working
	}
	defer gchatMu.Unlock()

	gws := strings.TrimSpace(k.Config("gws"))
	if gws == "" {
		gws = hostpaths.GWS()
	}

	statePath, err := gchatStatePath()
	if err != nil {
		return err
	}
	state := loadGchatState(statePath)

	spaces, err := listGchatSpaces(ctx, gws)
	if err != nil {
		return fmt.Errorf("gchat-watch: list spaces: %w", err)
	}

	// Find spaces with activity newer than our checkpoint.
	type active struct {
		space gchatSpace
		since string
	}
	var work []active
	for _, sp := range spaces {
		if sp.Name == "" || sp.LastActiveTime == "" {
			continue
		}
		last := state[sp.Name]
		if last == "" {
			// First sighting: only look back a bounded window.
			cutoff := time.Now().Add(-gchatFirstRunAge).UTC().Format(time.RFC3339)
			if sp.LastActiveTime <= cutoff {
				state[sp.Name] = sp.LastActiveTime // nothing recent; just record
				continue
			}
			last = cutoff
		}
		if sp.LastActiveTime > last {
			work = append(work, active{space: sp, since: last})
		}
	}
	if len(work) == 0 {
		saveGchatState(statePath, state)
		return nil
	}
	if len(work) > gchatMaxSpaces {
		work = work[:gchatMaxSpaces]
	}

	var list strings.Builder
	for _, w := range work {
		name := w.space.DisplayName
		if name == "" {
			name = "(direct message)"
		}
		fmt.Fprintf(&list, "- %q | id: %s | new activity since %s\n", name, w.space.Name, w.since)
	}

	prompt := "You are the operator's proactive Google Chat assistant, working their account via the gws CLI at " + gws + " (Google Workspace; also available: the gh CLI for GitHub, git, and a full shell).\n\n" +
		"These Google Chat spaces have NEW activity:\n" + list.String() + "\n" +
		"For EACH space:\n" +
		"1. Read the recent messages: `" + gws + " chat spaces messages list --parent <space id> --page-size 15` (discover exact flags with --help if needed). Only consider messages newer than the given 'since' time, and ignore messages the operator themself sent (check senders; the operator is the authenticated account — their own asks to others are not for you).\n" +
		"2. If someone asked the operator to do something ROUTINE that you can complete on this machine — e.g. close or merge a specific PR (`gh pr close`/`gh pr merge` in the right repo), share a link/file they already have, a quick factual answer, scheduling via `gws calendar` — DO IT NOW, then reply briefly in the space via `" + gws + " chat +send --help` (discover syntax) in the operator's casual human voice (never reveal you're an AI).\n" +
		"3. If it's a real DECISION, production-destructive (force-push, deleting repos/branches others use), money, or ambiguous — do NOT act; flag it.\n" +
		"4. Social chatter with no ask → skip.\n\n" +
		"Output EXACTLY one line per space, no other text:\n" +
		"ACTED <space name>: <what you did>\n" +
		"APPROVE <space name>: <the ask + your suggested action>\n" +
		"SKIP <space name>: <why>"

	out, err := k.Harness(ctx, prompt)
	if err != nil {
		return fmt.Errorf("gchat-watch: harness: %w", err)
	}
	if looksLikeError(out) {
		return fmt.Errorf("gchat-watch: harness returned error/refusal: %.120s", out)
	}

	// Only advance checkpoints for the spaces we actually processed.
	for _, w := range work {
		state[w.space.Name] = w.space.LastActiveTime
	}
	saveGchatState(statePath, state)

	var acted, approve []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "ACTED"):
			acted = append(acted, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(up, "APPROVE"):
			approve = append(approve, strings.TrimSpace(line[7:]))
		}
	}
	k.Logf("gchat-watch: %d spaces — %d acted, %d need approval", len(work), len(acted), len(approve))
	if len(acted) > 0 {
		_ = k.Notify("✅ Handled on Google Chat", "• "+strings.Join(acted, "\n• "))
	}
	proposeItems(k, "Flagged by the gchat-watch loop from Google Chat activity.", approve)
	return nil
}

func listGchatSpaces(ctx context.Context, gws string) ([]gchatSpace, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, gws, "chat", "spaces", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var resp struct {
		Spaces []gchatSpace `json:"spaces"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	return resp.Spaces, nil
}

func gchatStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".karmax")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "gchat-watch.state"), nil
}

func loadGchatState(path string) map[string]string {
	state := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}

func saveGchatState(path string, state map[string]string) {
	if data, err := json.Marshal(state); err == nil {
		_ = os.WriteFile(path, data, 0644)
	}
}
