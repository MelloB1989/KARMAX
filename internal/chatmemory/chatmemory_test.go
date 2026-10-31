package chatmemory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"go.uber.org/zap"
)

var t0 = time.Date(2026, 10, 29, 21, 0, 0, 0, time.UTC)

type fakeSource struct {
	chats []Chat
	msgs  map[string][]Message // by account|chat
	names map[string]string
	reads map[string]int // Messages calls, by account|chat
}

func srcKey(c Chat) string { return c.Account + "|" + c.ID }

func (f *fakeSource) Chats(context.Context) ([]Chat, error) { return f.chats, nil }

func (f *fakeSource) Messages(_ context.Context, c Chat, after time.Time, limit int) ([]Message, error) {
	f.reads[srcKey(c)]++
	var out []Message
	for _, m := range f.msgs[srcKey(c)] {
		if m.At.After(after) {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSource) Name(_ context.Context, id string) string { return f.names[id] }

type fakeMemory struct {
	mu        sync.Mutex
	hits      []memory.SearchResult
	writes    []memory.MemoryEntry
	forgotten []string
	multi     map[string]bool // handles that name a document of several facts
}

func (f *fakeMemory) SearchWith(string, int, memory.SearchOpts) ([]memory.SearchResult, error) {
	return f.hits, nil
}

func (f *fakeMemory) Write(e memory.MemoryEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, e)
	return nil
}

func (f *fakeMemory) ForgetFact(_ context.Context, handle string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.multi[handle] {
		return false, nil
	}
	f.forgotten = append(f.forgotten, handle)
	return true, nil
}

type memCursors map[string]Cursor

func (c memCursors) Get(chat string) (Cursor, bool, error) { v, ok := c[chat]; return v, ok, nil }
func (c memCursors) Set(chat string, v Cursor) error       { c[chat] = v; return nil }

type recorder struct {
	prompts []string
	replies []string
	err     error
}

func (r *recorder) ask(_ context.Context, prompt string) (string, error) {
	r.prompts = append(r.prompts, prompt)
	if r.err != nil {
		return "", r.err
	}
	if len(r.replies) == 0 {
		return `{"ops":[]}`, nil
	}
	reply := r.replies[0]
	r.replies = r.replies[1:]
	return reply, nil
}

func msg(id, chat, sender, text string, at time.Time, fromMe bool) Message {
	return Message{ID: id, Chat: chat, Sender: sender, Content: text, At: at, FromMe: fromMe}
}

type harness struct {
	src  *fakeSource
	mem  *fakeMemory
	cur  memCursors
	ask  *recorder
	sent map[string]bool
	s    *Syncer
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{
		src:  &fakeSource{msgs: map[string][]Message{}, names: map[string]string{}, reads: map[string]int{}},
		mem:  &fakeMemory{multi: map[string]bool{}},
		cur:  memCursors{},
		ask:  &recorder{},
		sent: map[string]bool{},
	}
	if cfg.Operator == "" {
		cfg.Operator = "Quinn"
	}
	if cfg.Service == "" {
		cfg.Name, cfg.Service, cfg.Tag = "wa-memory", "WhatsApp", "whatsapp"
	}
	if cfg.Since.IsZero() && cfg.FirstLook == nil {
		cfg.Since = t0.Add(-time.Hour)
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	sent := func(c Chat, text string, _ time.Time) bool { return h.sent[c.ID+"|"+text] }
	h.s = New(cfg, h.src, h.mem, h.cur, sent, h.ask.ask, zap.NewNop())
	return h
}

func (h *harness) chat(jid, name string, group bool, msgs ...Message) {
	h.add(Chat{ID: jid, Name: name, IsGroup: group}, msgs...)
}

// add lists a chat whose last activity is its last message.
func (h *harness) add(c Chat, msgs ...Message) {
	last := t0
	for _, m := range msgs {
		if m.At.After(last) {
			last = m.At
		}
	}
	if c.LastMessageAt.IsZero() {
		c.LastMessageAt = last
	}
	h.src.chats = append(h.src.chats, c)
	h.src.msgs[srcKey(c)] = msgs
}

func TestAFactSaidInAChatIsFiledUnderItsSubject(t *testing.T) {
	h := newHarness(t, Config{})
	h.chat("111@s.whatsapp.net", "Omar Haddad", false,
		msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "I moved to Fabrikam last week, heading their delivery team now", t0, false))
	h.ask.replies = []string{`{"ops":[{"op":"add","fact":"Omar Haddad moved to Fabrikam in October 2026 and heads its delivery team.","subject":"omar haddad","category":"relationship","importance":"medium","when":"2026-10-29"}]}`}

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.mem.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(h.mem.writes))
	}
	w := h.mem.writes[0]
	if w.Category != "relationship" || len(w.Tags) == 0 || w.Tags[0] != "omar haddad" {
		t.Errorf("filed as category %q tags %v; want relationship under the subject first", w.Category, w.Tags)
	}
	if !w.OccurredAt.Equal(time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dated %v, want the day it was said", w.OccurredAt)
	}
	if !strings.Contains(w.Content, "Fabrikam") {
		t.Errorf("content = %q", w.Content)
	}
}

