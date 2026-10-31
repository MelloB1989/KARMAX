package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

// fakeSlack is one or more Slack workspaces, told apart by token.
type fakeSlack struct {
	t   *testing.T
	mu  sync.Mutex
	srv *httptest.Server
	ws  map[string]*fakeWorkspace // token -> workspace
	// calls records every request as "method token form".
	calls []fakeCall
}

type fakeCall struct {
	Method string
	Token  string
	Form   url.Values
}

type fakeWorkspace struct {
	auth     map[string]any   // auth.test answer
	convs    []map[string]any // users.conversations
	history  map[string][]map[string]any
	search   []map[string]any
	searchOK bool
	users    map[string]string // id -> real name
	limited  bool              // answer 429
	posted   []url.Values
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{t: t, ws: map[string]*fakeWorkspace{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) workspace(token string, w *fakeWorkspace) *fakeWorkspace {
	if w.history == nil {
		w.history = map[string][]map[string]any{}
	}
	f.ws[token] = w
	return w
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	method := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Method: method, Token: token, Form: r.Form})
	ws := f.ws[token]
	f.mu.Unlock()

	reply := func(v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if ws == nil {
		reply(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	if ws.limited {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	switch method {
	case "auth.test":
		out := map[string]any{"ok": true}
		for k, v := range ws.auth {
			out[k] = v
		}
		reply(out)
	case "users.conversations":
		reply(map[string]any{"ok": true, "channels": ws.convs, "response_metadata": map[string]any{"next_cursor": ""}})
	case "conversations.history":
		oldest := r.Form.Get("oldest")
		var msgs []map[string]any
		for _, m := range ws.history[r.Form.Get("channel")] {
			if oldest == "" || tsAfter(m["ts"].(string), oldest) {
				msgs = append(msgs, m)
			}
		}
		// Slack answers newest first.
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
		reply(map[string]any{"ok": true, "messages": msgs, "has_more": false})
	case "search.messages":
		if !ws.searchOK {
			reply(map[string]any{"ok": false, "error": "missing_scope", "needed": "search:read"})
			return
		}
		reply(map[string]any{"ok": true, "messages": map[string]any{"matches": ws.search}})
	case "users.info":
		id := r.Form.Get("user")
		name, ok := ws.users[id]
		if !ok {
			reply(map[string]any{"ok": false, "error": "user_not_found"})
			return
		}
		reply(map[string]any{"ok": true, "user": map[string]any{"id": id, "name": strings.ToLower(name),
			"profile": map[string]any{"real_name": name, "display_name": ""}}})
	case "chat.postMessage":
		f.mu.Lock()
		ws.posted = append(ws.posted, r.Form)
		f.mu.Unlock()
		reply(map[string]any{"ok": true, "channel": r.Form.Get("channel"), "ts": "1793440000.000900"})
	default:
		reply(map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (f *fakeSlack) callsTo(method string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// memKV is the store's key-value table.
type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func newKV() *memKV { return &memKV{m: map[string]string{}} }

func (k *memKV) KVGet(group, key string) (string, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[group+"\x00"+key]
	return v, ok, nil
}

func (k *memKV) KVSet(group, key, value string, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[group+"\x00"+key] = value
	return nil
}

// Times in the fixtures: the source's clock reads 2026-10-31 09:00 UTC.
var clock = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

func ts(minutesBefore int, seq int) string {
	return fmt.Sprintf("%d.%06d", clock.Add(-time.Duration(minutesBefore)*time.Minute).Unix(), seq)
}

// northwind is a workspace where the operator (U0OPER) has a DM from Alice,
// a mention in #eng, an ordinary #eng message, and some things that are not
// news: their own message, a join, and KARMAX's own bot talking.
func northwind(f *fakeSlack) *fakeWorkspace {
	f.workspace("xoxb-northwind", &fakeWorkspace{auth: map[string]any{"user_id": "U0KARMAXBOT", "bot_id": "B0KARMAX", "team_id": "T0NW"}})
	return f.workspace("xoxp-northwind", &fakeWorkspace{
		auth: map[string]any{"user_id": "U0OPER", "user": "operator", "team": "Northwind Labs",
			"team_id": "T0NW", "url": "https://northwind-labs.slack.com/"},
		convs: []map[string]any{
			{"id": "D0ALICE", "is_im": true, "user": "U0ALICE"},
			{"id": "C0ENG", "is_channel": true, "name": "eng"},
		},
		history: map[string][]map[string]any{
			"D0ALICE": {
				{"type": "message", "user": "U0ALICE", "text": "can you review the launch deck today?", "ts": ts(10, 1)},
				{"type": "message", "user": "U0OPER", "text": "on it", "ts": ts(9, 1)},
			},
			"C0ENG": {
				{"type": "message", "subtype": "channel_join", "user": "U0BOB", "text": "<@U0BOB> has joined the channel", "ts": ts(30, 1)},
				{"type": "message", "user": "U0BOB", "text": "deploy is green", "ts": ts(20, 1)},
				{"type": "message", "user": "U0BOB", "text": "<@U0OPER> can you approve the migration?", "ts": ts(15, 1)},
				{"type": "message", "user": "U0KARMAXBOT", "bot_id": "B0KARMAX", "text": "noted", "ts": ts(14, 1)},
			},
		},
		users: map[string]string{"U0ALICE": "Alice Example", "U0BOB": "Bob Sample", "U0OPER": "Op Erator"},
	})
}

func newTestSource(f *fakeSlack, kv KV, got *[]sources.Event) *Source {
	s := New(Config{}, kv, func(e sources.Event) error { *got = append(*got, e); return nil })
	s.API = f.srv.URL
	s.Now = func() time.Time { return clock }
	return s
}

func byType(evs []sources.Event) map[string]sources.Event {
	out := map[string]sources.Event{}
	for _, e := range evs {
		out[e.Type] = e
	}
	return out
}

// A DM and a mention are the operator being asked something directly, so they
// are urgent; a channel message is worth knowing and nothing more. The
// operator's own words, a join, and KARMAX's own bot are not events at all.
func TestDirectMessagesAndMentionsAreUrgentAndChannelChatterIsNot(t *testing.T) {
	f := newFakeSlack(t)
	northwind(f)
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	res, err := s.Tick(context.Background(), []Workspace{{Integration: "slack-main", UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("published %d events, want 3 (dm, mention, channel): %+v", len(got), got)
	}
	ev := byType(got)
	dm, mention, chat := ev["message.dm"], ev["message.mention"], ev["message.channel"]
	if !dm.Urgent || !mention.Urgent || chat.Urgent {
		t.Errorf("urgency dm=%v mention=%v channel=%v, want true true false", dm.Urgent, mention.Urgent, chat.Urgent)
	}
	for _, e := range got {
		if e.Source != "slack" || e.Account != "northwind-labs" {
			t.Errorf("event from %s:%s, want slack:northwind-labs", e.Source, e.Account)
		}
	}
	if !strings.Contains(dm.Title, "Alice Example") || !strings.Contains(dm.Title, "launch deck") {
		t.Errorf("dm title %q should say who and what", dm.Title)
	}
	if !strings.Contains(mention.Title, "#eng") || !strings.Contains(mention.Summary, "@Op Erator") {
		t.Errorf("mention title %q / summary %q should name the channel and read the mention", mention.Title, mention.Summary)
	}
	if want := "https://northwind-labs.slack.com/archives/D0ALICE/p" + strings.ReplaceAll(ts(10, 1), ".", ""); dm.URL != want {
		t.Errorf("dm url %q, want %q", dm.URL, want)
	}
	if dm.Key != "T0NW:D0ALICE:"+ts(10, 1) {
		t.Errorf("dm key %q", dm.Key)
	}
	// Enough to answer without looking anything up.
	if dm.Data["channel"] != "D0ALICE" || dm.Data["ts"] != ts(10, 1) || dm.Data["workspace"] != "northwind-labs" {
		t.Errorf("dm data %v lacks where to reply", dm.Data)
	}
	if mention.Data["thread_ts"] != ts(15, 1) {
		t.Errorf("a reply to a channel mention goes in its thread; thread_ts = %v", mention.Data["thread_ts"])
	}
	if res.Published != 3 {
		t.Errorf("result says %d published", res.Published)
	}
}

// Polling sees the same message on every tick, and a restart starts over with
// nothing in memory: the cursor and Seen in the store keep it to once.
func TestAMessageIsPublishedOnceAcrossTicksAndRestarts(t *testing.T) {
	f := newFakeSlack(t)
	northwind(f)
	kv := newKV()
	var got []sources.Event
	ws := []Workspace{{Integration: "slack-main", UserToken: "xoxp-northwind"}}

	s := newTestSource(f, kv, &got)
	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	first := len(got)
	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	restarted := newTestSource(f, kv, &got)
	if _, err := restarted.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if len(got) != first {
		t.Fatalf("published %d then %d: the same messages came back", first, len(got))
	}
	// The restart asked Slack only for what came after the cursor.
	calls := f.callsTo("conversations.history")
	last := calls[len(calls)-1]
	if last.Form.Get("oldest") == "" || !tsAfter(last.Form.Get("oldest"), ts(60, 0)) {
		t.Errorf("after a restart history was read from %q, not from the cursor", last.Form.Get("oldest"))
	}
}

// A conversation never read before is read back only as far as the lookback,
// so connecting a workspace does not replay its history as news.
func TestAConversationNeverReadStartsAtTheLookback(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.history["D0ALICE"] = append([]map[string]any{
		{"type": "message", "user": "U0ALICE", "text": "old news from last week", "ts": ts(7*24*60, 1)},
	}, w.history["D0ALICE"]...)
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)
	s.cfg.Lookback = 2 * time.Hour

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if strings.Contains(e.Summary, "old news") {
			t.Fatal("a week-old message was published as news")
		}
	}
	oldest := f.callsTo("conversations.history")[0].Form.Get("oldest")
	if want := fmt.Sprintf("%d.000000", clock.Add(-2*time.Hour).Unix()); oldest != want {
		t.Errorf("first read from %q, want the lookback %q", oldest, want)
	}
}

// A mention inside a thread never shows in conversations.history; search
// finds it. One found both ways is still one event.
func TestMentionsInThreadsComeFromSearchAndAreNotDoubled(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.searchOK = true
	w.search = []map[string]any{
		{"type": "message", "user": "U0BOB", "text": "<@U0OPER> thoughts on this thread?", "ts": ts(5, 1),
			"channel": map[string]any{"id": "C0ENG", "name": "eng"}, "permalink": "https://northwind-labs.slack.com/archives/C0ENG/p1?thread_ts=" + ts(40, 1)},
		// The same top-level mention history already has.
		{"type": "message", "user": "U0BOB", "text": "<@U0OPER> can you approve the migration?", "ts": ts(15, 1),
			"channel": map[string]any{"id": "C0ENG", "name": "eng"}},
	}
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatal(err)
	}
	mentions := 0
	var threaded sources.Event
	for _, e := range got {
		if e.Type == "message.mention" {
			mentions++
			if strings.Contains(e.Summary, "thoughts on this thread") {
				threaded = e
			}
		}
	}
	if mentions != 2 {
		t.Fatalf("%d mentions published, want 2 (one top-level, one in a thread)", mentions)
	}
	if !threaded.Urgent || threaded.Data["thread_ts"] != ts(40, 1) {
		t.Errorf("thread mention urgent=%v thread_ts=%v", threaded.Urgent, threaded.Data["thread_ts"])
	}
	if q := f.callsTo("search.messages")[0].Form.Get("query"); q != "<@U0OPER>" {
		t.Errorf("search query %q, want the operator's mention", q)
	}
}

// A token without search:read still reads every conversation; the gap is
// reported, not fatal.
func TestATokenWithoutSearchStillReadsConversations(t *testing.T) {
	f := newFakeSlack(t)
	northwind(f)
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}}); err != nil {
		t.Fatalf("a missing search scope failed the tick: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("published %d, want the 3 history events", len(got))
	}
	st := s.Status()
	if len(st) != 1 || !strings.Contains(st[0].Note, "search:read") {
		t.Errorf("status %+v should say mentions in threads need search:read", st)
	}
}

// Each workspace is its own account, so their events keep their own order and
// a reply goes back through the right login.
func TestTwoWorkspacesAreTwoAccounts(t *testing.T) {
	f := newFakeSlack(t)
	northwind(f)
	f.workspace("xoxp-contoso", &fakeWorkspace{
		auth:    map[string]any{"user_id": "U0OP2", "team": "Contoso", "team_id": "T0CO", "url": "https://contoso-example.slack.com/"},
		convs:   []map[string]any{{"id": "D0CAROL", "is_im": true, "user": "U0CAROL"}},
		history: map[string][]map[string]any{"D0CAROL": {{"type": "message", "user": "U0CAROL", "text": "lunch?", "ts": ts(3, 1)}}},
		users:   map[string]string{"U0CAROL": "Carol Demo"},
	})
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	if _, err := s.Tick(context.Background(), []Workspace{
		{Integration: "slack-main", UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"},
		{Integration: "slack-contoso", UserToken: "xoxp-contoso"},
	}); err != nil {
		t.Fatal(err)
	}
	accounts := map[string]int{}
	for _, e := range got {
		accounts[e.Account]++
	}
	if accounts["northwind-labs"] != 3 || accounts["contoso-example"] != 1 {
		t.Errorf("events per account %v", accounts)
	}
}

// Many conversations are read a budget at a time, DMs first, so a busy
// workspace never exhausts Slack's rate limit in one tick.
func TestConversationsAreReadWithinABudgetDirectMessagesFirst(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.convs = []map[string]any{
		{"id": "C0ENG", "is_channel": true, "name": "eng"},
		{"id": "C0OPS", "is_channel": true, "name": "ops"},
		{"id": "D0ALICE", "is_im": true, "user": "U0ALICE"},
	}
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)
	s.cfg.CallsPerTick = 2

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatal(err)
	}
	calls := f.callsTo("conversations.history")
	if len(calls) != 2 || calls[0].Form.Get("channel") != "D0ALICE" {
		t.Fatalf("first tick read %d conversations starting with %v; want 2, the DM first", len(calls), calls)
	}
	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatal(err)
	}
	read := map[string]bool{}
	for _, c := range f.callsTo("conversations.history") {
		read[c.Form.Get("channel")] = true
	}
	if !read["C0OPS"] || !read["C0ENG"] {
		t.Errorf("the second tick should reach what the first left; read %v", read)
	}
}

// Slack saying "slow down" pauses that workspace instead of failing forever.
func TestARateLimitedWorkspaceWaitsItsTurn(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.limited = true
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatalf("a rate limit is a pause, not a failure: %v", err)
	}
	before := len(f.callsTo("auth.test"))
	w.limited = false
	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsTo("auth.test")) != before {
		t.Error("asked Slack again before Retry-After had passed")
	}
	s.Now = func() time.Time { return clock.Add(3 * time.Minute) }
	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind"}}); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Error("nothing was read once the wait was over")
	}
}

