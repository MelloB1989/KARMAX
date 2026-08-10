// Package loopwasm is the SDK for writing a KARMAX loop as a WASM module.
//
// A loop built with this runs sandboxed: no filesystem, no environment, no
// sockets, and no host function it did not declare in its manifest. That is the
// point — it means someone can install your loop without reading it first.
//
// Build one with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o loop.wasm .
//
// then sign it with `karmax loops sign`.
//
//	//go:wasmexport run
//	func run() {
//	    hits, _ := loopwasm.Recall("what did we agree with the vendor", 5)
//	    loopwasm.Notify("Vendor", strings.Join(hits, "\n"))
//	}
//
// The package compiles on a normal machine too, where every call returns
// ErrNotInWASM — so the logic can be unit tested without a wasm toolchain.
package loopwasm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotInWASM is returned when a host call is attempted outside KARMAX.
var ErrNotInWASM = errors.New("loopwasm: not running inside KARMAX")

// Errors the host returns. A refusal is distinguishable from a failure, so a
// loop can say "I was not allowed to do that" rather than "it did not work".
var (
	// ErrNotDeclared means the manifest did not ask for this host function.
	ErrNotDeclared = errors.New("loopwasm: this loop did not declare that capability in its manifest")
	// ErrNotPermitted means the operator has not granted it.
	ErrNotPermitted = errors.New("loopwasm: the operator has not granted that capability")
	// ErrBufferTooSmall means the response did not fit; retry with more room.
	ErrBufferTooSmall = errors.New("loopwasm: the response did not fit in the buffer")
)

const (
	fnLog      = "log"
	fnRecall   = "recall"
	fnRemember = "remember"
	fnNotify   = "notify"
	fnHTTP     = "http"
	fnAsk      = "ask"
)

// initialBuffer is where a response is read into. Grown and retried when the
// host says it did not fit, so a large answer is not an error.
const initialBuffer = 32 << 10

// Log writes a line to KARMAX's log, attributed to this loop.
func Log(format string, args ...any) {
	_, _ = request(fnLog, fmt.Sprintf(format, args...))
}