func TestACorrectionReplacesTheFactItCorrects(t *testing.T) {
	h := newHarness(t, Config{})
	h.mem.hits = []memory.SearchResult{{Entry: memory.MemoryEntry{ID: "facts/people/omar.md#2026-08-01-omar-works-at-contoso", Content: "Omar works at Contoso."}}}
	h.chat("111@s.whatsapp.net", "Omar Haddad", false,
		msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "left contoso, I'm at fabrikam now", t0, false))
	h.ask.replies = []string{`{"ops":[{"op":"update","memory":1,"fact":"Omar Haddad left Contoso and works at Fabrikam (October 2026).","subject":"omar","category":"relationship","importance":"medium","when":"2026-10-29"}]}`}

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.mem.writes) != 1 || !strings.Contains(h.mem.writes[0].Content, "Fabrikam") {
		t.Fatalf("the correction was not written: %+v", h.mem.writes)
	}
	if len(h.mem.forgotten) != 1 || h.mem.forgotten[0] != "facts/people/omar.md#2026-08-01-omar-works-at-contoso" {
		t.Errorf("forgotten = %v; the corrected fact must go", h.mem.forgotten)
	}
}

func TestAMemoryTheModelWasNotShownIsNeverTouched(t *testing.T) {
	h := newHarness(t, Config{})
	h.mem.hits = []memory.SearchResult{{Entry: memory.MemoryEntry{ID: "facts/people/omar.md#x", Content: "Omar works at Contoso."}}}
	h.chat("111@s.whatsapp.net", "Omar", false, msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "plan cancelled", t0, false))
	h.ask.replies = []string{`{"ops":[{"op":"forget","memory":7},{"op":"forget","handle":"facts/projects/zero.md"},{"op":"update","memory":0,"fact":"x","subject":"y","category":"context"}]}`}

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.mem.forgotten) != 0 || len(h.mem.writes) != 0 {
		t.Errorf("acted on memories it was not shown: forgot %v, wrote %v", h.mem.forgotten, h.mem.writes)
	}
}

func TestADocumentOfManyFactsIsNotForgottenWhole(t *testing.T) {
	h := newHarness(t, Config{})
	h.mem.hits = []memory.SearchResult{{Entry: memory.MemoryEntry{ID: "facts/projects/contoso.md", Content: "## a\n\none\n\n## b\n\ntwo"}}}
	h.mem.multi["facts/projects/contoso.md"] = true
	h.chat("1@g.us", "Contoso", true, msg("a", "1@g.us", "9@lid", "the APK ask is done", t0, false))
	h.ask.replies = []string{`{"ops":[{"op":"forget","memory":1}]}`}

	res, err := h.s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.mem.forgotten) != 0 {
		t.Errorf("forgot %v", h.mem.forgotten)
	}
	if res.Refused == 0 {
		t.Error("a refused forget was not counted")
	}
}

