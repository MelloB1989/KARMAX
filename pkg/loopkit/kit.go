package loopkit

import "context"

// Kit is the capability surface a loop receives when it runs. KARMAX implements
// it and passes it to Loop.Run. Loop authors depend ONLY on this interface, not
// on KARMAX's internal packages — so third-party loops stay decoupled and
// compile against just this SDK.
type Kit interface {
	// Ask runs a prompt through the operator's main agent — full toolset,
	// long-term memory, and judgement — and returns its reply. Use for tasks
	// that need reasoning or the agent's tools (sending messages, scheduling,
	// etc.). Consumes the agent's model budget, so prefer Harness for pure
	// web/text work.
	Ask(ctx context.Context, prompt string) (string, error)

	// Harness runs a prompt directly through the Claude Code CLI (web search,
	// file, and shell tools) and returns its text output. It runs on the Claude
	// subscription, independent of the main model — ideal for web research and
	// heavy work even when the main model is rate-limited.
	Harness(ctx context.Context, prompt string) (string, error)

	// Remember stores a durable, standalone fact in the operator's long-term
	// memory (tagged with the loop name).
	Remember(fact string) error

	// Recall returns up to limit memory snippets semantically matching query.
	Recall(query string, limit int) ([]string, error)

	// Notify sends a notification to the operator's phone app: it is saved to
	// the in-app feed AND delivered as a push (so it survives a missed push).
	Notify(title, body string) error

	// SendWhatsApp sends a message through the operator's connected WhatsApp
	// account to target (a phone number, contact name, or chat JID). Use for
	// loops that deliver to a specific recipient rather than the app.
	SendWhatsApp(ctx context.Context, target, content string) error

	// ReadWhatsApp returns recent WhatsApp messages as formatted text. Pass a
	// chat name or JID, or "" for the most recent messages across chats.
	ReadWhatsApp(ctx context.Context, chat string, limit int) (string, error)

	// HTTP performs an HTTP request and returns the response body, status code,
	// and any transport error. Headers and body may be empty.
	HTTP(ctx context.Context, method, url string, headers map[string]string, body string) (string, int, error)

	// Trigger reports what fired this run (schedule / webhook / manual) and any
	// payload — e.g. for a webhook, Payload["body"] holds the request body.
	Trigger() Trigger

	// Config returns an install-time configuration value for this loop (for
	// example an API key the operator entered when installing it), or "" if the
	// key is unset.
	Config(key string) string

	// Logf writes a line to KARMAX's logs, prefixed with the loop's name.
	Logf(format string, args ...any)

	// RunLoop triggers another registered loop by name (manual trigger). Lets a
	// loop hand work to a dedicated loop rather than doing it inline.
	RunLoop(name string) error
}
