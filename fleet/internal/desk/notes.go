package desk

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Claude Code says what it needs through its hooks: every Claude Code the
// desk starts runs with --settings HookSettings, whose hooks run
// HookScript at a notification (it needs a permission, asks something, or
// waits for the operator), at the end of each turn, and when the operator
// sends a prompt. The script leaves a note for the session (tmux gives it
// FLEET_DESK_SESSION), which the list reads: a prompt's note says nothing,
// and the prompt itself is never kept.

// hookScript is HookScript: it never fails Claude Code, and keeps only a
// notification's payload (Claude Code's message), never a prompt.
const hookScript = `#!/bin/sh
# fleetd's desk: Claude Code's hooks leave a note of what it needs
# (fleet/internal/desk/notes.go). Written by fleetd.
s=$FLEET_DESK_SESSION
case "$s" in ""|*/*|.*) cat >/dev/null; exit 0 ;; esac
d=${FLEET_DESK_NOTES:-$HOME/.karmax/fleet/desk-notes}
mkdir -p "$d" 2>/dev/null || { cat >/dev/null; exit 0; }
t="$d/.$s.$$"
if [ "$1" = notification ]; then
	{ printf '%s %s\n' "$1" "$(date +%s)"; cat; } >"$t" 2>/dev/null
else
	cat >/dev/null
	printf '%s %s\n' "$1" "$(date +%s)" >"$t" 2>/dev/null
fi
mv -f "$t" "$d/$s" 2>/dev/null
exit 0
`

// hookSettings is HookSettings for the script at path.
func hookSettings(script string) []byte {
	cmd := func(ev string) []map[string]any {
		return []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": shq(script) + " " + ev, "timeout": 10}}}}
	}
	b, _ := json.MarshalIndent(map[string]any{"hooks": map[string]any{
		"Notification":     cmd("notification"),
		"Stop":             cmd("stop"),
		"UserPromptSubmit": cmd("prompt"),
	}}, "", "  ")
	return append(b, '\n')
}

// hooks writes HookScript and HookSettings when they are not as they
// should be, and says whether Claude Code may be given them.
func (m *Manager) hooks() bool {
	if m.HookSettings == "" || m.HookScript == "" {
		return false
	}
	ok := func(path string, want []byte, mode os.FileMode) bool {
		if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, want) {
			return true
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			m.logf("hooks: %v", err)
			return false
		}
		tmp := path + ".new"
		if err := os.WriteFile(tmp, want, mode); err != nil {
			m.logf("hooks: %v", err)
			return false
		}
		if err := os.Rename(tmp, path); err != nil {
			m.logf("hooks: %v", err)
			return false
		}
		return true
	}
	return ok(m.HookScript, []byte(hookScript), 0o700) && ok(m.HookSettings, hookSettings(m.HookScript), 0o600)
}

func (m *Manager) notesDir() string {
	if m.Notes != "" {
		return m.Notes
	}
	return filepath.Join(filepath.Dir(m.State), "desk-notes")
}

// noteCache is each session's note as last read, by its file's time and
// size: the list reads a file again only when the hook wrote it again.
type noteCache struct {
	mu sync.Mutex
	m  map[string]noteSeen
}

type noteSeen struct {
	mod  time.Time
	size int64
	note *fleetapi.DeskNote
}

// note is what session name's Claude Code last said it needs, nil for
// nothing (it was answered, or no hook ran).
func (m *Manager) note(name string) *fleetapi.DeskNote {
	p := filepath.Join(m.notesDir(), name)
	fi, err := os.Stat(p)
	m.notes.mu.Lock()
	defer m.notes.mu.Unlock()
	if m.notes.m == nil {
		m.notes.m = map[string]noteSeen{}
	}
	if err != nil {
		delete(m.notes.m, name)
		return nil
	}
	old, seen := m.notes.m[name]
	if seen && old.mod.Equal(fi.ModTime()) && old.size == fi.Size() {
		return old.note
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return old.note
	}
	n := parseNote(b, old.note)
	m.notes.m[name] = noteSeen{mod: fi.ModTime(), size: fi.Size(), note: n}
	return n
}

// parseNote is a hook's note ("<event> <unix seconds>", then a
// notification's payload). Claude Code's reminder that it still waits
// (idle_prompt), after a turn that ended, is not news: the earlier note
// stands.
func parseNote(b []byte, prev *fleetapi.DeskNote) *fleetapi.DeskNote {
	head, rest, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(head))
	if len(f) != 2 {
		return nil
	}
	sec, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return nil
	}
	at := time.Unix(sec, 0).UTC()
	switch f[0] {
	case "stop":
		return &fleetapi.DeskNote{Kind: fleetapi.NoteDone, Text: "finished its turn", At: at}
	case "notification":
		var p struct {
			Message string `json:"message"`
			Type    string `json:"notification_type"`
		}
		json.Unmarshal(rest, &p)
		if p.Type == "idle_prompt" && prev != nil && prev.Kind == fleetapi.NoteDone {
			return prev
		}
		text := strings.TrimSpace(p.Message)
		text = strings.TrimPrefix(text, "Claude ")
		if text == "" {
			text = "needs you"
		}
		return &fleetapi.DeskNote{Kind: fleetapi.NoteNeeds, Text: oneLine(text), At: at}
	}
	return nil // prompt: the operator answered
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}

// forgetNote removes a session's note: the session ended.
func (m *Manager) forgetNote(name string) {
	os.Remove(filepath.Join(m.notesDir(), name))
	m.notes.mu.Lock()
	delete(m.notes.m, name)
	m.notes.mu.Unlock()
}
