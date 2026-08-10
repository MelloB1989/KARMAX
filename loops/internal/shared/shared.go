// Package shared holds what the scan loops have in common: parsing the
// harness's structured output, and routing what it found to the right place.
//
// Ported from the compiled-in karmax-loops module. Three things changed, all of
// them because the sandbox has no ambient authority — and all three are better
// for it:
//
//   - operator chats came from os.Getenv; they now come from the host, which
//     knows them, rather than from whatever the daemon's environment holds
//   - the monitored-chat list came from an HTTP call to wacli on localhost;
//     loops cannot reach localhost any more, since that API sends messages as
//     the operator, so the host fetches it
//   - the pending-actions queue was a file in ~/.karmax; it is now short-term
//     memory, which is in the same store as everything else and visible to the
//     operator instead of being a file nobody knows about
package shared

import (
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/pkg/loopwasm"
)

// ScanOutputSpec is appended to every scan prompt so the harness answers in a
// shape that can be parsed rather than interpreted.
const ScanOutputSpec = `
Reply with ONLY these sections, each line prefixed exactly as shown. Omit a section entirely if it is empty.

ACTED: <one line per message you actually sent>
APPROVE: <one line per thing needing the operator's decision — include the draft and the recipient>
REMIND: <one line per thing only the operator can do>
INFORM: <one line per thing worth telling the operator about, needing no action>
`

// LooksLikeError reports whether harness output is a refusal or an error rather
// than an answer.
//
// The harness CLI prints refusals to stdout and exits 0, so without this a loop
// stores "I cannot help with that" as though it were a digest.
func LooksLikeError(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return true
	}
	for _, p := range []string{
		"i cannot", "i can't", "i'm unable", "i am unable", "i apologize",
		"i'm sorry", "i am sorry", "error:", "failed to", "unable to",
		"execution error", "api error", "rate limit",
	} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// ParseScanOutcomes splits the harness's reply into its four sections.
func ParseScanOutcomes(out string) (acted, approve, remind, inform []string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ACTED:"):
			acted = appendItem(acted, line, "ACTED:")
		case strings.HasPrefix(line, "APPROVE:"):
			approve = appendItem(approve, line, "APPROVE:")
		case strings.HasPrefix(line, "REMIND:"):
			remind = appendItem(remind, line, "REMIND:")
		case strings.HasPrefix(line, "INFORM:"):
			inform = appendItem(inform, line, "INFORM:")
		}
	}
	return
}

func appendItem(dst []string, line, prefix string) []string {
	item := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if MeaningfulItem(item) {
		return append(dst, item)
	}
	return dst
}

// MeaningfulItem filters out the model's ways of saying nothing.
func MeaningfulItem(item string) bool {
	t := strings.ToLower(strings.TrimSpace(item))
	if len(t) < 4 {
		return false
	}
	for _, empty := range []string{"none", "n/a", "na", "nothing", "-", "(none)", "no items", "nil"} {
		if t == empty {
			return false
		}
	}
	return true
}

// InformItems sends the operator a single notification for everything that
// needs no action, rather than one per item.
func InformItems(title string, items []string) {
	if len(items) == 0 {
		return
	}
	_ = loopwasm.Notify(title, "• "+strings.Join(items, "\n• "))
}

// ProposeItems raises one approval per thing needing a decision.
func ProposeItems(source string, items []string) {
	for _, item := range items {
		title := firstLine(item)
		if err := loopwasm.Propose(title, source, item); err != nil {
			loopwasm.Log("propose failed for %q: %v (falling back to a notification)", title, err)
			_ = loopwasm.Notify("⚠️ Needs your decision", item)
		}
	}
}

// RemindItems puts things only the operator can do on their list.
func RemindItems(source string, items []string) {
	for _, item := range items {
		title := firstLine(item)
		if err := loopwasm.Remind(title, "", source); err != nil {
			loopwasm.Log("remind failed for %q: %v (falling back to a notification)", title, err)
			_ = loopwasm.Notify("⏰ You need to do this yourself", item)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// MonitoredChats returns the chats KARMAX watches, minus the operator's own.
func MonitoredChats() ([]string, error) {
	var res struct {
		Chats []string `json:"chats"`
	}
	if err := loopwasm.ToolJSON("whatsapp.monitored", nil, &res); err != nil {
		return nil, err
	}
	return res.Chats, nil
}

// ReadThread returns a chat's recent messages as text for a prompt. The result
// arrives already fenced as untrusted content — whoever wrote it is not the
// operator — so it can be dropped into a prompt as-is.
//
// This lives here rather than in loopwasm because loopwasm knows nothing about
// WhatsApp, and that is the point: integrations reach a loop as tools.
func ReadThread(chatID string, limit int) string {
	var res struct {
		Messages string `json:"messages"`
	}
	if err := loopwasm.ToolJSON("whatsapp.read",
		map[string]any{"chat": chatID, "limit": limit}, &res); err != nil {
		return ""
	}
	return res.Messages
}

// SendWhatsApp sends as the operator, threading onto replyTo when non-empty.
func SendWhatsApp(chatID, text, replyTo string) error {
	_, err := loopwasm.Tool("whatsapp.send", map[string]any{
		"to": chatID, "text": text, "reply_to": replyTo})
	return err
}

// OperatorChatSet returns the operator's own chats, normalised for comparison.
func OperatorChatSet() map[string]bool {
	set := map[string]bool{}
	for _, c := range loopwasm.OperatorChats() {
		if n := NormalizeChatID(c); n != "" {
			set[n] = true
		}
	}
	return set
}

// NormalizeChatID reduces a chat id or phone number to a comparable form.
func NormalizeChatID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	return s
}

// The pending-actions queue.
//
// It decouples DISCOVERY — any scan loop enqueues — from EXECUTION, which
// act-on-pending drains. It was an append-only file; it is short-term memory
// now, which survives restarts the same way and is something the operator can
// actually look at.
const pendingGroup = "pending-actions"

// EnqueuePending adds actionable items to the queue.
func EnqueuePending(items []string) error {
	for i, item := range items {
		item = strings.ReplaceAll(strings.TrimSpace(item), "\n", " ")
		if item == "" {
			continue
		}
		// Keyed by content so the same item discovered twice does not queue
		// twice — which the file version could not do.
		key := fmt.Sprintf("%x-%d", hash(item), i)
		if err := loopwasm.ShortSet(pendingGroup, key, item, 0); err != nil {
			return err
		}
	}
	return nil
}

// DrainPending takes everything in the queue and clears it.
func DrainPending() ([]string, error) {
	entries, err := loopwasm.ShortAll(pendingGroup)
	if err != nil {
		return nil, err
	}
	var items []string
	for _, e := range entries {
		if strings.TrimSpace(e.Value) == "" {
			continue
		}
		items = append(items, e.Value)
		// Cleared one at a time: a crash mid-drain leaves the rest queued
		// rather than losing all of them.
		_ = loopwasm.ShortForget(pendingGroup, e.Key)
	}
	return items, nil
}

// RequeuePending puts items back after a failure, so they are not lost.
func RequeuePending(items []string) { _ = EnqueuePending(items) }

// hash is FNV-1a, for stable keys without pulling in a dependency.
func hash(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
