package config

import (
	"fmt"
	"regexp"
	"strings"
)

// A tool's class says what the KARMAX gateway does with a call to it: a read
// runs at once, a write waits for the orchestrator's approval, a deny is
// refused.
const (
	ToolRead  = "read"
	ToolWrite = "write"
	ToolDeny  = "deny"
)

// alwaysDenied are the tools no agent reaches through fleetd, whatever
// karmax.tools says: a shell, files and raw HTTP on KARMAX's host, its
// harness, tasks, schedules and proposals, the org's operations, writes to
// memory (the orchestrator alone writes it), credentials and secrets, the
// fleet itself, and anything that runs code or starts an agent (a coding
// harness, a sandbox, a subagent, the orchestrator). A trailing * is a
// prefix, a leading * a suffix.
var alwaysDenied = []string{
	"shell.exec", "file.*", "http.request", "harness.*", "task.*", "scheduler.*",
	"proposal.*", "orgops*", "memory.ingest", "memory.forget",
	"claude_code.call", "codex.call", "subagent.spawn", "sandbox.start", "orchestrator.send",
	"*.exec", "*.run", "*.spawn",
}

// knownReads are KARMAX's tools that only read but whose names do not say
// so: every read is free, so they run without an approval unless
// karmax.tools says otherwise. Checked against the tools' own descriptions.
var knownReads = map[string]bool{
	"activity.recent": true, "cost.report": true, "karmax.capabilities": true, "tools.load": true,
	"slack.channels": true, "slack.user": true, "google.chat.spaces": true,
	"instagram.inbox": true, "instagram.reads": true, "instagram.commenters": true, "instagram.call": true,
	"keka.departments": true, "keka.employees": true, "keka.leave": true,
	// wacli's: status, a chat's media and the like. Not whatsapp_group_participants
	// (it adds and removes people), whatsapp_group_invite_link (it can reset the
	// link) or whatsapp_sync.
	"whatsapp_status": true, "whatsapp_logs": true, "whatsapp_message_receipts": true,
	"whatsapp_webhook_deliveries": true, "whatsapp_group_info": true, "whatsapp_call_status": true,
	"whatsapp_check_numbers": true, "whatsapp_resolve": true, "whatsapp_download_media": true,
}

// deniedWords are parts of a name that deny a tool wherever they appear.
var deniedWords = []string{"credential", "secret", "fleet"}

// readVerbs name a tool that only reads, as any word of its name but the
// first (google.mail.search, notion.query_database, whatsapp_list_chats).
var readVerbs = map[string]bool{
	"list": true, "get": true, "read": true, "search": true, "view": true, "show": true,
	"find": true, "recall": true, "retrieve": true, "monitored": true, "lookup": true,
	"query": true, "history": true, "status": true, "schema": true, "describe": true,
	"count": true,
}

// writeVerbs make a tool a write wherever they appear in its name, a read
// verb or not (whatsapp_set_chat_locked, slack.history_delete).
var writeVerbs = map[string]bool{
	"send": true, "set": true, "delete": true, "create": true, "edit": true, "join": true,
	"leave": true, "rename": true, "place": true, "answer": true, "reject": true, "end": true,
	"test": true, "replay": true, "bulk": true, "add": true, "update": true, "post": true,
	"remove": true, "start": true, "stop": true, "sync": true, "write": true, "upload": true,
	"invite": true, "reply": true, "forward": true, "share": true, "move": true, "copy": true,
	"put": true, "patch": true, "modify": true, "change": true, "mark": true, "archive": true,
	"approve": true, "accept": true, "decline": true, "cancel": true, "revoke": true, "reset": true,
	"enable": true, "disable": true, "lock": true, "unlock": true, "pin": true, "mute": true,
	"block": true, "kick": true, "ban": true, "promote": true, "demote": true, "assign": true,
	"merge": true, "close": true, "submit": true, "publish": true, "schedule": true, "book": true,
	"pay": true, "transfer": true, "save": true, "insert": true, "upsert": true, "clear": true,
	"purge": true, "grant": true, "subscribe": true, "unsubscribe": true, "react": true,
	"comment": true, "notify": true, "trigger": true, "call": true, "push": true, "import": true,
}

// toolName is a KARMAX tool's name: dotted parts, nothing that would make it
// another path or a query on KARMAX's API.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// ValidToolName reports whether a name could be a KARMAX tool's.
func ValidToolName(name string) bool { return toolName.MatchString(name) }

// globMatch matches a pattern: exact, a prefix ending in *, or (the
// always-denied list only) a suffix starting with *.
func globMatch(pattern, name string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	if p, ok := strings.CutPrefix(pattern, "*"); ok {
		return strings.HasSuffix(name, p)
	}
	return pattern == name
}

// AlwaysDenied reports whether a tool is one no entry in karmax.tools opens.
func AlwaysDenied(name string) bool {
	n := strings.ToLower(name)
	for _, p := range alwaysDenied {
		if globMatch(p, n) {
			return true
		}
	}
	for _, w := range deniedWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// ToolClass is what the gateway does with a call to the named tool: the
// always-denied list first, then the most specific karmax.tools entry (an
// exact name, else the longest prefix*), then the known reads, then the name
// itself — a read verb in it and no write verb reads, anything else writes.
// A name that is not a tool's is denied.
func (k Karmax) ToolClass(name string) string {
	if !ValidToolName(name) || AlwaysDenied(name) {
		return ToolDeny
	}
	if c, ok := k.Tools[name]; ok {
		return c
	}
	best, class := -1, ""
	for p, c := range k.Tools {
		if strings.HasSuffix(p, "*") && globMatch(p, name) && len(p) > best {
			best, class = len(p), c
		}
	}
	if best >= 0 {
		return class
	}
	if knownReads[name] {
		return ToolRead
	}
	// Words split on dots and underscores alike, so wacli's whatsapp_list_chats
	// reads as KARMAX's own google.mail.search does; the first is the namespace.
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '_' })
	read := false
	for i, w := range words {
		if i == 0 {
			continue
		}
		if writeVerbs[w] {
			return ToolWrite
		}
		read = read || readVerbs[w]
	}
	if read {
		return ToolRead
	}
	return ToolWrite
}

// validateTools checks karmax.tools: each key a tool name or prefix*, each
// class read, write or deny, and no entry that would open an always-denied
// tool.
func (k Karmax) validateTools() error {
	for p, c := range k.Tools {
		if c != ToolRead && c != ToolWrite && c != ToolDeny {
			return fmt.Errorf("karmax: tools: %s: %q is not read, write or deny", p, c)
		}
		name := strings.TrimSuffix(p, "*")
		if name == "" || !ValidToolName(strings.TrimSuffix(name, ".")) {
			return fmt.Errorf("karmax: tools: %q is not a tool name or a prefix ending in *", p)
		}
		if c != ToolDeny && !strings.HasSuffix(p, "*") && AlwaysDenied(p) {
			return fmt.Errorf("karmax: tools: %s is always denied to agents; no entry opens it", p)
		}
	}
	return nil
}