// With no user token the source is off, and says exactly what to run.
func TestWithoutAUserTokenTheSourceIsOffAndSaysWhatToRun(t *testing.T) {
	f := newFakeSlack(t)
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	res, err := s.Tick(context.Background(), []Workspace{{Integration: "slack-main", BotToken: "xoxb-northwind"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || len(got) != 0 {
		t.Fatal("an unconfigured source talked to Slack")
	}
	for _, want := range []string{"karmax login slack-main", "xoxp-", "im:history"} {
		if !strings.Contains(res.Off, want) {
			t.Errorf("off reason %q does not say %q", res.Off, want)
		}
	}

	// A bot token pasted where the user token goes is refused, not used.
	res, _ = s.Tick(context.Background(), []Workspace{{Integration: "slack-main", UserToken: "xoxb-northwind"}})
	if len(f.calls) != 0 || !strings.Contains(res.Off, "xoxp-") {
		t.Errorf("a bot token in the user slot was used, or not explained: %q", res.Off)
	}
}

// KARMAX answers as the operator: their own account, in the DM or the thread.
func TestKarmaxRepliesAsTheOperatorInAThread(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)
	ws := []Workspace{{Integration: "slack-main", UserToken: "xoxp-northwind"}}
	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}

	sent, err := s.Send(context.Background(), ws, "northwind-labs", "C0ENG", ts(15, 1), "approved, go ahead")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.posted) != 1 {
		t.Fatalf("%d posts", len(w.posted))
	}
	p := w.posted[0]
	if p.Get("channel") != "C0ENG" || p.Get("thread_ts") != ts(15, 1) || p.Get("text") != "approved, go ahead" {
		t.Errorf("posted %v", p)
	}
	post := f.callsTo("chat.postMessage")[0]
	if post.Token != "xoxp-northwind" {
		t.Error("the reply did not go out as the operator's own account")
	}
	if sent.Workspace != "northwind-labs" || sent.TS == "" || !strings.Contains(sent.URL, "/archives/C0ENG/p") {
		t.Errorf("sent %+v", sent)
	}

	// With one workspace the name may be left out; an unknown one is refused.
	if _, err := s.Send(context.Background(), ws, "", "D0ALICE", "", "done"); err != nil {
		t.Errorf("the only workspace was not assumed: %v", err)
	}
	if _, err := s.Send(context.Background(), ws, "elsewhere", "D0ALICE", "", "x"); err == nil {
		t.Error("a workspace nobody configured was accepted")
	}
}

