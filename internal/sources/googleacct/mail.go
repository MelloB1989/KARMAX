package googleacct

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/quotedprintable"
	netmail "net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gmail"
)

// Gmail through the CLIs: the reads the Gmail source makes (gmail.API) and
// the actions KARMAX takes as the operator.
//
// gws speaks the raw API, which can mask a message down to its headers and
// attachment names, so the source never pulls a body. gog's `gmail raw`
// returns the API's own message too; its history names messages without
// their labels, and it threads a reply itself.

// gmailMessage is a Gmail message as the API (gws, and gog's raw) writes it.
type gmailMessage struct {
	ID           string    `json:"id"`
	ThreadID     string    `json:"threadId"`
	HistoryID    string    `json:"historyId"`
	LabelIDs     []string  `json:"labelIds"`
	Snippet      string    `json:"snippet"`
	InternalDate string    `json:"internalDate"`
	Payload      gmailPart `json:"payload"`
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Data string `json:"data"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

func (m gmailMessage) message() gmail.Message {
	out := gmail.Message{ID: m.ID, ThreadID: m.ThreadID, Labels: m.LabelIDs, Snippet: m.Snippet}
	if ms, err := strconv.ParseInt(m.InternalDate, 10, 64); err == nil && ms > 0 {
		out.At = time.UnixMilli(ms)
	}
	for _, h := range m.Payload.Headers {
		out.Headers = append(out.Headers, gmail.Header{Name: h.Name, Value: h.Value})
	}
	out.Attachments = m.Payload.attachments()
	return out
}

func (p gmailPart) attachments() []string {
	var out []string
	if f := strings.TrimSpace(p.Filename); f != "" {
		out = append(out, f)
	}
	for _, sub := range p.Parts {
		out = append(out, sub.attachments()...)
	}
	return out
}

// find is the first part of a MIME type, depth first, with content.
func (p gmailPart) find(mimeType string) (string, bool) {
	if strings.HasPrefix(strings.ToLower(p.MimeType), mimeType) && p.Filename == "" && p.Body.Data != "" {
		if s, ok := decodeBase64URL(p.Body.Data); ok {
			return s, true
		}
	}
	for _, sub := range p.Parts {
		if s, ok := sub.find(mimeType); ok {
			return s, true
		}
	}
	return "", false
}

func decodeBase64URL(s string) (string, bool) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b), true
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return string(b), true
	}
	return "", false
}

var (
	htmlDropped = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)\s*>`)
	htmlBreaks  = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6]|/blockquote)\b[^>]*>`)
	htmlTags    = regexp.MustCompile(`(?s)<[^>]*>`)
	blankLines  = regexp.MustCompile(`\n{3,}`)
)

// textOfHTML is what an HTML body says, without its markup.
func textOfHTML(s string) string {
	s = htmlDropped.ReplaceAllString(s, "")
	s = htmlBreaks.ReplaceAllString(s, "\n")
	s = htmlTags.ReplaceAllString(s, "")
	s = strings.ReplaceAll(html.UnescapeString(s), " ", " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// text is a message's body as text: the plain part when there is one, else
// the HTML part without its markup.
func (m gmailMessage) text() string {
	if s, ok := m.Payload.find("text/plain"); ok {
		return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	}
	if s, ok := m.Payload.find("text/html"); ok {
		return textOfHTML(s)
	}
	return ""
}

// headersOnly masks a full message down to what the source may read: labels,
// snippet, headers and the parts' names — no part's content.
const headersOnly = "id,threadId,labelIds,snippet,historyId,internalDate," +
	"payload(mimeType,filename,headers,parts(mimeType,filename,parts(mimeType,filename,parts(mimeType,filename,parts(mimeType,filename)))))"

// metadataHeaders are the headers asked for when a login may read nothing
// but metadata.
var metadataHeaders = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "In-Reply-To", "References",
	"Reply-To", "List-Unsubscribe", "List-Id", "Precedence", "Auto-Submitted", "X-Autoreply", "X-Autorespond", "Return-Path"}

func me(extra map[string]any) map[string]any {
	out := map[string]any{"userId": "me"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func goneAs(err, as error) error {
	var ce *cliError
	if errors.As(err, &ce) && ce.gone {
		return as
	}
	return err
}

// Start is the account's current history id (gmail.API).
func (c *Client) Start(ctx context.Context, account string) (string, error) {
	var id string
	err := c.with(ctx, account, GmailRead, func(l Login) error {
		if l.Path == Gws {
			var p struct {
				HistoryID string `json:"historyId"`
			}
			err := c.gws(ctx, l, "gmail profile", &p, "gmail", "users", "getProfile", "--params", jsonArg(me(nil)))
			id = p.HistoryID
			return err
		}
		// gog has no profile: the newest message's history id is where the
		// mailbox stands.
		var listed struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		if err := c.gog(ctx, l, "gmail search", &listed, "gmail", "messages", "search", "in:anywhere", "--max", "1"); err != nil {
			return err
		}
		if len(listed.Messages) == 0 {
			return fmt.Errorf("gmail %s: the mailbox has no message to start watching from", account)
		}
		var m gmailMessage
		err := c.gog(ctx, l, "gmail raw", &m, "gmail", "raw", listed.Messages[0].ID, "--format", "minimal")
		id = m.HistoryID
		return err
	})
	if err == nil && id == "" {
		err = fmt.Errorf("gmail %s: Google gave no history id", account)
	}
	return id, err
}

// History lists the inbox messages added since start (gmail.API).
func (c *Client) History(ctx context.Context, account, start string) (gmail.Page, error) {
	var page gmail.Page
	err := c.with(ctx, account, GmailRead, func(l Login) error {
		page = gmail.Page{}
		seen := map[string]bool{}
		add := func(r gmail.Ref) {
			if r.ID != "" && !seen[r.ID] {
				seen[r.ID] = true
				page.Added = append(page.Added, r)
			}
		}
		if l.Path == Gws {
			return c.gwsPages(ctx, l, "gmail history list", func(raw []byte) error {
				var p struct {
					History []struct {
						MessagesAdded []struct {
							Message struct {
								ID       string   `json:"id"`
								ThreadID string   `json:"threadId"`
								LabelIDs []string `json:"labelIds"`
							} `json:"message"`
						} `json:"messagesAdded"`
					} `json:"history"`
					HistoryID string `json:"historyId"`
				}
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				for _, h := range p.History {
					for _, a := range h.MessagesAdded {
						labels := a.Message.LabelIDs
						if labels == nil {
							labels = []string{}
						}
						add(gmail.Ref{ID: a.Message.ID, ThreadID: a.Message.ThreadID, Labels: labels})
					}
				}
				if p.HistoryID != "" {
					page.Next = p.HistoryID
				}
				return nil
			}, "gmail", "users", "history", "list", "--params", jsonArg(me(map[string]any{
				"startHistoryId": start, "historyTypes": []string{"messageAdded"}, "labelId": "INBOX", "maxResults": 500})))
		}
		var h struct {
			HistoryID string   `json:"historyId"`
			Messages  []string `json:"messages"`
		}
		if err := c.gog(ctx, l, "gmail history", &h, "gmail", "history", "--since", start, "--max", "500", "--all"); err != nil {
			return err
		}
		for _, id := range h.Messages {
			add(gmail.Ref{ID: id}) // gog does not say the labels
		}
		page.Next = h.HistoryID
		return nil
	})
	if err != nil {
		return gmail.Page{}, goneAs(err, gmail.ErrHistoryExpired)
	}
	if page.Next == "" {
		page.Next = start
	}
	return page, nil
}

// Recent lists the newest inbox messages, newest first (gmail.API). The
// inbox is asked for by label, not by search, which a login granted only
// metadata cannot do.
func (c *Client) Recent(ctx context.Context, account string, max int) ([]gmail.Ref, error) {
	var out []gmail.Ref
	err := c.with(ctx, account, GmailRead, func(l Login) error {
		out = nil
		var listed struct {
			Messages []struct {
				ID       string `json:"id"`
				ThreadID string `json:"threadId"`
			} `json:"messages"`
		}
		var err error
		if l.Path == Gws {
			err = c.gws(ctx, l, "gmail messages list", &listed, "gmail", "users", "messages", "list",
				"--params", jsonArg(me(map[string]any{"labelIds": []string{"INBOX"}, "maxResults": max})))
		} else {
			err = c.gog(ctx, l, "gmail search", &listed, "gmail", "messages", "search", "in:inbox", "--max", fmt.Sprint(max))
		}
		for _, m := range listed.Messages {
			out = append(out, gmail.Ref{ID: m.ID, ThreadID: m.ThreadID})
		}
		return err
	})
	return out, err
}

// Message reads one message's labels, headers, snippet and attachment names,
// never its body (gmail.API).
func (c *Client) Message(ctx context.Context, account, id string) (gmail.Message, error) {
	var out gmail.Message
	err := c.with(ctx, account, GmailRead, func(l Login) error {
		var m gmailMessage
		var err error
		switch {
		case l.Path == Gws && l.Can(GmailBody):
			err = c.gws(ctx, l, "gmail get", &m, "gmail", "users", "messages", "get",
				"--params", jsonArg(me(map[string]any{"id": id, "format": "full", "fields": headersOnly})))
		case l.Path == Gws:
			// Granted metadata only: the full format is refused, and the
			// attachments go unnamed.
			err = c.gws(ctx, l, "gmail get", &m, "gmail", "users", "messages", "get",
				"--params", jsonArg(me(map[string]any{"id": id, "format": "metadata", "metadataHeaders": metadataHeaders})))
		default:
			err = c.gog(ctx, l, "gmail raw", &m, "gmail", "raw", id, "--format", "metadata")
		}
		out = m.message()
		return err
	})
	if err != nil {
		return gmail.Message{}, goneAs(err, gmail.ErrGone)
	}
	return out, nil
}

// Thread lists a thread's messages with their labels (gmail.API).
func (c *Client) Thread(ctx context.Context, account, threadID string) ([]gmail.Ref, error) {
	var out []gmail.Ref
	err := c.with(ctx, account, GmailRead, func(l Login) error {
		var t struct {
			Messages []gmailMessage `json:"messages"`
		}
		var err error
		if l.Path == Gws {
			err = c.gws(ctx, l, "gmail thread", &t, "gmail", "users", "threads", "get",
				"--params", jsonArg(me(map[string]any{"id": threadID, "format": "minimal"})))
		} else {
			var wrapped struct {
				Thread struct {
					Messages []gmailMessage `json:"messages"`
				} `json:"thread"`
			}
			err = c.gog(ctx, l, "gmail thread", &wrapped, "gmail", "thread", "get", threadID)
			t.Messages = wrapped.Thread.Messages
		}
		out = nil
		for _, m := range t.Messages {
			out = append(out, gmail.Ref{ID: m.ID, ThreadID: threadID, Labels: m.LabelIDs})
		}
		return err
	})
	return out, goneAs(err, gmail.ErrGone)
}

// Mail is one message as the operator reads it.
type Mail struct {
	ID          string   `json:"id"`
	ThreadID    string   `json:"thread"`
	From        string   `json:"from,omitempty"`
	To          string   `json:"to,omitempty"`
	Cc          string   `json:"cc,omitempty"`
	Date        string   `json:"date,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
	Body        string   `json:"body"`
}

