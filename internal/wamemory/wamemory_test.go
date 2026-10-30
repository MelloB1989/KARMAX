package wamemory

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
	msgs  map[string][]Message
	names map[string]string
}

func (f *fakeSource) Chats(context.Context) ([]Chat, error) { return f.chats, nil }

func (f *fakeSource) Messages(_ context.Context, chat string, after time.Time, limit int) ([]Message, error) {
	var out []Message
	for _, m := range f.msgs[chat] {
		if m.At.After(after) {
			out = append(out, m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeSource) Name(_ context.Context, jid string) string { return f.names[jid] }

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
	return Message{ID: id, ChatJID: chat, SenderJID: sender, Content: text, At: at, FromMe: fromMe}
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
		src:  &fakeSource{msgs: map[string][]Message{}, names: map[string]string{}},
		mem:  &fakeMemory{multi: map[string]bool{}},
		cur:  memCursors{},
		ask:  &recorder{},
		sent: map[string]bool{},
	}
	if cfg.Operator == "" {
		cfg.Operator = "Quinn"
	}
	if cfg.Since.IsZero() {
		cfg.Since = t0.Add(-time.Hour)
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	sent := func(chat, text string, _ time.Time) bool { return h.sent[chat+"|"+text] }
	h.s = New(cfg, h.src, h.mem, h.cur, sent, h.ask.ask, zap.NewNop())
	return h
}

func (h *harness) chat(jid, name string, group bool, msgs ...Message) {
	last := t0
	for _, m := range msgs {
		if m.At.After(last) {
			last = m.At
		}
	}
	h.src.chats = append(h.src.chats, Chat{JID: jid, Name: name, IsGroup: group, LastMessageAt: last})
	h.src.msgs[jid] = msgs
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

	h.src.msgs["111@s.whatsapp.net"] = append(h.src.msgs["111@s.whatsapp.net"],
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
	h := newHarness(t, Config{OperatorJIDs: []string{"555@lid"}})
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
		"KARMAX (on Quinn's account): I'll send the brief tonight", "Quinn: from my other phone", "Contoso core (group)"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestDeniedChatsAndChannelsAreSkipped(t *testing.T) {
	h := newHarness(t, Config{Deny: []string{"Memes", "222@g.us"}})
	h.chat("1@g.us", "Memes", true, msg("a", "1@g.us", "9@lid", "lol", t0, false))
	h.chat("222@g.us", "Spam", true, msg("b", "222@g.us", "9@lid", "buy now", t0, false))
	h.chat("3@newsletter", "News", false, msg("c", "3@newsletter", "3@newsletter", "headline", t0, false))
	h.chat("status@broadcast", "", false, msg("d", "status@broadcast", "9@lid", "story", t0, false))

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