// A busy channel is one event per read, not one per line: the agent sees the
// conversation, and a mention in it still stands on its own.
func TestChannelChatterIsOneEventPerChannelPerRead(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.history["C0ENG"] = append(w.history["C0ENG"],
		map[string]any{"type": "message", "user": "U0ALICE", "text": "staging is back", "ts": ts(8, 1)},
		map[string]any{"type": "message", "user": "U0BOB", "text": "thanks <@U0ALICE>", "ts": ts(7, 1)},
	)
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}}); err != nil {
		t.Fatal(err)
	}
	var chatter []sources.Event
	mentions := 0
	for _, e := range got {
		switch e.Type {
		case "message.channel":
			chatter = append(chatter, e)
		case "message.mention":
			mentions++
		}
	}
	if len(chatter) != 1 || mentions != 1 {
		t.Fatalf("%d chatter events and %d mentions, want 1 and 1", len(chatter), mentions)
	}
	c := chatter[0]
	if c.Urgent || !strings.Contains(c.Title, "#eng") || !strings.Contains(c.Title, "3 new messages") {
		t.Errorf("chatter title %q urgent %v", c.Title, c.Urgent)
	}
	for _, said := range []string{"Bob Sample: deploy is green", "Alice Example: staging is back", "Bob Sample: thanks @Alice Example"} {
		if !strings.Contains(c.Summary, said) {
			t.Errorf("chatter summary lacks %q:\n%s", said, c.Summary)
		}
	}
	if msgs, _ := c.Data["messages"].([]map[string]any); len(msgs) != 3 {
		t.Errorf("chatter data carries %d messages, want 3", len(msgs))
	}
	if c.Key != "T0NW:C0ENG:"+ts(7, 1) {
		t.Errorf("chatter key %q is not the last message's", c.Key)
	}
}