func TestSecretsAreNeverStored(t *testing.T) {
	h := newHarness(t, Config{})
	h.chat("111@s.whatsapp.net", "Priya", false, msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "login is priya / Hunter2!x", t0, false))
	h.ask.replies = []string{`{"ops":[` +
		`{"op":"add","fact":"Priya's portal password is Hunter2!x.","subject":"priya","category":"relationship"},` +
		`{"op":"add","fact":"The OTP for the bank login was 482913.","subject":"bank","category":"context"},` +
		`{"op":"add","fact":"Runpod API key: rpa_8f3k2j4h5g6f7d8s9a0.","subject":"runpod","category":"reference"},` +
		`{"op":"add","fact":"Priya shared the portal login on 2026-10-29; credentials were not stored.","subject":"priya","category":"relationship"}]}`}

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.mem.writes) != 1 || !strings.Contains(h.mem.writes[0].Content, "were not stored") {
		t.Errorf("writes = %+v; only the fact without a secret may be stored", h.mem.writes)
	}
}

func TestEachMessageIsReadOnce(t *testing.T) {
	h := newHarness(t, Config{})
	h.chat("111@s.whatsapp.net", "Omar", false,
		msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "first message", t0, false),
		msg("b", "111@s.whatsapp.net", "111@s.whatsapp.net", "second message", t0.Add(time.Minute), false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.ask.prompts) != 1 {
		t.Fatalf("the same messages were read %d times", len(h.ask.prompts))
	}

	h.src.msgs["|111@s.whatsapp.net"] = append(h.src.msgs["|111@s.whatsapp.net"],
		msg("c", "111@s.whatsapp.net", "111@s.whatsapp.net", "third message", t0.Add(2*time.Minute), false))
	h.src.chats[0].LastMessageAt = t0.Add(2 * time.Minute)
	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := h.ask.prompts[len(h.ask.prompts)-1]
	if strings.Contains(last, "first message") || !strings.Contains(last, "third message") {
		t.Errorf("the next pass should hold only what is new:\n%s", last)
	}
}

func TestMessagesAreReadAgainWhenTheModelFails(t *testing.T) {
	h := newHarness(t, Config{})
	h.chat("111@s.whatsapp.net", "Omar", false, msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "deadline moved to friday", t0, false))
	h.ask.err = errors.New("harness down")

	if _, err := h.s.Tick(context.Background()); err == nil {
		t.Error("a failed extraction was reported as success")
	}
	h.ask.err = nil
	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := h.ask.prompts[len(h.ask.prompts)-1]; !strings.Contains(last, "deadline moved to friday") {
		t.Error("messages a failed pass did not process were skipped")
	}
}