// Recall returns memories matching a query.
func Recall(query string, limit int) ([]string, error) {
	req, err := json.Marshal(map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	out, err := request(fnRecall, string(req))
	if err != nil {
		return nil, err
	}
	var res struct {
		Hits []string `json:"hits"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// Remember stores a durable fact.
func Remember(fact string) error {
	req, err := json.Marshal(map[string]any{"fact": fact})
	if err != nil {
		return err
	}
	_, err = request(fnRemember, string(req))
	return err
}

// Notify sends the operator a notification.
func Notify(title, body string) error {
	req, err := json.Marshal(map[string]any{"title": title, "body": body})
	if err != nil {
		return err
	}
	_, err = request(fnNotify, string(req))
	return err
}

// Ask puts a question to the operator's agent, which has tools and judgement.
func Ask(prompt string) (string, error) {
	req, err := json.Marshal(map[string]any{"prompt": prompt})
	if err != nil {
		return "", err
	}
	out, err := request(fnAsk, string(req))
	if err != nil {
		return "", err
	}
	var res struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res.Answer, nil
}

// Response is the result of an HTTP call.
type Response struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// HTTP makes a request, which must be to a host the manifest declared.
func HTTP(method, url string, headers map[string]string, body string) (*Response, error) {
	req, err := json.Marshal(map[string]any{
		"method": method, "url": url, "headers": headers, "body": body,
	})
	if err != nil {
		return nil, err
	}
	out, err := request(fnHTTP, string(req))
	if err != nil {
		return nil, err
	}
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// request performs one host call, growing the buffer if the answer did not fit.
func request(name, payload string) ([]byte, error) {
	size := initialBuffer
	for attempt := 0; attempt < 4; attempt++ {
		out := make([]byte, size)
		n := hostCall(name, payload, out)
		switch {
		case n >= 0:
			return out[:n], nil
		case n == -2:
			return nil, ErrNotDeclared
		case n == -3:
			return nil, ErrNotPermitted
		case n == -4:
			size *= 8
			continue
		case n == -5:
			return nil, fmt.Errorf("loopwasm: %s rejected the request as malformed", name)
		default:
			return nil, fmt.Errorf("loopwasm: %s failed", name)
		}
	}
	return nil, ErrBufferTooSmall
}

// --- The rest of the surface -------------------------------------------------
//
// Everything below executes host-side. The guest is orchestration glue, which
// is why it can be sandboxed without losing capability: a loop that needs the
// shell asks the harness for it, and the harness is not in here.

const (
	fnConfig      = "config"
	fnHostTool    = "hosttool"
	fnHarness     = "harness"
	fnGateway     = "gateway"
	fnSummarize   = "summarize"
	fnPropose     = "propose"
	fnRemind      = "remind"
	fnSendWA      = "send_whatsapp"
	fnReadWA      = "read_whatsapp"
	fnShortSet    = "short_set"
	fnShortGet    = "short_get"
	fnShortAll    = "short_all"
	fnChatGet     = "chat_summary_get"
	fnChatSave    = "chat_summary_save"
	fnRunLoop     = "run_loop"
	fnTrigger     = "trigger"
	fnShortForget = "short_forget"
	fnOperators   = "operator_chats"
	fnMonitored   = "monitored_chats"
	fnWAChats     = "wa_chats"
	fnWAMessages  = "wa_messages"
	fnGChatSpaces = "gchat_spaces"
)

func scalar(fn, payload string) (string, error) {
	out, err := request(fn, payload)
	if err != nil {
		return "", err
	}
	var res struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res.Value, nil
}

func prompted(fn, prompt string) (string, error) {
	req, err := json.Marshal(map[string]any{"prompt": prompt})
	if err != nil {
		return "", err
	}
	out, err := request(fn, string(req))
	if err != nil {
		return "", err
	}
	var res struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res.Answer, nil
}

// Config returns an install-time setting, or "".
func Config(key string) string {
	v, _ := scalar(fnConfig, key)
	return v
}

// HostTool resolves where a host binary lives — "wacli", "gws", "karmax".
//
// It returns a PATH, not permission to run it: a sandboxed loop cannot exec.
// The path is for naming the tool inside a Harness prompt, where the harness
// runs it host-side.
func HostTool(name string) string {
	v, _ := scalar(fnHostTool, name)
	return v
}

// Harness runs a prompt through a coding harness with shell, file and web
// tools. Use it for research and for anything needing the command line.
func Harness(prompt string) (string, error) { return prompted(fnHarness, prompt) }

// Gateway asks the main model directly — no agent loop. Cheapest path.
//
// lend names host tools to make available for this call only: "wacli" gives the
// model read-only WhatsApp access. A loop cannot supply a tool of its own — the
// host owns what those tools may do, so the allowlist is one rule rather than
// one per loop.
func Gateway(prompt string, lend ...string) (string, error) {
	req, err := json.Marshal(map[string]any{"prompt": prompt, "lend": lend})
	if err != nil {
		return "", err
	}
	out, err := request(fnGateway, string(req))
	if err != nil {
		return "", err
	}
	var res struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res.Answer, nil
}

// Summarize uses the cheap summary model for bulk distillation.
func Summarize(prompt string) (string, error) { return prompted(fnSummarize, prompt) }

// Propose asks the operator to approve an action before it happens. Use this,
// never Notify, for anything that is a decision, a commitment, or money.
func Propose(title, summary, action string) error {
	req, err := json.Marshal(map[string]any{"Title": title, "Summary": summary, "Action": action})
	if err != nil {
		return err
	}
	_, err = request(fnPropose, string(req))
	return err
}

// Remind puts something on the operator's list, for things only they can do.
func Remind(title, due, notes string) error {
	req, err := json.Marshal(map[string]any{"Title": title, "Due": due, "Notes": notes})
	if err != nil {
		return err
	}
	_, err = request(fnRemind, string(req))
	return err
}

// SendWhatsApp sends a message as the operator. replyTo threads it onto an
// existing message; pass "" for a plain send.
func SendWhatsApp(to, text, replyTo string) error {
	req, err := json.Marshal(map[string]any{"To": to, "Text": text, "reply_to": replyTo})
	if err != nil {
		return err
	}
	_, err = request(fnSendWA, string(req))
	return err
}

// ReadWhatsApp returns recent messages. The result arrives already fenced as
// untrusted content, because whoever wrote it is not the operator.
func ReadWhatsApp(chat string, limit int) (string, error) {
	req, err := json.Marshal(map[string]any{"chat": chat, "limit": limit})
	if err != nil {
		return "", err
	}
	out, err := request(fnReadWA, string(req))
	if err != nil {
		return "", err
	}
	var res struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res.Text, nil
}

// ShortMemory is one short-term working note.
type ShortMemory struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ShortSet stores working state. ttlSeconds <= 0 never expires.
//
// Seconds rather than a Duration: a guest and a host do not share a time
// package, and an int is the same on both sides.
func ShortSet(group, key, value string, ttlSeconds int) error {
	req, err := json.Marshal(map[string]any{
		"Group": group, "Key": key, "Value": value, "ttl_seconds": ttlSeconds})
	if err != nil {
		return err
	}
	_, err = request(fnShortSet, string(req))
	return err
}

// ShortGet reads working state.
func ShortGet(group, key string) (string, bool, error) {
	req, err := json.Marshal(map[string]any{"Group": group, "Key": key})
	if err != nil {
		return "", false, err
	}
	out, err := request(fnShortGet, string(req))
	if err != nil {
		return "", false, err
	}
	var res struct {
		Value string `json:"value"`
		Found bool   `json:"found"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", false, err
	}
	return res.Value, res.Found, nil
}

// ShortAll returns every live note in a group.
func ShortAll(group string) ([]ShortMemory, error) {
	req, err := json.Marshal(map[string]any{"Group": group})
	if err != nil {
		return nil, err
	}
	out, err := request(fnShortAll, string(req))
	if err != nil {
		return nil, err
	}
	var res struct {
		Entries []ShortMemory `json:"entries"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}

// ChatSummary is the stored cold-memory record for one chat.
type ChatSummary struct {
	ChatJID         string `json:"jid"`
	ChatName        string `json:"name"`
	IsGroup         bool   `json:"is_group"`
	Summary         string `json:"summary"`
	MessageCount    int    `json:"message_count"`
	OwnMessageCount int    `json:"own_count"`
	// Unix seconds, because a guest and a host do not share a time.Time.
	LastMessageAt int64  `json:"last_message_at"`
	SummarizedAt  int64  `json:"summarized_at"`
	Status        string `json:"status"`
}

// GetChatSummary reads a stored per-chat summary; nil when there is none.
func GetChatSummary(jid string) (*ChatSummary, error) {
	req, err := json.Marshal(map[string]any{"JID": jid})
	if err != nil {
		return nil, err
	}
	out, err := request(fnChatGet, string(req))
	if err != nil {
		return nil, err
	}
	var res struct {
		Found   bool         `json:"found"`
		Summary *ChatSummary `json:"summary"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	if !res.Found {
		return nil, nil
	}
	return res.Summary, nil
}

// SaveChatSummary stores a per-chat summary.
func SaveChatSummary(c ChatSummary) error {
	req, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = request(fnChatSave, string(req))
	return err
}

// RunLoop triggers another loop by name.
func RunLoop(name string) error {
	_, err := request(fnRunLoop, name)
	return err
}

// TriggerInfo reports what started this run.
type TriggerInfo struct {
	Loop    string         `json:"loop"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload"`
}

// Trigger reports what started this run — "schedule", "event", "webhook",
// "timer" or "manual", with whatever payload came with it.
func Trigger() TriggerInfo {
	out, err := request(fnTrigger, "")
	if err != nil {
		return TriggerInfo{}
	}
	var t TriggerInfo
	if err := json.Unmarshal(out, &t); err != nil {
		return TriggerInfo{}
	}
	return t
}

// ShortForget drops one short-term note.
func ShortForget(group, key string) error {
	req, err := json.Marshal(map[string]any{"Group": group, "Key": key})
	if err != nil {
		return err
	}
	_, err = request(fnShortForget, string(req))
	return err
}

// OperatorChats lists the chats that belong to the operator themselves, so a
// loop can tell a command from a third party's message.
//
// A compiled-in loop read WHATSAPP_OPERATOR_CHATS from the environment. The
// sandbox has no environment, and that is the right outcome: the daemon's
// environment holds considerably more than this, and a loop asking for exactly
// what it needs is better than a loop reading whatever is lying around.
func OperatorChats() []string {
	out, err := request(fnOperators, "")
	if err != nil {
		return nil
	}
	var res struct {
		Chats []string `json:"chats"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil
	}
	return res.Chats
}

// MonitoredChats lists the chats KARMAX is watching, excluding the operator's
// own. The host fetches it — a loop cannot reach wacli's API itself, since that
// API can send messages as the operator.
func MonitoredChats() ([]string, error) {
	out, err := request(fnMonitored, "")
	if err != nil {
		return nil, err
	}
	var res struct {
		Chats []string `json:"chats"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Chats, nil
}

// WhatsAppChats returns the raw JSON of the operator's chats.
func WhatsAppChats(limit int) (string, error) {
	req, _ := json.Marshal(map[string]any{"limit": limit})
	return rawOf(fnWAChats, string(req))
}

// WhatsAppMessages returns the raw JSON of a chat's messages.
//
// Structured rather than formatted, because callers parse it. A compiled-in
// loop ran `wacli messages` itself; a sandboxed one asks the host for a NAMED
// read instead, which is a capability the operator can grant or withhold.
func WhatsAppMessages(chat string, limit int, fromMeOnly bool) (string, error) {
	req, _ := json.Marshal(map[string]any{
		"chat": chat, "limit": limit, "from_me_only": fromMeOnly})
	return rawOf(fnWAMessages, string(req))
}

// GoogleChatSpaces returns the raw JSON of the operator's Google Chat spaces.
func GoogleChatSpaces() (string, error) { return rawOf(fnGChatSpaces, "") }

// rawOf returns the tool's output AND its error together.
//
// Both, because for these tools the output is the diagnosis: gws exits non-zero
// with a JSON body explaining that Google needs an interactive reauth, and a
// loop can only tell the operator what to do if it can read that.
func rawOf(fn, payload string) (string, error) {
	out, err := request(fn, payload)
	if err != nil {
		return "", err
	}
	var res struct {
		Raw   string `json:"raw"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	if res.Error != "" {
		return res.Raw, errors.New(res.Error)
	}
	return res.Raw, nil
}
