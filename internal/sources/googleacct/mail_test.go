package googleacct

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gmail"
)

// gmailHost is one gws login for the account with the given scopes.
func gmailHost(scopes ...string) (*fakeCLI, *Client) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatusWith(acctAddr, scopes...)})
	return f, NewClient(hostWith(f, "/cfg/gws"), time.UTC)
}

// gogMail is one gog login for the account, with Gmail.
func gogMail() (*fakeCLI, *Client) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[{"email":"priya@example.com","services":["calendar","gmail"],"auth":"oauth"}]}`})
	h := hostWith(f)
	h.Gws = ""
	return f, NewClient(h, time.UTC)
}

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

const historyPages = `{"history":[{"id":"101","messagesAdded":[{"message":{"id":"m1","threadId":"m1","labelIds":["INBOX","UNREAD"]}}]}],"nextPageToken":"p2","historyId":"150"}
{"history":[{"id":"102","messagesAdded":[{"message":{"id":"m2","threadId":"t9","labelIds":["INBOX","IMPORTANT"]}}]}],"historyId":"160"}
`

func TestGwsReadsHistoryForNewInboxMail(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users history list", reply{out: historyPages})
	page, err := c.History(context.Background(), acctAddr, "100")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Added) != 2 || page.Added[1].ID != "m2" || page.Added[1].ThreadID != "t9" || page.Next != "160" {
		t.Fatalf("page %+v", page)
	}
	if strings.Join(page.Added[1].Labels, ",") != "INBOX,IMPORTANT" {
		t.Errorf("labels %v", page.Added[1].Labels)
	}
	p := params(t, f.find(t, "gws", "history", "list"))
	if p["startHistoryId"] != "100" || p["labelId"] != "INBOX" || p["userId"] != "me" {
		t.Errorf("params %v", p)
	}
	if hs, _ := p["historyTypes"].([]any); len(hs) != 1 || hs[0] != "messageAdded" {
		t.Errorf("history types %v", p["historyTypes"])
	}
}

func TestAnExpiredHistoryIDIsSaidSo(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users history list", reply{out: `{"error":{"code": 404,"message":"Requested entity was not found."}}`, code: 1})
	if _, err := c.History(context.Background(), acctAddr, "1"); !errors.Is(err, gmail.ErrHistoryExpired) {
		t.Fatalf("err %v", err)
	}
	if h := c.Health(acctAddr); h.Err != nil {
		t.Errorf("an expired history id is not an unreachable account: %v", h.Err)
	}
}

func TestGwsStartsFromTheProfilesHistoryID(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users getProfile", reply{out: "Using keyring backend: file\n" + `{"emailAddress":"priya@example.com","historyId":"4242"}`})
	f.on("gws", "", "gmail users messages list", reply{out: `{"messages":[{"id":"m3","threadId":"m3"},{"id":"m2","threadId":"m2"}]}`})
	if id, err := c.Start(context.Background(), acctAddr); err != nil || id != "4242" {
		t.Fatalf("start %q %v", id, err)
	}
	refs, err := c.Recent(context.Background(), acctAddr, 25, false)
	if err != nil || len(refs) != 2 || refs[0].ID != "m3" {
		t.Fatalf("recent %+v %v", refs, err)
	}
	if p := params(t, f.find(t, "gws", "messages", "list")); p["maxResults"] != float64(25) || strings.Contains(argValue(f.find(t, "gws", "messages", "list").args, "--params"), `"q"`) {
		t.Errorf("recent params %v: the inbox by label, not by search (a metadata-only login cannot search)", p)
	}
	if _, err := c.Recent(context.Background(), acctAddr, 25, true); err != nil {
		t.Fatal(err)
	}
	if p := params(t, f.find(t, "gws", "messages", "list")); strings.Join(asStrings(p["labelIds"]), ",") != "INBOX,UNREAD" {
		t.Errorf("unread recent params %v", p)
	}
}

const fullMessage = `{"id":"m2","threadId":"t9","labelIds":["INBOX","UNREAD","IMPORTANT"],"snippet":"See the attached draft","internalDate":"1793610000000",
"payload":{"mimeType":"multipart/mixed","headers":[{"name":"From","value":"Dana Reyes <dana@example.org>"},{"name":"To","value":"priya@example.com"},
{"name":"Subject","value":"Lease renewal"},{"name":"Message-ID","value":"<abc@mail.example.org>"}],
"parts":[{"mimeType":"multipart/alternative","filename":"","parts":[{"mimeType":"text/plain","filename":""},{"mimeType":"text/html","filename":""}]},
{"mimeType":"application/pdf","filename":"lease.pdf"}]}}`

// A message is read for its headers, labels and attachment names — never
// its body, which is masked out of the answer.
func TestAMessageIsReadWithoutItsBody(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users messages get", reply{out: fullMessage})
	m, err := c.Message(context.Background(), acctAddr, "m2")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "m2" || m.ThreadID != "t9" || m.Header("Subject") != "Lease renewal" || m.Snippet != "See the attached draft" {
		t.Fatalf("message %+v", m)
	}
	if !m.At.Equal(time.UnixMilli(1793610000000)) || strings.Join(m.Attachments, ",") != "lease.pdf" {
		t.Errorf("at %v attachments %v", m.At, m.Attachments)
	}
	p := params(t, f.find(t, "gws", "messages", "get"))
	fields, _ := p["fields"].(string)
	if p["format"] != "full" || !strings.Contains(fields, "filename") || strings.Contains(fields, "body") || strings.Contains(fields, "data") {
		t.Errorf("a message must be read without its body: %v", p)
	}
}

// A login granted only Gmail's metadata scope cannot ask for the full
// format at all, so it reads the metadata format.
func TestAMetadataOnlyLoginReadsTheMetadataFormat(t *testing.T) {
	f, c := gmailHost(scopeMeta)
	f.on("gws", "", "gmail users messages get", reply{out: `{"id":"m2","threadId":"t9","labelIds":["INBOX"],"payload":{"headers":[{"name":"From","value":"dana@example.org"}]}}`})
	if _, err := c.Message(context.Background(), acctAddr, "m2"); err != nil {
		t.Fatal(err)
	}
	if p := params(t, f.find(t, "gws", "messages", "get")); p["format"] != "metadata" {
		t.Errorf("params %v", p)
	}
}

func TestAMessageDeletedMeanwhileIsGone(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users messages get", reply{out: `{"error":{"code": 404,"message":"Not Found"}}`, code: 1})
	if _, err := c.Message(context.Background(), acctAddr, "m2"); !errors.Is(err, gmail.ErrGone) {
		t.Fatalf("err %v", err)
	}
}

func TestAThreadIsListedWithLabelsOnly(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users threads get", reply{out: `{"id":"t9","messages":[{"id":"t9","labelIds":["SENT"]},{"id":"m2","labelIds":["INBOX"]}]}`})
	refs, err := c.Thread(context.Background(), acctAddr, "t9")
	if err != nil || len(refs) != 2 || refs[0].Labels[0] != "SENT" {
		t.Fatalf("thread %+v %v", refs, err)
	}
	if p := params(t, f.find(t, "gws", "threads", "get")); p["format"] != "minimal" {
		t.Errorf("params %v", p)
	}
}

func TestGogReadsHistoryAndMessagesRaw(t *testing.T) {
	f, c := gogMail()
	f.on("gog", "", "gmail history --since 100", reply{out: `{"historyId":"170","messages":["m1","m2"]}`})
	f.on("gog", "", "gmail raw m2 --format metadata", reply{out: `{"id":"m2","threadId":"t9","labelIds":["INBOX"],"payload":{"headers":[{"name":"Subject","value":"Lease renewal"}]}}`})
	f.on("gog", "", "gmail messages search in:anywhere", reply{out: `{"messages":[{"id":"m7","threadId":"m7"}]}`})
	f.on("gog", "", "gmail raw m7 --format minimal", reply{out: `{"id":"m7","historyId":"999"}`})
	page, err := c.History(context.Background(), acctAddr, "100")
	if err != nil || len(page.Added) != 2 || page.Added[0].Labels != nil || page.Next != "170" {
		t.Fatalf("page %+v %v", page, err)
	}
	m, err := c.Message(context.Background(), acctAddr, "m2")
	if err != nil || m.Header("Subject") != "Lease renewal" {
		t.Fatalf("message %+v %v", m, err)
	}
	if id, err := c.Start(context.Background(), acctAddr); err != nil || id != "999" {
		t.Fatalf("start %q %v", id, err)
	}
	if got := f.find(t, "gog", "gmail", "history"); argValue(got.args, "--account") != acctAddr {
		t.Errorf("history as the wrong account: %v", got)
	}
}

const original = `{"id":"m2","threadId":"t9","labelIds":["INBOX"],"payload":{"headers":[
{"name":"From","value":"Dana Reyes <dana@example.org>"},{"name":"To","value":"Priya Sharma <priya@example.com>, sam@example.org"},
{"name":"Cc","value":"lee@example.org, priya@example.net"},{"name":"Subject","value":"Lease renewal"},
{"name":"Message-ID","value":"<abc@mail.example.org>"},{"name":"References","value":"<root@mail.example.org>"}]}}`

// sentMessage decodes the RFC 5322 message a gws send carried.
func sentMessage(t *testing.T, f *fakeCLI) (*mail.Message, string, map[string]any) {
	t.Helper()
	b := body(t, f.find(t, "gws", "gmail", "messages", "send"))
	raw, _ := b["raw"].(string)
	dec, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		dec, err = base64.RawURLEncoding.DecodeString(raw)
	}
	if err != nil {
		t.Fatalf("raw is not base64url: %v", err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(dec)))
	if err != nil {
		t.Fatalf("not a message: %v\n%s", err, dec)
	}
	text, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
	return m, string(text), b
}

// A reply stays in its thread: the thread id, In-Reply-To and References
// all point at the message answered.
func TestAReplyIsThreadedToTheMessageItAnswers(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users messages get", reply{out: original})
	f.on("gws", "", "gmail users messages send", reply{out: `{"id":"r1","threadId":"t9","labelIds":["SENT"]}`})
	sent, err := c.ReplyMail(context.Background(), acctAddr, MailReply{MessageID: "m2", Body: "Signed copy attached by Friday — thanks!"})
	if err != nil {
		t.Fatal(err)
	}
	m, text, b := sentMessage(t, f)
	if b["threadId"] != "t9" || sent.ThreadID != "t9" || sent.ID != "r1" {
		t.Errorf("not in the thread: %v %+v", b, sent)
	}
	if m.Header.Get("In-Reply-To") != "<abc@mail.example.org>" || m.Header.Get("References") != "<root@mail.example.org> <abc@mail.example.org>" {
		t.Errorf("threading headers %v", m.Header)
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject")); subj != "Re: Lease renewal" {
		t.Errorf("subject %q", subj)
	}
	if to := m.Header.Get("To"); !strings.Contains(to, "dana@example.org") || strings.Contains(to, "sam@example.org") || m.Header.Get("Cc") != "" {
		t.Errorf("a plain reply goes to the sender only: to %q cc %q", to, m.Header.Get("Cc"))
	}
	if strings.TrimSpace(text) != "Signed copy attached by Friday — thanks!" {
		t.Errorf("body %q", text)
	}
	if strings.Join(sent.To, ",") != "Dana Reyes <dana@example.org>" {
		t.Errorf("sent to %v", sent.To)
	}
}

// Reply-all answers everyone on the message but the operator, under any of
// their addresses.
func TestReplyAllLeavesOutTheOperatorsOwnAddresses(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users messages get", reply{out: original})
	f.on("gws", "", "gmail users messages send", reply{out: `{"id":"r1","threadId":"t9"}`})
	if _, err := c.ReplyMail(context.Background(), acctAddr, MailReply{MessageID: "m2", Body: "ok", All: true, Own: []string{"priya@example.net"}}); err != nil {
		t.Fatal(err)
	}
	m, _, _ := sentMessage(t, f)
	all := strings.ToLower(m.Header.Get("To") + "," + m.Header.Get("Cc"))
	for _, want := range []string{"dana@example.org", "sam@example.org", "lee@example.org"} {
		if !strings.Contains(all, want) {
			t.Errorf("reply-all left out %s: %s", want, all)
		}
	}
	if strings.Contains(all, "priya@") {
		t.Errorf("reply-all wrote to the operator: %s", all)
	}
}

// Answering a thread whose last word was the operator's continues it to the
// same people.
func TestReplyingAfterTheOperatorsOwnMessageWritesToItsRecipients(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users threads get", reply{out: `{"id":"t9","messages":[{"id":"m2","labelIds":["INBOX"]},{"id":"m3","labelIds":["SENT"]}]}`})
	f.on("gws", "", "gmail users messages get", reply{out: `{"id":"m3","threadId":"t9","labelIds":["SENT"],"payload":{"headers":[
{"name":"From","value":"priya@example.com"},{"name":"To","value":"Dana Reyes <dana@example.org>"},{"name":"Subject","value":"Re: Lease renewal"},
{"name":"Message-ID","value":"<own@mail.example.com>"}]}}`})
	f.on("gws", "", "gmail users messages send", reply{out: `{"id":"r2","threadId":"t9"}`})
	if _, err := c.ReplyMail(context.Background(), acctAddr, MailReply{ThreadID: "t9", Body: "Following up."}); err != nil {
		t.Fatal(err)
	}
	m, _, _ := sentMessage(t, f)
	if to := m.Header.Get("To"); !strings.Contains(to, "dana@example.org") {
		t.Errorf("to %q", to)
	}
	if subj, _ := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject")); subj != "Re: Lease renewal" {
		t.Errorf("subject %q, want no second Re:", subj)
	}
}

// A new email is a plain-text RFC 5322 message whose headers the model
// cannot inject into.
func TestANewEmailCannotHaveHeadersInjected(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users messages send", reply{out: `{"id":"n1","threadId":"n1"}`})
	_, err := c.SendMail(context.Background(), acctAddr, Outgoing{To: []string{"dana@example.org"}, Cc: []string{"Lee Park <lee@example.org>"},
		Subject: "Lease\r\nBcc: evil@example.org", Body: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	m, _, b := sentMessage(t, f)
	if m.Header.Get("Bcc") != "" || b["threadId"] != nil {
		t.Errorf("headers %v body %v", m.Header, b)
	}
	if _, err := c.SendMail(context.Background(), acctAddr, Outgoing{To: []string{"not an address"}, Subject: "x", Body: "y"}); err == nil {
		t.Error("an unreadable address was sent to")
	}
	if _, err := c.SendMail(context.Background(), acctAddr, Outgoing{To: []string{"dana@example.org"}, Subject: "x"}); err == nil {
		t.Error("an empty email was sent")
	}
}

func TestSendingNeedsTheSendScope(t *testing.T) {
	_, c := gmailHost(scopeReadonly)
	_, err := c.SendMail(context.Background(), acctAddr, Outgoing{To: []string{"dana@example.org"}, Subject: "x", Body: "y"})
	var ae *AuthError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), scopeModify) {
		t.Fatalf("err %v", err)
	}
}

func TestGogRepliesThroughItsOwnThreading(t *testing.T) {
	f, c := gogMail()
	f.on("gog", "", "gmail raw m2 --format metadata", reply{out: original})
	f.on("gog", "", "gmail send", reply{out: `{"messageId":"r1","threadId":"t9"}`})
	sent, err := c.ReplyMail(context.Background(), acctAddr, MailReply{MessageID: "m2", Body: "Thanks"})
	if err != nil || sent.ThreadID != "t9" {
		t.Fatalf("sent %+v %v", sent, err)
	}
	got := f.find(t, "gog", "gmail", "send")
	if argValue(got.args, "--reply-to-message-id") != "m2" || argValue(got.args, "--subject") != "Re: Lease renewal" ||
		!strings.Contains(argValue(got.args, "--to"), "dana@example.org") || argValue(got.args, "--body") != "Thanks" {
		t.Errorf("gog send %v", got)
	}
}

// Labels are named as the operator sees them; one that does not exist yet is
// made. Trash and spam are never applied: deleting mail is the operator's.
func TestLabellingByNameAndNeverTrash(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users labels list", reply{out: `{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"UNREAD","name":"UNREAD","type":"system"},{"id":"Label_7","name":"Clients","type":"user"}]}`})
	f.on("gws", "", "gmail users labels create", reply{out: `{"id":"Label_9","name":"Leases"}`})
	f.on("gws", "", "gmail users threads modify", reply{out: `{"id":"t9","messages":[{"id":"m2","labelIds":["Label_7","Label_9"]}]}`})
	if _, err := c.LabelMail(context.Background(), acctAddr, MailTarget{ThreadID: "t9"}, []string{"clients", "Leases"}, []string{"inbox", "unread"}); err != nil {
		t.Fatal(err)
	}
	b := body(t, f.find(t, "gws", "threads", "modify"))
	if strings.Join(asStrings(b["addLabelIds"]), ",") != "Label_7,Label_9" || strings.Join(asStrings(b["removeLabelIds"]), ",") != "INBOX,UNREAD" {
		t.Errorf("modify %v", b)
	}
	if name := body(t, f.find(t, "gws", "labels", "create"))["name"]; name != "Leases" {
		t.Errorf("created %v", name)
	}
	for _, bad := range []string{"TRASH", "spam"} {
		if _, err := c.LabelMail(context.Background(), acctAddr, MailTarget{MessageID: "m2"}, []string{bad}, nil); err == nil {
			t.Errorf("%s was applied", bad)
		}
	}
}

// Reading a message gives its text: the plain part when there is one, else
// the HTML without its markup.
func TestReadingAMessageGivesItsText(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users messages get m-plain", reply{out: `{"id":"m-plain","threadId":"t1","payload":{"mimeType":"multipart/alternative","headers":[{"name":"Subject","value":"Hi"}],
"parts":[{"mimeType":"text/html","body":{"data":"` + b64("<p>HTML</p>") + `"}},{"mimeType":"text/plain","body":{"data":"` + b64("Plain words") + `"}}]}}`})
	f.on("gws", "", "gmail users messages get m-html", reply{out: `{"id":"m-html","threadId":"t1","payload":{"mimeType":"text/html",
"body":{"data":"` + b64("<html><style>p{}</style><p>Hello&nbsp;there</p><br>Bye</html>") + `"}}}`})
	plain, err := c.ReadMail(context.Background(), acctAddr, "m-plain")
	if err != nil || plain.Body != "Plain words" || plain.Subject != "Hi" {
		t.Fatalf("plain %+v %v", plain, err)
	}
	h, err := c.ReadMail(context.Background(), acctAddr, "m-html")
	if err != nil || !strings.Contains(h.Body, "Hello there") || strings.Contains(h.Body, "<") || strings.Contains(h.Body, "p{}") {
		t.Fatalf("html %+v %v", h, err)
	}
	if p := params(t, f.find(t, "gws", "messages", "get", "m-html")); p["format"] != "full" {
		t.Errorf("params %v", p)
	}
}

func asStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