// A bot writing in a DM is a notice, not a person asking. KARMAX's own bot
// answering the operator in their DM with it is never an event: while the
// bot cannot be identified the workspace waits rather than risk hearing it.
func TestABotInADirectMessageIsNotAPersonAsking(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.convs = append(w.convs, map[string]any{"id": "D0CI", "is_im": true, "user": "U0CIBOT"},
		map[string]any{"id": "D0KARMAX", "is_im": true, "user": "U0KARMAXBOT"})
	w.history["D0CI"] = []map[string]any{{"type": "message", "subtype": "bot_message", "bot_id": "B0CI",
		"username": "Build Bot", "text": "build 812 failed", "ts": ts(4, 1)}}
	w.history["D0KARMAX"] = []map[string]any{{"type": "message", "user": "U0KARMAXBOT", "bot_id": "B0KARMAX",
		"text": "done: the deck is reviewed", "ts": ts(2, 1)}}
	bot := f.ws["xoxb-northwind"]
	bot.limited = true
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)
	ws := []Workspace{{UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}}

	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("published %d events before KARMAX's bot could be told apart", len(got))
	}
	bot.limited = false
	s.Now = func() time.Time { return clock.Add(3 * time.Minute) }
	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	ci := false
	for _, e := range got {
		if e.Data["channel"] == "D0KARMAX" {
			t.Errorf("KARMAX's own bot was heard as %q", e.Title)
		}
		if e.Data["channel"] == "D0CI" {
			ci = true
			if e.Urgent || !strings.Contains(e.Title, "Build Bot") {
				t.Errorf("a bot's DM: urgent=%v title %q", e.Urgent, e.Title)
			}
		}
	}
	if !ci {
		t.Error("the build bot's DM was not published at all")
	}

	// A bot token Slack refuses does not silence the operator's DMs; it is noted.
	f2 := newFakeSlack(t)
	northwind(f2)
	delete(f2.ws, "xoxb-northwind")
	var got2 []sources.Event
	s2 := newTestSource(f2, newKV(), &got2)
	if _, err := s2.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if len(got2) == 0 || !strings.Contains(s2.Status()[0].Note, "bot token") {
		t.Errorf("with a refused bot token: %d events, note %q", len(got2), s2.Status()[0].Note)
	}
}

