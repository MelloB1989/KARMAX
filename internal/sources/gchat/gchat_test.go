package gchat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
)

const acct = "priya@example.com"

var t0 = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

var me = Me{User: "users/100", Name: "Priya Sharma", Email: acct}

type fakeAPI struct {
	me       Me
	meErr    error
	spaces   []Space
	msgs     map[string][]Message // by space, oldest first
	lastRead map[string]time.Time
	msgErr   map[string]error
	people   map[string]Person
	calls    map[string]int
}

func newFake() *fakeAPI {
	return &fakeAPI{me: me, msgs: map[string][]Message{}, lastRead: map[string]time.Time{},
		msgErr: map[string]error{}, people: map[string]Person{}, calls: map[string]int{}}
}

func (f *fakeAPI) Me(context.Context, string) (Me, error) { f.calls["me"]++; return f.me, f.meErr }
func (f *fakeAPI) Spaces(context.Context, string) ([]Space, error) {
	f.calls["spaces"]++
	return f.spaces, nil
}
func (f *fakeAPI) Messages(_ context.Context, _ string, space string, after time.Time, max int) ([]Message, error) {
	f.calls["messages:"+space]++
	if err := f.msgErr[space]; err != nil {
		return nil, err
	}
	var out []Message
	for _, m := range f.msgs[space] {
		if m.Created.After(after) {
			out = append(out, m)
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}
func (f *fakeAPI) LastRead(_ context.Context, _ string, space string) (time.Time, error) {
	f.calls["lastread:"+space]++
	return f.lastRead[space], nil
}
func (f *fakeAPI) Person(_ context.Context, _ string, user string) (Person, error) {
	f.calls["person:"+user]++
	p, ok := f.people[user]
	if !ok {
		return Person{}, errors.New("not found")
	}
	return p, nil
}

type memStore map[string]State

func (m memStore) Load(a string) (State, bool, error) { st, ok := m[a]; return st, ok, nil }
func (m memStore) Save(a string, st State) error      { m[a] = st; return nil }

func newSeen() sources.Seen {
	kv := map[string]string{}
	return sources.Seen{Group: "gchat-test",
		Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
		Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil }}
}

type rig struct {
	api   *fakeAPI
	store memStore
	w     *Watcher
	now   time.Time
}

func newRig(cfg Config) *rig {
	r := &rig{api: newFake(), store: memStore{}, now: t0}
	cfg.Location = time.UTC
	r.w = New(cfg, r.api, r.store, newSeen())
	r.w.now = func() time.Time { return r.now }
	return r
}

func (r *rig) tick(t *testing.T) []sources.Event {
	t.Helper()
	var got []sources.Event
	if _, err := r.w.Tick(context.Background(), acct, func(e sources.Event) error {
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatalf("tick: %v", err)
	}
	return got
}

func msg(space, id, sender, text string, at time.Time) Message {
	return Message{Name: space + "/messages/" + id, Space: space, Thread: space + "/threads/" + id,
		Sender: sender, SenderType: "HUMAN", Text: text, Created: at}
}

func types(evs []sources.Event) string {
	var s []string
	for _, e := range evs {
		s = append(s, e.Type)
	}
	return strings.Join(s, ",")
}

const (
	dm   = "spaces/DM1"
	team = "spaces/TEAM"
	old  = "spaces/OLD"
)

// The first look publishes what is unread from the last day and nothing else:
// not what the operator already read, not old history, not their own words.
func TestFirstSightPublishesOnlyRecentUnread(t *testing.T) {
	r := newRig(Config{})
	r.api.spaces = []Space{
		{Name: dm, Type: "DIRECT_MESSAGE", LastActive: t0.Add(-time.Hour)},
		{Name: team, DisplayName: "Launch crew", Type: "SPACE", LastActive: t0.Add(-2 * time.Hour)},
		{Name: old, DisplayName: "Archive", Type: "SPACE", LastActive: t0.Add(-40 * 24 * time.Hour)},
	}
	r.api.msgs[dm] = []Message{
		msg(dm, "a", "users/200", "can you look at the deck before 5?", t0.Add(-time.Hour)),
	}
	r.api.msgs[team] = []Message{
		msg(team, "b", "users/300", "already read this one", t0.Add(-5*time.Hour)),
		msg(team, "c", "users/100", "my own words", t0.Add(-3*time.Hour)),
		msg(team, "d", "users/300", "fresh and unread", t0.Add(-2*time.Hour)),
	}
	r.api.msgs[old] = []Message{msg(old, "e", "users/300", "ancient", t0.Add(-40*24*time.Hour))}
	r.api.lastRead[team] = t0.Add(-4 * time.Hour)
	r.api.people["users/200"] = Person{Name: "Rahul Verma", Email: "rahul@example.com"}

	got := r.tick(t)
	if types(got) != "dm,message" {
		t.Fatalf("first sight published %q, want the unread DM and the unread space message", types(got))
	}
	if !got[0].Urgent || got[1].Urgent {
		t.Errorf("urgency: dm %v, space message %v", got[0].Urgent, got[1].Urgent)
	}
	// Said before KARMAX started watching: context for the agent, never a page.
	for _, e := range got {
		if !e.Backlog {
			t.Errorf("a first-sight message was published as news: %q", e.Title)
		}
	}
	if !strings.Contains(got[0].Title, "Rahul Verma") || got[0].Data["sender_email"] != "rahul@example.com" {
		t.Errorf("the DM should name its sender: %q %v", got[0].Title, got[0].Data)
	}
	if got[0].Source != "gchat" || got[0].Account != acct || got[0].Data["space"] != dm || got[0].Data["thread"] != dm+"/threads/a" {
		t.Errorf("a message must carry what replying needs: %+v", got[0])
	}
	if r.api.calls["messages:"+old] != 0 {
		t.Error("a space quiet for weeks was read on first sight")
	}

	r.now = t0.Add(2 * time.Minute)
	if again := r.tick(t); len(again) != 0 {
		t.Fatalf("nothing new, yet published %q", types(again))
	}
	if r.api.calls["messages:"+team] != 1 {
		t.Errorf("a space with no new activity was read again (%d reads)", r.api.calls["messages:"+team])
	}
}

func TestNewMessagesMentionsAndDMs(t *testing.T) {
	r := newRig(Config{})
	r.api.spaces = []Space{
		{Name: dm, Type: "DIRECT_MESSAGE", LastActive: t0.Add(-48 * time.Hour)},
		{Name: team, DisplayName: "Launch crew", Type: "SPACE", LastActive: t0.Add(-48 * time.Hour)},
	}
	r.tick(t)

	r.now = t0.Add(10 * time.Minute)
	r.api.spaces[0].LastActive = t0.Add(9 * time.Minute)
	r.api.spaces[1].LastActive = t0.Add(9 * time.Minute)
	mention := msg(team, "m", "users/300", "@Priya Sharma can you approve?", t0.Add(7*time.Minute))
	mention.Mentions = []string{"users/100"}
	other := msg(team, "o", "users/300", "@Rahul Verma ping", t0.Add(8*time.Minute))
	other.Mentions = []string{"users/200"}
	bot := msg(dm, "bot", "users/900", "build passed", t0.Add(6*time.Minute))
	bot.SenderType = "BOT"
	r.api.msgs[dm] = []Message{bot, msg(dm, "x", "users/200", "call me", t0.Add(9*time.Minute))}
	r.api.msgs[team] = []Message{
		msg(team, "own", "users/100", "my reply", t0.Add(5*time.Minute)),
		mention, other,
	}
	got := r.tick(t)
	if types(got) != "dm,dm,mention,message" {
		t.Fatalf("published %q, want dm,dm,mention,message", types(got))
	}
	urgent := []bool{false, true, true, false}
	for i, e := range got {
		if e.Backlog {
			t.Errorf("%q was said after KARMAX started watching, yet published as backlog", e.Title)
		}
		if e.Urgent != urgent[i] {
			t.Errorf("%s %q urgent = %v, want %v (a bot is never urgent; a person writing directly is)", e.Type, e.Title, e.Urgent, urgent[i])
		}
	}
	if !strings.Contains(got[2].Title, "Launch crew") || !strings.Contains(got[2].Title, "mentioned you") {
		t.Errorf("mention title %q", got[2].Title)
	}
}

// A path that cannot list mentions still knows one when the text names the
// operator.
func TestAMentionIsKnownByNameWhenThePathCannotSay(t *testing.T) {
	r := newRig(Config{})
	r.api.spaces = []Space{{Name: team, Type: "SPACE"}}
	r.tick(t)
	r.now = t0.Add(5 * time.Minute)
	r.api.msgs[team] = []Message{msg(team, "m", "users/300", "thanks @priya sharma!", t0.Add(4*time.Minute))}
	got := r.tick(t)
	if types(got) != "mention" || !got[0].Urgent {
		t.Fatalf("published %q, want an urgent mention", types(got))
	}
}

// A space first seen after the account was (someone opened a new DM) is new
// in full: everything said since KARMAX started watching is told.
func TestANewSpaceAfterFirstSight(t *testing.T) {
	r := newRig(Config{})
	r.tick(t)
	r.now = t0.Add(time.Hour)
	r.api.spaces = []Space{{Name: dm, Type: "DIRECT_MESSAGE", LastActive: t0.Add(50 * time.Minute)}}
	r.api.msgs[dm] = []Message{msg(dm, "hi", "users/200", "hello there", t0.Add(50*time.Minute))}
	r.api.lastRead[dm] = t0.Add(55 * time.Minute) // read already, still news to KARMAX
	if got := r.tick(t); types(got) != "dm" {
		t.Fatalf("published %q, want the new DM", types(got))
	}
}

func TestOneSpaceFailingDoesNotStopTheOthers(t *testing.T) {
	r := newRig(Config{})
	r.api.spaces = []Space{
		{Name: dm, Type: "DIRECT_MESSAGE", LastActive: t0.Add(-48 * time.Hour)},
		{Name: team, Type: "SPACE", LastActive: t0.Add(-48 * time.Hour)},
	}
	r.tick(t)
	r.now = t0.Add(10 * time.Minute)
	for i := range r.api.spaces {
		r.api.spaces[i].LastActive = t0.Add(9 * time.Minute)
	}
	r.api.msgs[dm] = []Message{msg(dm, "x", "users/200", "hello", t0.Add(9*time.Minute))}
	r.api.msgs[team] = []Message{msg(team, "y", "users/300", "standup?", t0.Add(9*time.Minute))}
	r.api.msgErr[dm] = errors.New("permission denied")

	var got []sources.Event
	res, err := r.w.Tick(context.Background(), acct, func(e sources.Event) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatalf("one space failing failed the tick: %v", err)
	}
	if types(got) != "message" || len(res.Failed) != 1 || res.Failed[0] != dm {
		t.Fatalf("published %q, failed %v", types(got), res.Failed)
	}
	// It is read again next time from where it was.
	r.now = t0.Add(12 * time.Minute)
	delete(r.api.msgErr, dm)
	if got := r.tick(t); types(got) != "dm" {
		t.Fatalf("the failed space's message was lost: %q", types(got))
	}
}

// Without knowing who the operator is, their own messages cannot be told
// apart, so nothing is published.
func TestWithoutKnowingWhoTheOperatorIsNothingIsPublished(t *testing.T) {
	r := newRig(Config{})
	r.api.me, r.api.meErr = Me{}, errors.New("people api disabled")
	_, err := r.w.Tick(context.Background(), acct, func(sources.Event) error { return nil })
	if err == nil {
		t.Fatal("published without knowing who the operator is")
	}
	if _, ok := r.store[acct]; ok {
		t.Fatal("state saved, so the first look would be skipped")
	}
}

// A path that cannot say when a space was last active is probed a few spaces
// a tick, round the list.
func TestSpacesWithoutActivityTimesAreProbedInTurn(t *testing.T) {
	r := newRig(Config{MaxSpaces: 2})
	r.api.spaces = []Space{{Name: "spaces/A"}, {Name: "spaces/B"}, {Name: "spaces/C"}}
	r.tick(t)
	r.now = t0.Add(time.Minute)
	r.tick(t)
	for _, s := range []string{"spaces/A", "spaces/B", "spaces/C"} {
		if r.api.calls["messages:"+s] == 0 {
			t.Errorf("%s was never probed in two ticks of two", s)
		}
	}
}

func TestSenderNamesAreLookedUpOnce(t *testing.T) {
	r := newRig(Config{})
	r.api.spaces = []Space{{Name: team, Type: "SPACE", LastActive: t0.Add(-48 * time.Hour)}}
	r.tick(t)
	r.api.people["users/300"] = Person{Name: "Anita Rao", Email: "anita@example.com"}
	for i := 1; i <= 2; i++ {
		r.now = t0.Add(time.Duration(i) * 10 * time.Minute)
		r.api.spaces[0].LastActive = r.now.Add(-time.Minute)
		r.api.msgs[team] = append(r.api.msgs[team], msg(team, strings.Repeat("z", i), "users/300", "update", r.now.Add(-time.Minute)))
		got := r.tick(t)
		if len(got) != 1 || !strings.Contains(got[0].Title, "Anita Rao") {
			t.Fatalf("tick %d: %v", i, got)
		}
	}
	if n := r.api.calls["person:users/300"]; n != 1 {
		t.Errorf("looked the same sender up %d times", n)
	}
}

// Who wrote a message is decided by the account's own Chat user when both
// sides have one; a name decides only when the path gives no user at all.
func TestMeWrote(t *testing.T) {
	me := Me{User: "users/100", Name: "Quinn Avery"}
	for _, c := range []struct {
		m    Message
		want bool
	}{
		{Message{Sender: "users/100"}, true},
		{Message{Sender: "users/7", SenderName: "Quinn Avery"}, false},
		{Message{SenderName: "quinn avery "}, true},
		{Message{SenderName: "Dana Reyes"}, false},
		{Message{}, false},
	} {
		if got := me.Wrote(c.m); got != c.want {
			t.Errorf("Wrote(%+v) = %v, want %v", c.m, got, c.want)
		}
	}
	if (Me{Name: "Quinn Avery"}).Wrote(Message{Sender: "users/100", SenderName: "Quinn Avery"}) {
		t.Error("with no user of its own, the account claimed a message that names its user")
	}
}
