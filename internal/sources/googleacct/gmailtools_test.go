package googleacct

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/tools"
)

// twoMailAccounts is a host with two accounts that both have Gmail.
func twoMailAccounts() (*fakeCLI, *Client) {
	f := &fakeCLI{}
	f.on("gog", "", "auth list", reply{out: `{"accounts":[{"email":"work@example.org","services":["calendar","chat","gmail"],"auth":"oauth"}]}`})
	f.on("gws", "", "auth status", reply{out: gwsStatusWith(acctAddr, scopeCal, scopeModify)})
	return f, NewClient(hostWith(f, "/cfg/gws"), time.UTC)
}

func recorder() (*[]tools.SentAs, tools.SentRecorder) {
	var got []tools.SentAs
	return &got, func(s tools.SentAs) { got = append(got, s) }
}

func TestTheGmailToolsAreOfferedAndNoneDeletes(t *testing.T) {
	_, c := gmailHost(scopeModify)
	set := Tools(c, time.UTC, "", nil)
	for _, name := range []string{"gmail.read", "gmail.reply", "gmail.send", "gmail.label"} {
		toolNamed(t, set, name)
	}
	if len(set) != len(ToolNames()) {
		t.Fatalf("Tools gives %d, ToolNames lists %d", len(set), len(ToolNames()))
	}
	for _, tl := range set {
		if n := tl.Manifest().Name; strings.Contains(n, "delete") || strings.Contains(n, "trash") {
			t.Errorf("a tool that deletes: %s", n)
		}
	}
}

// A reply lent for a Gmail event goes out as that event's account, in its
// thread, and is recorded as sent as the operator.
func TestAReplyIsSentAsTheEventsAccountAndRecorded(t *testing.T) {
	f, c := twoMailAccounts()
	f.on("gws", "", "gmail users messages get", reply{out: original})
	f.on("gws", "", "gmail users messages send", reply{out: `{"id":"r1","threadId":"t9"}`})
	got, rec := recorder()
	res := run(t, toolNamed(t, Tools(c, time.UTC, acctAddr, rec), "gmail.reply"),
		map[string]any{"message_id": "m2", "text": "Signed copy by Friday."})
	if res.IsError {
		t.Fatalf("reply: %v", res.Error)
	}
	if got := f.find(t, "gws", "messages", "send"); got.envValue("GOOGLE_WORKSPACE_CLI_CONFIG_DIR") != "/cfg/gws" {
		t.Errorf("sent through the wrong login: %v", got)
	}
	if len(*got) != 1 {
		t.Fatalf("recorded %+v", *got)
	}
	s := (*got)[0]
	if s.Service != "gmail" || s.Account != acctAddr || s.Text != "Signed copy by Friday." ||
		!strings.Contains(s.To, "dana@example.org") || !strings.Contains(s.To, "Lease renewal") || !strings.Contains(s.URL, "t9") {
		t.Errorf("recorded %+v", s)
	}
	if out, _ := json.Marshal(res.Output); !strings.Contains(string(out), acctAddr) || !strings.Contains(string(out), "t9") {
		t.Errorf("the result should say which account and thread: %s", out)
	}
}

// Unbound with two accounts, a send names its account or is refused.
func TestASendNamesItsAccountWhenThereIsAChoice(t *testing.T) {
	f, c := twoMailAccounts()
	f.on("gws", "", "gmail users messages send", reply{out: `{"id":"n1","threadId":"n1"}`})
	got, rec := recorder()
	send := toolNamed(t, Tools(c, time.UTC, "", rec), "gmail.send")
	in := map[string]any{"to": []any{"dana@example.org"}, "subject": "Lease", "text": "Hello Dana"}
	if res := run(t, send, in); !res.IsError || !strings.Contains(res.Error, "work@example.org") {
		t.Fatalf("an ambiguous account should be refused with the choices: %+v", res)
	}
	in["account"] = acctAddr
	if res := run(t, send, in); res.IsError {
		t.Fatalf("send: %v", res.Error)
	}
	if len(*got) != 1 || (*got)[0].To != "dana@example.org (“Lease”)" || (*got)[0].Text != "Hello Dana" {
		t.Errorf("recorded %+v", *got)
	}
}

// Reading returns the text, bounded.
func TestReadingIsBounded(t *testing.T) {
	f, c := gmailHost(scopeModify)
	long := strings.Repeat("word ", 4000)
	f.on("gws", "", "gmail users messages get", reply{out: `{"id":"m2","threadId":"t9","payload":{"mimeType":"text/plain","headers":[{"name":"Subject","value":"Long"}],"body":{"data":"` + b64(long) + `"}}}`})
	f.on("gws", "", "gmail users threads get", reply{out: `{"id":"t9","messages":[{"id":"a","payload":{"mimeType":"text/plain","body":{"data":"` + b64(long) + `"}}},{"id":"b","payload":{"mimeType":"text/plain","body":{"data":"` + b64("Short reply") + `"}}}]}`})
	read := toolNamed(t, Tools(c, time.UTC, "", nil), "gmail.read")
	res := run(t, read, map[string]any{"message_id": "m2", "max_chars": 500})
	if res.IsError {
		t.Fatal(res.Error)
	}
	out, _ := json.Marshal(res.Output)
	if len(out) > 1500 || !strings.Contains(string(out), "truncated") {
		t.Errorf("a long body should be cut to max_chars: %d bytes", len(out))
	}
	res = run(t, read, map[string]any{"thread_id": "t9"})
	out, _ = json.Marshal(res.Output)
	if res.IsError || !strings.Contains(string(out), "Short reply") {
		t.Fatalf("thread %s %v", out, res.Error)
	}
	if run(t, read, map[string]any{}).IsError == false {
		t.Error("a read naming nothing should be refused")
	}
}

// Labelling archives and marks read, records nothing as sent, and never
// trashes.
func TestLabellingArchivesAndMarksRead(t *testing.T) {
	f, c := gmailHost(scopeModify)
	f.on("gws", "", "gmail users labels list", reply{out: `{"labels":[{"id":"INBOX","name":"INBOX"},{"id":"UNREAD","name":"UNREAD"},{"id":"STARRED","name":"STARRED"}]}`})
	f.on("gws", "", "gmail users messages modify", reply{out: `{"id":"m2","labelIds":["STARRED"]}`})
	got, rec := recorder()
	label := toolNamed(t, Tools(c, time.UTC, "", rec), "gmail.label")
	res := run(t, label, map[string]any{"message_id": "m2", "archive": true, "mark_read": true, "add": []any{"starred"}})
	if res.IsError {
		t.Fatal(res.Error)
	}
	b := body(t, f.find(t, "gws", "messages", "modify"))
	if strings.Join(asStrings(b["removeLabelIds"]), ",") != "INBOX,UNREAD" || strings.Join(asStrings(b["addLabelIds"]), ",") != "STARRED" {
		t.Errorf("modify %v", b)
	}
	if len(*got) != 0 {
		t.Errorf("labelling is not something sent: %+v", *got)
	}
	if res := run(t, label, map[string]any{"message_id": "m2", "add": []any{"Trash"}}); !res.IsError {
		t.Error("trash was applied")
	}
}