func (m gmailMessage) mail() Mail {
	g := m.message()
	return Mail{ID: m.ID, ThreadID: m.ThreadID, From: g.Header("From"), To: g.Header("To"), Cc: g.Header("Cc"),
		Date: g.Header("Date"), Subject: g.Header("Subject"), Labels: m.LabelIDs, Attachments: g.Attachments, Body: m.text()}
}

// ReadMail reads one message with its body.
func (c *Client) ReadMail(ctx context.Context, account, id string) (Mail, error) {
	var out Mail
	err := c.with(ctx, account, GmailBody, func(l Login) error {
		var m gmailMessage
		var err error
		if l.Path == Gws {
			err = c.gws(ctx, l, "gmail get", &m, "gmail", "users", "messages", "get",
				"--params", jsonArg(me(map[string]any{"id": id, "format": "full"})))
		} else {
			err = c.gog(ctx, l, "gmail raw", &m, "gmail", "raw", id, "--format", "full")
		}
		out = m.mail()
		return err
	})
	return out, goneAs(err, gmail.ErrGone)
}

// ReadThread reads a thread's messages with their bodies, oldest first.
func (c *Client) ReadThread(ctx context.Context, account, threadID string) ([]Mail, error) {
	var out []Mail
	err := c.with(ctx, account, GmailBody, func(l Login) error {
		var t struct {
			Messages []gmailMessage `json:"messages"`
		}
		var err error
		if l.Path == Gws {
			err = c.gws(ctx, l, "gmail thread", &t, "gmail", "users", "threads", "get",
				"--params", jsonArg(me(map[string]any{"id": threadID, "format": "full"})))
		} else {
			var wrapped struct {
				Thread struct {
					Messages []gmailMessage `json:"messages"`
				} `json:"thread"`
			}
			err = c.gog(ctx, l, "gmail thread", &wrapped, "gmail", "thread", "get", threadID)
			t.Messages = wrapped.Thread.Messages
		}
		out = nil
		for _, m := range t.Messages {
			out = append(out, m.mail())
		}
		return err
	})
	return out, goneAs(err, gmail.ErrGone)
}

