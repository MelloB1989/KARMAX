package googleacct

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/sources/gmail"
	"github.com/MelloB1989/karmax/internal/tools"
)

// The Gmail actions, as the operator: read a message or thread, reply in its
// thread, send a new email, and label (archive, mark read). There is no
// delete or trash: deleting the operator's mail is on the hard floor.

func gmailTools(b base) []tools.Tool {
	return []tools.Tool{&mailReadTool{b}, &mailReplyTool{b}, &mailSendTool{b}, &mailLabelTool{b}}
}

func gmailToolNames() []string {
	return []string{"gmail.read", "gmail.reply", "gmail.send", "gmail.label"}
}

// sentTo names where an email went, as the operator would say it: its
// recipients and its subject.
func sentTo(s SentMail) string {
	who := strings.Join(append(append([]string{}, s.To...), s.Cc...), ", ")
	if s.Subject == "" {
		return who
	}
	return who + " (“" + s.Subject + "”)"
}

const mailIDParams = `"message_id": {"type": "string", "description": "The message, e.g. the gmail event's data.message."},
				"thread_id": {"type": "string", "description": "The thread, e.g. the gmail event's data.thread."}`

// --- gmail.read

type mailReadTool struct{ base }

func (t *mailReadTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gmail.read",
		Description: "Read an email, or a whole thread, with its text, from one of the operator's Gmail accounts. A gmail " +
			"event carries only the sender, subject and Gmail's snippet; read the body here before answering or acting " +
			"on it. Pass message_id for one message or thread_id for the conversation (both are in the event's data).",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				` + mailIDParams + `,
				"max_chars": {"type": "integer", "description": "Longest text kept per message (default 6000, at most 20000)."}
			}
		}`),
	}
}

// threadBudget bounds a whole thread's text.
const threadBudget = 24000

func clip(s string, n int) (string, bool) {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s, false
	}
	return string(r[:n]) + "\n…[truncated]", true
}

func (t *mailReadTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	per := 6000
	switch v := in["max_chars"].(type) {
	case float64:
		if v > 0 {
			per = int(v)
		}
	case int:
		if v > 0 {
			per = v
		}
	}
	if per > 20000 {
		per = 20000
	}
	var mails []Mail
	switch id, thread := str(in, "message_id"), str(in, "thread_id"); {
	case id != "":
		var m Mail
		m, err = t.c.ReadMail(ctx, account, id)
		mails = []Mail{m}
	case thread != "":
		mails, err = t.c.ReadThread(ctx, account, thread)
	default:
		err = fmt.Errorf("say what to read: a message_id or a thread_id")
	}
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	truncated, budget := false, threadBudget
	for i := range mails {
		n := per
		if n > budget {
			n = budget
		}
		var cut bool
		mails[i].Body, cut = clip(mails[i].Body, n)
		truncated = truncated || cut
		budget -= len([]rune(mails[i].Body))
		if budget < 200 {
			budget = 200 // the latest messages still say something
		}
	}
	out := map[string]any{"account": account, "messages": mails}
	if truncated {
		out["truncated"] = true
	}
	return tools.SuccessResult(out), nil
}

// --- gmail.reply

type mailReplyTool struct{ base }

func (t *mailReplyTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gmail.reply",
		Description: "Reply to an email AS THE OPERATOR, from the Gmail account it arrived on, inside its thread (Gmail " +
			"shows it as part of the conversation). Pass the message_id answered (or the thread_id, to answer its latest " +
			"message). It goes to whoever the message asks replies to go to; reply_all also writes to everyone else on " +
			"it. Plain text, in the operator's own voice. The operator is told afterwards what was sent.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				` + mailIDParams + `,
				"text": {"type": "string", "description": "The reply, plain text. The quoted original is not added."},
				"reply_all": {"type": "boolean", "description": "Also write to everyone else on the message (default false)."},
				"cc": {"type": "array", "items": {"type": "string"}, "description": "More addresses to copy."}
			},
			"required": ["text"]
		}`),
	}
}

