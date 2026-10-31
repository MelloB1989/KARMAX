package chatmemory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gchat"
	"go.uber.org/zap"
)

// fakeChatAPI is Google Chat for some accounts, as googleacct reads it.
type fakeChatAPI struct {
	me      map[string]gchat.Me
	meErr   map[string]error
	spaces  map[string][]gchat.Space
	msgs    map[string][]gchat.Message // by account|space
	people  map[string]gchat.Person    // by account|user: what that account can see
	lookups int
}

func (f *fakeChatAPI) Me(_ context.Context, account string) (gchat.Me, error) {
	if err := f.meErr[account]; err != nil {
		return gchat.Me{}, err
	}
	return f.me[account], nil
}

func (f *fakeChatAPI) Spaces(_ context.Context, account string) ([]gchat.Space, error) {
	return f.spaces[account], nil
}

func (f *fakeChatAPI) Messages(_ context.Context, account, space string, after time.Time, max int) ([]gchat.Message, error) {
	var out []gchat.Message
	for _, m := range f.msgs[account+"|"+space] {
		if m.Created.After(after) {
			out = append(out, m)
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out, nil
}

func (f *fakeChatAPI) LastRead(context.Context, string, string) (time.Time, error) {
	return time.Time{}, nil
}

func (f *fakeChatAPI) Person(_ context.Context, account, user string) (gchat.Person, error) {
	f.lookups++
	p, ok := f.people[account+"|"+user]
	if !ok {
		return gchat.Person{}, errors.New("not found")
	}
	return p, nil
}

const (
	work = "quinn@example.org"
	side = "quinn@example.net"
)

func newChatAPI() *fakeChatAPI {
	return &fakeChatAPI{
		me: map[string]gchat.Me{
			work: {User: "users/100", Name: "Quinn Avery", Email: work},
			side: {User: "users/200", Name: "Quinn Avery", Email: side},
		},
		meErr:  map[string]error{},
		spaces: map[string][]gchat.Space{},
		msgs:   map[string][]gchat.Message{},
		people: map[string]gchat.Person{},
	}
}

func accounts(list ...string) func(context.Context) []string {
	return func(context.Context) []string { return list }
}

func TestGChatListsEverySpaceOfEveryAccount(t *testing.T) {
	api := newChatAPI()
	api.spaces[work] = []gchat.Space{
		{Name: "spaces/AAAA", DisplayName: "Launch crew", Type: "SPACE", LastActive: t0},
		{Name: "spaces/DDDD", Type: "DIRECT_MESSAGE", LastActive: t0.Add(-time.Hour)},
	}
	api.spaces[side] = []gchat.Space{{Name: "spaces/GGGG", DisplayName: "", Type: "GROUP_CHAT", LastActive: t0}}
	src := NewGChat(api, accounts(work, side), zap.NewNop())

	chats, err := src.Chats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 3 {
		t.Fatalf("chats = %+v", chats)
	}
	want := []Chat{
		{ID: "spaces/AAAA", Account: work, Name: "Launch crew", IsGroup: true, Kind: "space", LastMessageAt: t0},
		{ID: "spaces/DDDD", Account: work, IsGroup: false, Kind: "direct message", LastMessageAt: t0.Add(-time.Hour)},
		{ID: "spaces/GGGG", Account: side, IsGroup: true, Kind: "group chat", LastMessageAt: t0},
	}
	for i, w := range want {
		if chats[i] != w {
			t.Errorf("chat %d = %+v, want %+v", i, chats[i], w)
		}
	}
}

// Every message is read, the operator's own included, and the operator is
// told apart by the account's own Chat user — never by a name.
func TestGChatReadsEveryoneAndKnowsTheOperatorByTheirAccount(t *testing.T) {
	api := newChatAPI()
	api.msgs[work+"|spaces/AAAA"] = []gchat.Message{
		{Name: "spaces/AAAA/messages/1", Sender: "users/100", Text: "I'll own the rollout", Created: t0},
		{Name: "spaces/AAAA/messages/2", Sender: "users/7", SenderName: "Quinn Avery", Text: "a namesake writes", Created: t0.Add(time.Minute)},
		{Name: "spaces/AAAA/messages/3", Sender: "users/8", SenderName: "Build bot", SenderType: "BOT", Text: "deploy finished", Created: t0.Add(2 * time.Minute)},
	}
	src := NewGChat(api, accounts(work), zap.NewNop())

	got, err := src.Messages(context.Background(), Chat{ID: "spaces/AAAA", Account: work}, t0.Add(-time.Hour), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("messages = %+v", got)
	}
	if !got[0].FromMe || got[0].ID != "spaces/AAAA/messages/1" || got[0].Content != "I'll own the rollout" {
		t.Errorf("the operator's own message = %+v", got[0])
	}
	if got[1].FromMe {
		t.Error("someone sharing the operator's name was taken for the operator")
	}
	if got[2].FromMe || got[2].SenderName != "Build bot (an app)" {
		t.Errorf("an app's message = %+v; want it marked as an app", got[2])
	}
}

// Without user ids (the gog path), the operator's messages are known by the
// name the account's own profile gives.
func TestGChatWithoutUserIDsKnowsTheOperatorByTheirProfileName(t *testing.T) {
	api := newChatAPI()
	api.me[work] = gchat.Me{Name: "Quinn Avery", Email: work}
	api.msgs[work+"|spaces/AAAA"] = []gchat.Message{
		{Name: "m1", SenderName: "Quinn Avery", Text: "mine", Created: t0},
		{Name: "m2", SenderName: "Dana Reyes", Text: "theirs", Created: t0.Add(time.Minute)},
	}
	src := NewGChat(api, accounts(work), zap.NewNop())

	got, err := src.Messages(context.Background(), Chat{ID: "spaces/AAAA", Account: work}, t0.Add(-time.Hour), 50)
	if err != nil || len(got) != 2 {
		t.Fatalf("messages = %+v, %v", got, err)
	}
	if !got[0].FromMe || got[1].FromMe {
		t.Errorf("from me = %v, %v; want the first only", got[0].FromMe, got[1].FromMe)
	}
}

// A sender is named once, through an account that saw them, and remembered.
func TestGChatNamesASenderOnceThroughAnAccountThatSawThem(t *testing.T) {
	api := newChatAPI()
	api.msgs[side+"|spaces/GGGG"] = []gchat.Message{{Name: "m1", Sender: "users/7", Text: "hi", Created: t0}}
	api.msgs[side+"|spaces/HHHH"] = []gchat.Message{{Name: "m2", Sender: "users/9", Text: "hello", Created: t0}}
	api.people[side+"|users/7"] = gchat.Person{Name: "Dana Reyes", Email: "dana@example.net"}
	api.people[side+"|users/9"] = gchat.Person{Email: "lee@example.net"}
	src := NewGChat(api, accounts(work, side), zap.NewNop())
	ctx := context.Background()
	for _, sp := range []string{"spaces/GGGG", "spaces/HHHH"} {
		if _, err := src.Messages(ctx, Chat{ID: sp, Account: side}, t0.Add(-time.Hour), 50); err != nil {
			t.Fatal(err)
		}
	}

	if n := src.Name(ctx, "users/7"); n != "Dana Reyes" {
		t.Errorf("name = %q", n)
	}
	if n := src.Name(ctx, "users/7"); n != "Dana Reyes" || api.lookups != 1 {
		t.Errorf("name = %q after %d lookups; want one lookup, remembered", n, api.lookups)
	}
	if n := src.Name(ctx, "users/9"); n != "lee@example.net" {
		t.Errorf("a person with no name = %q, want their address", n)
	}
	if n := src.Name(ctx, "users/404"); n != "" {
		t.Errorf("a sender never seen = %q", n)
	}
}

// An account whose own Chat user cannot be told is not read: its operator's
// messages would be taken for someone else's. The other accounts still are.
func TestGChatSkipsAnAccountThatCannotSayWhoTheOperatorIs(t *testing.T) {
	api := newChatAPI()
	api.meErr[side] = errors.New("signed out")
	api.spaces[work] = []gchat.Space{{Name: "spaces/AAAA", Type: "SPACE", LastActive: t0}}
	api.spaces[side] = []gchat.Space{{Name: "spaces/GGGG", Type: "SPACE", LastActive: t0}}
	src := NewGChat(api, accounts(work, side), zap.NewNop())

	chats, err := src.Chats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0].Account != work {
		t.Errorf("chats = %+v; want only the account that could be read", chats)
	}

	api.meErr[work] = errors.New("signed out")
	src = NewGChat(api, accounts(work, side), zap.NewNop())
	if _, err := src.Chats(context.Background()); err == nil {
		t.Error("no account could be read, and that was not an error")
	}
}

// A path that cannot say when a space was last active has it read in turn
// rather than never.
func TestGChatASpaceOfUnknownActivityIsRead(t *testing.T) {
	api := newChatAPI()
	api.spaces[work] = []gchat.Space{{Name: "spaces/AAAA", Type: "SPACE"}}
	src := NewGChat(api, accounts(work), zap.NewNop())
	src.now = func() time.Time { return t0 }

	chats, err := src.Chats(context.Background())
	if err != nil || len(chats) != 1 || !chats[0].LastMessageAt.Equal(t0) {
		t.Fatalf("chats = %+v, %v; want it counted as active now", chats, err)
	}
}

// Through the syncer: a Google Chat direct message reaches memory with the
// operator's own words as theirs, the other person by name, and the service
// named.
func TestGChatThroughTheSyncer(t *testing.T) {
	api := newChatAPI()
	api.spaces[work] = []gchat.Space{{Name: "spaces/DDDD", Type: "DIRECT_MESSAGE", LastActive: t0.Add(time.Minute)}}
	api.msgs[work+"|spaces/DDDD"] = []gchat.Message{
		{Name: "spaces/DDDD/messages/1", Sender: "users/7", Text: "can you send the Q4 plan by monday?", Created: t0},
		{Name: "spaces/DDDD/messages/2", Sender: "users/100", Text: "yes, by monday noon", Created: t0.Add(time.Minute)},
	}
	api.people[work+"|users/7"] = gchat.Person{Name: "Dana Reyes"}
	rec := &recorder{}
	s := New(Config{Name: "gchat-memory", Service: "Google Chat", Tag: "gchat", Operator: "Quinn",
		Since: t0.Add(-time.Hour), Location: time.UTC},
		NewGChat(api, accounts(work), zap.NewNop()), &fakeMemory{}, memCursors{}, nil, rec.ask, zap.NewNop())

	if _, err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.prompts) != 1 {
		t.Fatalf("prompts = %d", len(rec.prompts))
	}
	p := rec.prompts[0]
	for _, want := range []string{"CHAT: Dana Reyes (a Google Chat direct message)",
		"Dana Reyes: can you send the Q4 plan", "Quinn: yes, by monday noon"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Quinn Avery") {
		t.Errorf("the operator was named by their profile, not as the operator:\n%s", p)
	}
}