// Outgoing is an email to send as the account.
type Outgoing struct {
	To, Cc, Bcc []string
	Subject     string
	Body        string // plain text
	// ThreadID, InReplyTo and References thread a reply; ReplyTo is the
	// message it answers, which gog threads by itself.
	ThreadID, InReplyTo, References, ReplyTo string
}

// SentMail is an email KARMAX sent.
type SentMail struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"thread"`
	To       []string `json:"to"`
	Cc       []string `json:"cc,omitempty"`
	Subject  string   `json:"subject"`
}

// addresses reads a list of recipients, each entry possibly several.
func addresses(list []string) ([]*netmail.Address, error) {
	var out []*netmail.Address
	for _, entry := range list {
		entry = oneLine(entry)
		if entry == "" {
			continue
		}
		parsed, err := netmail.ParseAddressList(entry)
		if err != nil {
			return nil, fmt.Errorf("%q is not an email address", entry)
		}
		out = append(out, parsed...)
	}
	return out, nil
}

// oneLine keeps a header value on one line, so nothing can be added after it.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func rendered(list []*netmail.Address) []string {
	var out []string
	for _, a := range list {
		if n := strings.TrimSpace(a.Name); n != "" {
			out = append(out, n+" <"+strings.ToLower(a.Address)+">")
		} else {
			out = append(out, strings.ToLower(a.Address))
		}
	}
	return out
}

func headerList(list []*netmail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

func bare(list []*netmail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.Address
	}
	return strings.Join(parts, ",")
}

// rfc5322 is the message a gws send carries: plain text, its headers each on
// one line.
func rfc5322(to, cc, bcc []*netmail.Address, o Outgoing) []byte {
	var b bytes.Buffer
	h := func(k, v string) {
		if v != "" {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	h("To", headerList(to))
	h("Cc", headerList(cc))
	h("Bcc", headerList(bcc))
	h("Subject", mime.QEncoding.Encode("utf-8", oneLine(o.Subject)))
	h("In-Reply-To", oneLine(o.InReplyTo))
	h("References", oneLine(o.References))
	h("MIME-Version", "1.0")
	h("Content-Type", `text/plain; charset="UTF-8"`)
	h("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(o.Body, "\r\n", "\n")))
	_ = qp.Close()
	return b.Bytes()
}

// SendMail sends an email as the account.
func (c *Client) SendMail(ctx context.Context, account string, o Outgoing) (SentMail, error) {
	if strings.TrimSpace(o.Body) == "" {
		return SentMail{}, fmt.Errorf("nothing to send: the email has no text")
	}
	to, err := addresses(o.To)
	if err != nil {
		return SentMail{}, err
	}
	cc, err := addresses(o.Cc)
	if err != nil {
		return SentMail{}, err
	}
	bcc, err := addresses(o.Bcc)
	if err != nil {
		return SentMail{}, err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return SentMail{}, fmt.Errorf("an email needs at least one recipient")
	}
	out := SentMail{To: rendered(to), Cc: rendered(cc), Subject: oneLine(o.Subject)}
	err = c.with(ctx, account, GmailSend, func(l Login) error {
		if l.Path == Gws {
			body := map[string]any{"raw": base64.URLEncoding.EncodeToString(rfc5322(to, cc, bcc, o))}
			if o.ThreadID != "" {
				body["threadId"] = o.ThreadID
			}
			var res gmailMessage
			err := c.gws(ctx, l, "gmail send", &res, "gmail", "users", "messages", "send",
				"--params", jsonArg(me(nil)), "--json", jsonArg(body))
			out.ID, out.ThreadID = res.ID, res.ThreadID
			return err
		}
		args := []string{"gmail", "send", "--subject", oneLine(o.Subject), "--body", o.Body}
		if len(to) > 0 {
			args = append(args, "--to", bare(to))
		}
		if len(cc) > 0 {
			args = append(args, "--cc", bare(cc))
		}
		if len(bcc) > 0 {
			args = append(args, "--bcc", bare(bcc))
		}
		if o.ReplyTo != "" {
			args = append(args, "--reply-to-message-id", o.ReplyTo)
		}
		var res struct {
			MessageID string `json:"messageId"`
			ThreadID  string `json:"threadId"`
		}
		err := c.gog(ctx, l, "gmail send", &res, args...)
		out.ID, out.ThreadID = res.MessageID, res.ThreadID
		return err
	})
	if out.ThreadID == "" {
		out.ThreadID = o.ThreadID
	}
	return out, err
}

// MailReply is an answer to a message.
type MailReply struct {
	// MessageID is the message answered; with only ThreadID, the thread's
	// latest message is.
	MessageID, ThreadID string
	Body                string
	// All answers everyone on the message, not only its sender.
	All bool
	Cc  []string
	// Own are the operator's other addresses, never written to by a reply.
	Own []string
}

// ReplyMail answers a message in its thread, as the account: to whoever it
// asks replies to go to (everyone on it, with All), threaded by In-Reply-To,
// References and the thread id.
func (c *Client) ReplyMail(ctx context.Context, account string, r MailReply) (SentMail, error) {
	id := strings.TrimSpace(r.MessageID)
	if id == "" && strings.TrimSpace(r.ThreadID) != "" {
		refs, err := c.Thread(ctx, account, r.ThreadID)
		if err != nil {
			return SentMail{}, err
		}
		for i := len(refs) - 1; i >= 0; i-- {
			if !containsLabel(refs[i].Labels, "DRAFT") {
				id = refs[i].ID
				break
			}
		}
	}
	if id == "" {
		return SentMail{}, fmt.Errorf("say which message to answer: its message id (or the thread id)")
	}
	orig, err := c.Message(ctx, account, id)
	if err != nil {
		return SentMail{}, err
	}

	own := map[string]bool{strings.ToLower(account): true}
	for _, a := range append(append([]string{}, r.Own...), c.h.Accounts(ctx).Emails()...) {
		own[strings.ToLower(strings.TrimSpace(a))] = true
	}
	parse := func(h string) []*netmail.Address {
		list, _ := netmail.ParseAddressList(h)
		return list
	}
	from := parse(orig.Header("From"))
	mine := containsLabel(orig.Labels, "SENT") || (len(from) > 0 && own[strings.ToLower(from[0].Address)])
	var to, cc []*netmail.Address
	switch {
	case mine:
		// Answering the operator's own last word continues it to the
		// people it went to.
		to = parse(orig.Header("To"))
		if r.All {
			cc = parse(orig.Header("Cc"))
		}
	default:
		if rt := parse(orig.Header("Reply-To")); len(rt) > 0 {
			to = rt
		} else {
			to = from
		}
		if r.All {
			cc = append(parse(orig.Header("To")), parse(orig.Header("Cc"))...)
		}
	}
	seen := map[string]bool{}
	keep := func(list []*netmail.Address) []*netmail.Address {
		var out []*netmail.Address
		for _, a := range list {
			k := strings.ToLower(a.Address)
			if own[k] || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, a)
		}
		return out
	}
	to, cc = keep(to), keep(cc)
	if len(to) == 0 && len(cc) > 0 {
		to, cc = cc[:1], cc[1:]
	}
	if len(to) == 0 {
		return SentMail{}, fmt.Errorf("message %s has nobody to answer but the operator", id)
	}

	subject := oneLine(orig.Header("Subject"))
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = strings.TrimSpace("Re: " + subject)
	}
	msgID := oneLine(orig.Header("Message-ID"))
	o := Outgoing{
		To: rendered(to), Cc: append(rendered(cc), r.Cc...), Subject: subject, Body: r.Body,
		ThreadID: orig.ThreadID, InReplyTo: msgID, References: strings.TrimSpace(oneLine(orig.Header("References")) + " " + msgID),
		ReplyTo: id,
	}
	return c.SendMail(ctx, account, o)
}

func containsLabel(labels []string, l string) bool {
	for _, x := range labels {
		if x == l {
			return true
		}
	}
	return false
}

// MailTarget is a message, or a whole thread.
type MailTarget struct{ MessageID, ThreadID string }

// floorLabels move mail where Google deletes it in time; applying them is
// deleting the operator's data, which is theirs to decide.
var floorLabels = map[string]bool{"TRASH": true, "SPAM": true}

// LabelMail adds and removes labels on a message or a thread, by name as the
// operator sees them; a label to add that does not exist yet is made.
// Archiving is removing INBOX, marking read removing UNREAD.
func (c *Client) LabelMail(ctx context.Context, account string, t MailTarget, add, remove []string) ([]string, error) {
	if t.MessageID == "" && t.ThreadID == "" {
		return nil, fmt.Errorf("say which message or thread to label")
	}
	for _, a := range add {
		if floorLabels[strings.ToUpper(strings.TrimSpace(a))] {
			return nil, fmt.Errorf("moving mail to %s deletes it in time, and deleting the operator's mail is theirs to decide: ask them", strings.ToLower(a))
		}
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil, fmt.Errorf("nothing to change: name labels to add or remove")
	}
	var labels []string
	err := c.with(ctx, account, GmailModify, func(l Login) error {
		if l.Path == Gog {
			args := []string{"gmail", "messages", "modify", t.MessageID}
			if t.ThreadID != "" && t.MessageID == "" {
				args = []string{"gmail", "thread", "modify", t.ThreadID}
			}
			if len(add) > 0 {
				args = append(args, "--add", strings.Join(add, ","))
			}
			if len(remove) > 0 {
				args = append(args, "--remove", strings.Join(remove, ","))
			}
			return c.gog(ctx, l, "gmail modify", nil, args...)
		}
		var listed struct {
			Labels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := c.gws(ctx, l, "gmail labels", &listed, "gmail", "users", "labels", "list", "--params", jsonArg(me(nil))); err != nil {
			return err
		}
		ids := map[string]string{}
		for _, lb := range listed.Labels {
			ids[strings.ToLower(lb.ID)] = lb.ID
			ids[strings.ToLower(lb.Name)] = lb.ID
		}
		var addIDs, removeIDs []string
		for _, name := range add {
			name = strings.TrimSpace(name)
			id, ok := ids[strings.ToLower(name)]
			if !ok {
				var made struct {
					ID string `json:"id"`
				}
				if err := c.gws(ctx, l, "gmail label create", &made, "gmail", "users", "labels", "create", "--params", jsonArg(me(nil)),
					"--json", jsonArg(map[string]any{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"})); err != nil {
					return err
				}
				id = made.ID
				ids[strings.ToLower(name)] = id
			}
			addIDs = append(addIDs, id)
		}
		for _, name := range remove {
			id, ok := ids[strings.ToLower(strings.TrimSpace(name))]
			if !ok {
				return fmt.Errorf("there is no label %q to remove", name)
			}
			removeIDs = append(removeIDs, id)
		}
		body := map[string]any{}
		if len(addIDs) > 0 {
			body["addLabelIds"] = addIDs
		}
		if len(removeIDs) > 0 {
			body["removeLabelIds"] = removeIDs
		}
		if t.MessageID != "" {
			var m gmailMessage
			err := c.gws(ctx, l, "gmail modify", &m, "gmail", "users", "messages", "modify",
				"--params", jsonArg(me(map[string]any{"id": t.MessageID})), "--json", jsonArg(body))
			labels = m.LabelIDs
			return err
		}
		var th struct {
			Messages []gmailMessage `json:"messages"`
		}
		err := c.gws(ctx, l, "gmail thread modify", &th, "gmail", "users", "threads", "modify",
			"--params", jsonArg(me(map[string]any{"id": t.ThreadID})), "--json", jsonArg(body))
		if n := len(th.Messages); n > 0 {
			labels = th.Messages[n-1].LabelIDs
		}
		return err
	})
	return labels, goneAs(err, gmail.ErrGone)
}
