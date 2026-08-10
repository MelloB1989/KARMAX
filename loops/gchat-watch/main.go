//go:build wasip1

// Package gchatwatch watches Google Chat (via gws) and proactively acts on
// new messages: routine dev chores immediately, real decisions flagged.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/loops/internal/shared"
	"github.com/MelloB1989/karmax/pkg/loopwasm"
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

// gchatAuthDown latches the Google-auth state so the operator is alerted ONCE
// when gws loses authentication (Google's periodic reauth for sensitive scopes
// invalidates the token — "invalid_rapt"/"invalid_grant"/exit 2), and once more
// when it recovers — instead of failing silently every 2 minutes. Guarded by
// gchatMu, which the run holds. errGchatAuth is the sentinel for that state.
var gchatAuthDown bool

type gchatAuthError struct{ detail string }

func (e *gchatAuthError) Error() string { return "google auth: " + e.detail }

func isGchatAuthError(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "invalid_rapt") || strings.Contains(l, "invalid_grant") ||
		strings.Contains(l, "autherror") || strings.Contains(l, "auth error") ||
		strings.Contains(l, "reauth") || strings.Contains(l, "credentials missing") ||
		strings.Contains(l, "unauthenticated") || strings.Contains(l, "401")
}

type gchatSpace struct {
	Name           string `json:"name"` // "spaces/XXXX"
	DisplayName    string `json:"displayName"`
	SpaceType      string `json:"spaceType"`
	LastActiveTime string `json:"lastActiveTime"`
}

//go:wasmexport run
func run() {
	if err := watch(); err != nil {
		loopwasm.Log("gchat-watch: %v", err)
	}
}

func watch() error {
	if !gchatMu.TryLock() {
		return nil // previous tick still working
	}
	defer gchatMu.Unlock()

	gws := strings.TrimSpace(loopwasm.Config("gws"))
	if gws == "" {
		gws = loopwasm.HostTool("gws")
	}

	statePath, err := gchatStatePath()
	if err != nil {
		return err
	}
	state := loadGchatState(statePath)

	spaces, err := listGchatSpaces()
	if err != nil {
		var authErr *gchatAuthError
		if errors.As(err, &authErr) {
			// Latched: alert the operator ONCE (Google reauth for sensitive
			// scopes needs an interactive `gws auth login` on the host — it
			// can't be refreshed non-interactively), and stop spamming a WARN
			// every 2 minutes. Recovery is announced below.
			if !gchatAuthDown {
				gchatAuthDown = true
				msg := "⚠️ Google Workspace access expired (Google Chat/Gmail/Calendar via gws). Run `gws auth login` on the KARMAX host to reconnect — until then I can't watch or act on Google Chat. (" + authErr.detail + ")"
				_ = loopwasm.Notify("⚠️ Google access expired", msg)
				loopwasm.Log("gchat-watch: google auth DOWN — %s", authErr.detail)
			}
			return nil
		}
		return fmt.Errorf("gchat-watch: list spaces: %w", err)
	}
	if gchatAuthDown {
		gchatAuthDown = false
		_ = loopwasm.Notify("✅ Google access restored", "Google Workspace is reconnected — I'm watching Google Chat again.")
		loopwasm.Log("gchat-watch: google auth restored")
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
		"3. If it's a real DECISION, production-destructive (force-push, deleting repos/branches others use), money, or ambiguous — do NOT act; flag it as APPROVE.\n" +
		"4. If it's something ONLY the operator can personally do (attend something, provide a file/credential you don't have): flag it as REMIND.\n" +
		"5. Social chatter with no ask → skip.\n\n" +
		shared.ScanOutputSpec

	out, err := loopwasm.Harness(prompt)
	if err != nil {
		return fmt.Errorf("gchat-watch: harness: %w", err)
	}
	if shared.LooksLikeError(out) {
		return fmt.Errorf("gchat-watch: harness returned error/refusal: %.120s", out)
	}

	// Only advance checkpoints for the spaces we actually processed.
	for _, w := range work {
		state[w.space.Name] = w.space.LastActiveTime
	}
	saveGchatState(statePath, state)

	acted, approve, remind, inform := shared.ParseScanOutcomes(out)
	loopwasm.Log("gchat-watch: %d spaces — %d acted, %d need approval, %d reminders, %d fyi", len(work), len(acted), len(approve), len(remind), len(inform))
	if len(acted) > 0 {
		_ = loopwasm.Notify("✅ Handled on Google Chat", "• "+strings.Join(acted, "\n• "))
	}
	shared.ProposeItems("Flagged by the gchat-watch loop from Google Chat activity.", approve)
	shared.RemindItems("Flagged by the gchat-watch loop: only you can do this one.", remind)
	shared.InformItems("📣 Google Chat update", inform)
	return nil
}

func listGchatSpaces() ([]gchatSpace, error) {
	raw, err := loopwasm.GoogleChatSpaces()
	out := []byte(raw)
	if err != nil {
		if isGchatAuthError(string(out)) || strings.Contains(err.Error(), "exit status 2") {
			return nil, &gchatAuthError{detail: firstLine(string(out))}
		}
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(firstLine(string(out))))
	}
	// A successful call can still carry an auth-error JSON body (gws exits 0).
	if isGchatAuthError(string(out)) {
		return nil, &gchatAuthError{detail: firstLine(string(out))}
	}
	var resp struct {
		Spaces []gchatSpace `json:"spaces"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	return resp.Spaces, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
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

func main() {}