func (t *mailReplyTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	sent, err := t.c.ReplyMail(ctx, account, MailReply{MessageID: str(in, "message_id"), ThreadID: str(in, "thread_id"),
		Body: strings.TrimSpace(str(in, "text")), All: boolOr(in, "reply_all", false), Cc: strList(in["cc"])})
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	t.sent.Record(tools.SentAs{Service: gmail.Name, Account: account, To: sentTo(sent), Text: str(in, "text"),
		URL: gmail.URL(account, sent.ThreadID)})
	return tools.SuccessResult(map[string]any{"sent_as": account, "message": sent.ID, "thread": sent.ThreadID,
		"to": sent.To, "cc": sent.Cc, "subject": sent.Subject}), nil
}

// --- gmail.send

type mailSendTool struct{ base }

func (t *mailSendTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gmail.send",
		Description: "Send a new email AS THE OPERATOR from one of their Gmail accounts. To answer an email, use " +
			"gmail.reply instead, which keeps the thread. Plain text, in the operator's own voice. The operator is " +
			"told afterwards what was sent.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				"to": {"type": "array", "items": {"type": "string"}, "description": "Recipients' addresses."},
				"cc": {"type": "array", "items": {"type": "string"}},
				"bcc": {"type": "array", "items": {"type": "string"}},
				"subject": {"type": "string"},
				"text": {"type": "string", "description": "The email, plain text."}
			},
			"required": ["to", "subject", "text"]
		}`),
	}
}

func (t *mailSendTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if str(in, "subject") == "" {
		return tools.ErrorResult(fmt.Errorf("a new email needs a subject")), nil
	}
	sent, err := t.c.SendMail(ctx, account, Outgoing{To: strList(in["to"]), Cc: strList(in["cc"]), Bcc: strList(in["bcc"]),
		Subject: str(in, "subject"), Body: strings.TrimSpace(str(in, "text"))})
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	t.sent.Record(tools.SentAs{Service: gmail.Name, Account: account, To: sentTo(sent), Text: str(in, "text"),
		URL: gmail.URL(account, sent.ThreadID)})
	return tools.SuccessResult(map[string]any{"sent_as": account, "message": sent.ID, "thread": sent.ThreadID,
		"to": sent.To, "cc": sent.Cc, "subject": sent.Subject}), nil
}

// --- gmail.label

type mailLabelTool struct{ base }

func (t *mailLabelTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "gmail.label",
		Description: "Organise the operator's mail: add or remove labels (by name; one that does not exist is made), " +
			"archive (out of the inbox) and mark read or unread — on one message or a whole thread. It cannot trash, " +
			"delete or mark spam: those are the operator's call, so ask them.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				` + accountParam + `,
				` + mailIDParams + `,
				"add": {"type": "array", "items": {"type": "string"}, "description": "Labels to add, e.g. STARRED or a label of the operator's."},
				"remove": {"type": "array", "items": {"type": "string"}, "description": "Labels to remove."},
				"archive": {"type": "boolean", "description": "true takes it out of the inbox; false puts it back."},
				"mark_read": {"type": "boolean", "description": "true marks it read; false marks it unread."}
			}
		}`),
	}
}

func (t *mailLabelTool) Execute(ctx context.Context, in map[string]any) (tools.ToolResult, error) {
	account, err := t.account(ctx, in)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	add, remove := strList(in["add"]), strList(in["remove"])
	if v, ok := in["archive"].(bool); ok {
		if v {
			remove = append(remove, "INBOX")
		} else {
			add = append(add, "INBOX")
		}
	}
	if v, ok := in["mark_read"].(bool); ok {
		if v {
			remove = append(remove, "UNREAD")
		} else {
			add = append(add, "UNREAD")
		}
	}
	target := MailTarget{MessageID: str(in, "message_id"), ThreadID: str(in, "thread_id")}
	labels, err := t.c.LabelMail(ctx, account, target, add, remove)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	out := map[string]any{"account": account, "added": add, "removed": remove}
	if labels != nil {
		out["labels"] = labels
	}
	return tools.SuccessResult(out), nil
}