// The first look at a workspace reads back as far as the lookback, so a DM or
// a mention from before KARMAX was watching would page the operator once at
// connect. What it finds is backlog: context for the agent, never a page.
func TestTheFirstLookAtAWorkspaceIsBacklog(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.searchOK = true
	w.search = []map[string]any{{"type": "message", "user": "U0BOB", "text": "<@U0OPER> thoughts on this thread?", "ts": ts(5, 1),
		"channel": map[string]any{"id": "C0ENG", "name": "eng"}}}
	var got []sources.Event
	s := newTestSource(f, newKV(), &got)
	ws := []Workspace{{Integration: "slack-main", UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}}

	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("published %d events on the first look, want 4: %+v", len(got), got)
	}
	for _, e := range got {
		if !e.Backlog {
			t.Errorf("%s %q from before the first look is not backlog", e.Type, e.Title)
		}
	}
}

// After the first look, what is said is news: in a conversation already read,
// in one that appears later (a new DM), and in a thread search finds.
func TestWhatIsSaidAfterTheFirstLookIsNews(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	w.searchOK = true
	kv := newKV()
	var got []sources.Event
	s := newTestSource(f, kv, &got)
	ws := []Workspace{{Integration: "slack-main", UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}}
	if _, err := s.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}

	later := clock.Add(5 * time.Minute)
	tsLater := func(seq int) string { return fmt.Sprintf("%d.%06d", later.Unix(), seq) }
	w.history["D0ALICE"] = append(w.history["D0ALICE"],
		map[string]any{"type": "message", "user": "U0ALICE", "text": "are you around?", "ts": tsLater(1)})
	w.convs = append(w.convs, map[string]any{"id": "D0DANA", "is_im": true, "user": "U0DANA"})
	w.history["D0DANA"] = []map[string]any{{"type": "message", "user": "U0DANA", "text": "hello, first time writing", "ts": tsLater(2)}}
	w.search = []map[string]any{{"type": "message", "user": "U0BOB", "text": "<@U0OPER> in the thread?", "ts": tsLater(3),
		"channel": map[string]any{"id": "C0ENG", "name": "eng"}}}
	w.users["U0DANA"] = "Dana Placeholder"

	got = got[:0]
	restarted := newTestSource(f, kv, &got) // a restart keeps the first look
	restarted.Now = func() time.Time { return later.Add(time.Minute) }
	if _, err := restarted.Tick(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("published %d events after the first look, want 3: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Backlog {
			t.Errorf("%q, said after the first look, is marked backlog", e.Title)
		}
	}
}

// A workspace read before first looks were recorded (a deploy over a running
// source) resumes from its cursors: what it reads there arrived while KARMAX
// was watching, so it is news even though no first look is on record.
func TestResumingFromACursorIsNeverBacklog(t *testing.T) {
	f := newFakeSlack(t)
	w := northwind(f)
	kv := newKV()
	// The cursors an earlier version left: everything up to 9 minutes ago read.
	_ = kv.KVSet(cursorGroup, "T0NW:D0ALICE", ts(9, 1), 0)
	_ = kv.KVSet(cursorGroup, "T0NW:C0ENG", ts(9, 1), 0)
	w.history["D0ALICE"] = append(w.history["D0ALICE"],
		map[string]any{"type": "message", "user": "U0ALICE", "text": "sent while KARMAX restarted", "ts": ts(1, 1)})
	var got []sources.Event
	s := newTestSource(f, kv, &got)

	if _, err := s.Tick(context.Background(), []Workspace{{UserToken: "xoxp-northwind", BotToken: "xoxb-northwind"}}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Backlog {
		t.Fatalf("resumed read published %+v; want the one new DM, as news", got)
	}
}