func TestSpeakersAreNamedForWhoTheyAre(t *testing.T) {
	h := newHarness(t, Config{OperatorIDs: []string{"555@lid"}})
	h.src.names["777@lid"] = "Priya Raman"
	h.sent["1@g.us|I'll send the brief tonight"] = true
	h.chat("1@g.us", "Contoso core", true,
		msg("a", "1@g.us", "777@lid", "can we move the call?", t0, false),
		msg("b", "1@g.us", "999:12@s.whatsapp.net", "sure, 9pm works", t0.Add(time.Minute), true),
		msg("c", "1@g.us", "999@s.whatsapp.net", "I'll send the brief tonight", t0.Add(2*time.Minute), true),
		msg("d", "1@g.us", "555:3@lid", "from my other phone", t0.Add(3*time.Minute), false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	for _, want := range []string{"Priya Raman: can we move the call?", "Quinn: sure, 9pm works",
		"KARMAX (on Quinn's account): I'll send the brief tonight", "Quinn: from my other phone", "CHAT: Contoso core (a WhatsApp group)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

// The operator's account is theirs however it is labelled, and memory is never
// told the operator has another name.
//
// About twenty "alternate name" facts had to be cleaned out of memory: lines
// from the operator's own accounts reached the extractor under whatever label
// the account carried — a legacy name, a business name — and the extractor
// dutifully recorded that the operator was also called that. The operator's
// accounts are now decided by the identity directory (which also knows the LIDs
// it has learned), and the instructions forbid the fact outright.
func TestTheOperatorKeepsOneNameWhateverTheirAccountIsCalled(t *testing.T) {
	h := newHarness(t, Config{
		OperatorIDs: []string{"555@lid"},
		IsOperator:  func(jid string) bool { return strings.HasPrefix(jid, "444") },
	})
	h.src.names["444@lid"] = "Omar" // a label on one of the operator's accounts
	h.chat("1@g.us", "Contoso core", true,
		msg("a", "1@g.us", "444:2@lid", "the call is at 2pm", t0, false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	if !strings.Contains(p, "Quinn: the call is at 2pm") || strings.Contains(p, "Omar:") {
		t.Errorf("the operator's account was named by its label:\n%s", p)
	}
	if !strings.Contains(p, "Quinn has no other name") {
		t.Errorf("the instructions do not forbid alternate names for the operator:\n%s", p)
	}
}

func TestDeniedChatsAreSkipped(t *testing.T) {
	h := newHarness(t, Config{Deny: []string{"Memes", "222@g.us"}})
	h.chat("1@g.us", "Memes", true, msg("a", "1@g.us", "9@lid", "lol", t0, false))
	h.chat("222@g.us", "Spam", true, msg("b", "222@g.us", "9@lid", "buy now", t0, false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.ask.prompts) != 0 {
		t.Errorf("read %d skipped chats", len(h.ask.prompts))
	}
}

func TestATickStopsAtItsBudgetAndRecentChatsGoFirst(t *testing.T) {
	h := newHarness(t, Config{BatchesPerTick: 2})
	for i := 0; i < 4; i++ {
		jid := fmt.Sprintf("%d@s.whatsapp.net", i)
		at := t0.Add(time.Duration(i) * time.Hour)
		h.chat(jid, fmt.Sprintf("chat %d", i), false, msg(fmt.Sprint(i), jid, jid, fmt.Sprintf("note %d", i), at, false))
	}
	res, err := h.s.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.ask.prompts) != 2 || res.Batches != 2 {
		t.Fatalf("ran %d batches, want the budget of 2", len(h.ask.prompts))
	}
	if !strings.Contains(h.ask.prompts[0], "note 3") || !strings.Contains(h.ask.prompts[1], "note 2") {
		t.Errorf("the most recently active chats should go first")
	}
	if !res.More {
		t.Error("work left over was not reported")
	}
}

func TestABackfillStartsAtSince(t *testing.T) {
	since := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Since: since})
	h.chat("111@s.whatsapp.net", "Omar", false,
		msg("old", "111@s.whatsapp.net", "111@s.whatsapp.net", "from july", since.Add(-24*time.Hour), false),
		msg("new", "111@s.whatsapp.net", "111@s.whatsapp.net", "from august", since.Add(24*time.Hour), false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	if strings.Contains(p, "from july") || !strings.Contains(p, "from august") {
		t.Errorf("backfill window wrong:\n%s", p)
	}
}

func TestALongHistoryIsReadInBatches(t *testing.T) {
	h := newHarness(t, Config{BatchChars: 60, BatchesPerTick: 10})
	var ms []Message
	for i := 0; i < 5; i++ {
		ms = append(ms, msg(fmt.Sprint(i), "111@s.whatsapp.net", "111@s.whatsapp.net",
			fmt.Sprintf("message number %d with some words", i), t0.Add(time.Duration(i)*time.Minute), false))
	}
	h.chat("111@s.whatsapp.net", "Omar", false, ms...)

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.ask.prompts) < 2 {
		t.Fatalf("a history over the batch size went in %d batch", len(h.ask.prompts))
	}
	seen := strings.Join(h.ask.prompts, "\n")
	for i := 0; i < 5; i++ {
		if strings.Count(seen, fmt.Sprintf("message number %d ", i)) != 1 {
			t.Errorf("message %d was read %d times", i, strings.Count(seen, fmt.Sprintf("message number %d ", i)))
		}
	}
}

// A message is shown, and its fact dated, in the operator's own day: 21:00 UTC
// is already tomorrow in India, and a fact dated by the UTC day is a day off.
func TestTimesAreTheOperatorsOwn(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	h := newHarness(t, Config{Location: ist})
	at := time.Date(2026, 10, 29, 21, 0, 0, 0, time.UTC)
	h.chat("111@s.whatsapp.net", "Omar", false, msg("a", "111@s.whatsapp.net", "111@s.whatsapp.net", "call moved to tomorrow 10am", at, false))
	h.src.chats[0].LastMessageAt = at
	h.ask.replies = []string{`{"ops":[{"op":"add","fact":"Omar moved the call to 2026-10-31 10:00.","subject":"omar","category":"task","when":"2026-10-30"}]}`}

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.ask.prompts[0], "2026-10-30 02:30 Omar:") {
		t.Errorf("message not shown in the operator's zone:\n%s", h.ask.prompts[0])
	}
	if len(h.mem.writes) != 1 || !h.mem.writes[0].OccurredAt.Equal(time.Date(2026, 10, 30, 0, 0, 0, 0, ist)) {
		t.Errorf("dated %v, want 2026-10-30 in IST", h.mem.writes)
	}
}

// The syncer reads any chat service. The prompt names the service where it
// matters, never assumes WhatsApp, and what it files is tagged with the
// service it came from.
func TestTheServiceIsNamedWhereItMatters(t *testing.T) {
	h := newHarness(t, Config{Name: "gchat-memory", Service: "Google Chat", Tag: "gchat"})
	h.add(Chat{ID: "spaces/AAAA", Account: "quinn@example.org", Name: "Launch crew", IsGroup: true, Kind: "space"},
		msg("spaces/AAAA/messages/1", "spaces/AAAA", "users/7", "the launch moved to 2026-11-12", t0, false))
	h.src.names["users/7"] = "Dana Reyes"
	h.ask.replies = []string{`{"ops":[{"op":"add","fact":"The launch moved to 2026-11-12.","subject":"launch","category":"project"}]}`}

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	for _, want := range []string{"Quinn's Google Chat", "CHAT: Launch crew (a Google Chat space)", "Dana Reyes: the launch moved"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "WhatsApp") {
		t.Errorf("a Google Chat prompt assumes WhatsApp:\n%s", p)
	}
	if len(h.mem.writes) != 1 {
		t.Fatalf("writes = %d", len(h.mem.writes))
	}
	if w := h.mem.writes[0]; w.Role != "gchat" || len(w.Tags) != 2 || w.Tags[1] != "gchat" {
		t.Errorf("filed with role %q tags %v; want the service's tag", w.Role, w.Tags)
	}
}

// One service, several of the operator's logins: the same space id on two
// accounts is two conversations, each with its own cursor.
func TestChatsOnDifferentAccountsKeepTheirOwnCursors(t *testing.T) {
	h := newHarness(t, Config{Service: "Google Chat", Tag: "gchat"})
	h.add(Chat{ID: "spaces/AAAA", Account: "quinn@example.org", Name: "Ops", IsGroup: true},
		msg("1", "spaces/AAAA", "users/7", "work account note", t0, false))
	h.add(Chat{ID: "spaces/AAAA", Account: "quinn@example.net", Name: "Ops", IsGroup: true},
		msg("1", "spaces/AAAA", "users/8", "side project note", t0, false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	seen := strings.Join(h.ask.prompts, "\n")
	if !strings.Contains(seen, "work account note") || !strings.Contains(seen, "side project note") {
		t.Fatalf("one account's chat hid the other's:\n%s", seen)
	}
	if len(h.cur) != 2 {
		t.Errorf("cursors = %v; want one per account", h.cur)
	}
}

// With no fixed start, a chat never read begins a lookback before KARMAX
// first looked at its account: a restart does not move the start, and an
// account added later gets its own week.
func TestAChatWithNoCursorStartsALookbackBeforeItsAccountWasFirstSeen(t *testing.T) {
	looked := map[string]time.Time{"quinn@example.org": t0}
	asked := 0
	h := newHarness(t, Config{Service: "Google Chat", Tag: "gchat", Lookback: 7 * 24 * time.Hour,
		FirstLook: func(account string) (time.Time, error) { asked++; return looked[account], nil }})
	h.add(Chat{ID: "spaces/AAAA", Account: "quinn@example.org", Name: "Ops", IsGroup: true},
		msg("old", "spaces/AAAA", "users/7", "from eight days before", t0.Add(-8*24*time.Hour), false),
		msg("new", "spaces/AAAA", "users/7", "from six days before", t0.Add(-6*24*time.Hour), false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	if strings.Contains(p, "eight days") || !strings.Contains(p, "six days") {
		t.Errorf("backfill window wrong:\n%s", p)
	}
	if asked == 0 {
		t.Error("the account's first look was never asked")
	}
}

// A message that carries its sender's name is shown by it; the service is
// asked only when it does not.
func TestAMessagesOwnSenderNameIsUsed(t *testing.T) {
	h := newHarness(t, Config{})
	h.src.names["users/7"] = "a stale label"
	m := msg("a", "spaces/AAAA", "users/7", "standup moves to 10:30", t0, false)
	m.SenderName = "Dana Reyes"
	h.add(Chat{ID: "spaces/AAAA", Name: "Ops", IsGroup: true}, m)

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := h.ask.prompts[0]; !strings.Contains(p, "Dana Reyes: standup moves") {
		t.Errorf("the message's own sender name was not used:\n%s", p)
	}
}

// A direct chat the service gives no name is named for the other person in it.
func TestAnUnnamedDirectChatIsNamedForTheOtherPerson(t *testing.T) {
	h := newHarness(t, Config{Service: "Google Chat", Tag: "gchat"})
	h.src.names["users/7"] = "Dana Reyes"
	h.add(Chat{ID: "spaces/DDDD", Account: "quinn@example.org", Kind: "direct message"},
		msg("a", "spaces/DDDD", "users/1", "are we still on for friday?", t0, true),
		msg("b", "spaces/DDDD", "users/7", "yes, 4pm at the office", t0.Add(time.Minute), false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	for _, want := range []string{"CHAT: Dana Reyes (a Google Chat direct message)", "Quinn: are we still on", "Dana Reyes: yes, 4pm"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

// A sender nobody can name is shown by their id: a phone number as one, any
// other id as itself.
func TestASenderWithNoNameIsShownByTheirID(t *testing.T) {
	h := newHarness(t, Config{})
	h.add(Chat{ID: "spaces/AAAA", Name: "Ops", IsGroup: true},
		msg("a", "spaces/AAAA", "users/7", "first", t0, false),
		msg("b", "spaces/AAAA", "919990001111@s.whatsapp.net", "second", t0.Add(time.Minute), false))

	if _, err := h.s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := h.ask.prompts[0]
	if !strings.Contains(p, " users/7: first") || !strings.Contains(p, " +919990001111: second") {
		t.Errorf("unnamed senders shown wrongly:\n%s", p)
	}
}

// A chat whose last activity was not a message — a reaction, someone joining —
// is read once, not again every tick.
func TestAChatWhoseLastActivityWasNotAMessageIsNotReadAgain(t *testing.T) {
	h := newHarness(t, Config{})
	h.add(Chat{ID: "spaces/AAAA", Name: "Ops", IsGroup: true, LastMessageAt: t0.Add(5 * time.Minute)},
		msg("a", "spaces/AAAA", "users/7", "the only message", t0, false))

	for i := 0; i < 3; i++ {
		if _, err := h.s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.src.reads["|spaces/AAAA"]; n > 2 {
		t.Errorf("a quiet chat was read %d times in 3 ticks", n)
	}
	if len(h.ask.prompts) != 1 {
		t.Errorf("prompts = %d, want the one message read once", len(h.ask.prompts))
	}
}
