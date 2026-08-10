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
	fnTool        = "tool"
	fnShortSet    = "short_set"
	fnShortGet    = "short_get"
	fnShortAll    = "short_all"
	fnChatGet     = "chat_summary_get"
	fnChatSave    = "chat_summary_save"
	fnRunLoop     = "run_loop"
	fnTrigger     = "trigger"
	fnShortForget = "short_forget"
	fnOperators   = "operator_chats"
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

// Tool calls one of KARMAX's tools and returns its output.
//
// This is how a loop reaches everything outside its own sandbox — WhatsApp,
// Google Workspace, GitHub, anything an integration exposes. The tool must be
// listed in the manifest's `tools:`, and the operator must have granted
// `tool:<name>`; either gate refusing is an error here rather than an empty
// result.
//
// Content that came from someone other than the operator arrives already
// fenced as untrusted, so a loop author cannot forget to do it.
func Tool(name string, input any) (string, error) {
	if input == nil {
		input = map[string]any{}
	}
	req, err := json.Marshal(map[string]any{"name": name, "input": input})
	if err != nil {
		return "", err
	}
	out, err := request(fnTool, string(req))
	if err != nil {
		return "", err
	}
	var res struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	// The output is returned alongside the error on purpose: a tool that failed
	// usually explains itself in what it printed, and "run gws auth login" is a
	// more useful thing for a loop to see than "it failed".
	if res.Error != "" {
		return res.Output, errors.New(res.Error)
	}
	return res.Output, nil
}

// ToolJSON calls a tool and decodes its output into out.
func ToolJSON(name string, input, out any) error {
	raw, err := Tool(name, input)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), out)
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

