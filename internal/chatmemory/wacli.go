package chatmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Wacli reads WhatsApp history from wacli's HTTP API.
type Wacli struct {
	base string
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	names map[string]string
}

// NewWacli reads from the wacli API at base.
func NewWacli(base string) *Wacli {
	return &Wacli{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 60 * time.Second},
		now: time.Now, names: map[string]string{}}
}

func (w *Wacli) get(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wacli %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Chats lists every conversation wacli holds. Channels and status updates
// are broadcast at the operator, not said to them, and are left out; a direct
// chat with no name is named by its number.
func (w *Wacli) Chats(ctx context.Context) ([]Chat, error) {
	var out struct {
		Chats []struct {
			ID            string `json:"jid"`
			Name          string `json:"name"`
			IsGroup       bool   `json:"is_group"`
			LastMessageAt string `json:"last_message_at"`
		} `json:"chats"`
	}
	if err := w.get(ctx, "/chats", url.Values{"limit": {"100000"}}, &out); err != nil {
		return nil, err
	}
	chats := make([]Chat, 0, len(out.Chats))
	for _, c := range out.Chats {
		if strings.HasSuffix(c.ID, "@newsletter") || strings.HasSuffix(c.ID, "@broadcast") {
			continue
		}
		at, _ := time.Parse(time.RFC3339, c.LastMessageAt)
		name := c.Name
		if name == "" && !c.IsGroup {
			name = shownID(bare(c.ID))
		}
		chats = append(chats, Chat{ID: c.ID, Name: name, IsGroup: c.IsGroup, LastMessageAt: at})
	}
	return chats, nil
}

// firstWindow is how far ahead a read first looks; narrowed when it fills.
const firstWindow = 7 * 24 * time.Hour

// Messages returns up to limit of a chat's messages after a time, oldest first.
//
// wacli answers a bounded query with the NEWEST messages inside the bounds, so
// asking for what follows a point in a long history would return today's
// messages and skip everything between. Reading forward instead takes a window
// ahead of the cursor, narrows it while it comes back full, and slides past
// windows with nothing in them.
func (w *Wacli) Messages(ctx context.Context, c Chat, after time.Time, limit int) ([]Message, error) {
	chat := c.ID
	if limit <= 0 {
		limit = 100
	}
	now := w.now()
	window := firstWindow
	for after.Before(now) {
		before := after.Add(window)
		got, err := w.page(ctx, chat, after, before, limit)
		if err != nil {
			return nil, err
		}
		switch {
		case len(got) >= limit && window > time.Minute:
			window /= 4 // full: there may be older ones the newest-first limit cut off
		case len(got) > 0:
			return got, nil
		default:
			after, window = before, window*2 // nothing here; look further
		}
	}
	return nil, nil
}

func (w *Wacli) page(ctx context.Context, chat string, after, before time.Time, limit int) ([]Message, error) {
	var out struct {
		Messages []struct {
			ID        string `json:"id"`
			Chat      string `json:"chat_jid"`
			Sender    string `json:"sender_jid"`
			Content   string `json:"content"`
			Timestamp string `json:"timestamp"`
			IsFromMe  bool   `json:"is_from_me"`
		} `json:"messages"`
	}
	q := url.Values{"chat_ref": {chat}, "after": {after.Format(time.RFC3339)},
		"before": {before.Format(time.RFC3339)}, "limit": {strconv.Itoa(limit)}}
	if err := w.get(ctx, "/messages", q, &out); err != nil {
		return nil, err
	}
	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		at, err := time.Parse(time.RFC3339, m.Timestamp)
		if err != nil {
			continue
		}
		msgs = append(msgs, Message{ID: m.ID, Chat: m.Chat, Sender: m.Sender,
			Content: m.Content, At: at, FromMe: m.IsFromMe})
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].At.Before(msgs[j].At) })
	return msgs, nil
}

// Name is the display name wacli resolves for a sender, cached; "" for none.
func (w *Wacli) Name(ctx context.Context, jid string) string {
	key := bare(jid)
	w.mu.Lock()
	n, ok := w.names[key]
	w.mu.Unlock()
	if ok {
		return n
	}
	user, server, _ := strings.Cut(jid, "@")
	user, _, _ = strings.Cut(user, ":")
	var out struct {
		Matches []struct {
			ID   string `json:"jid"`
			Name string `json:"name"`
		} `json:"matches"`
	}
	if err := w.get(ctx, "/resolve", url.Values{"ref": {user + "@" + server}}, &out); err == nil {
		for _, m := range out.Matches {
			if bare(m.ID) == key && strings.TrimSpace(m.Name) != "" {
				n = strings.TrimSpace(m.Name)
				break
			}
		}
	}
	w.mu.Lock()
	w.names[key] = n
	w.mu.Unlock()
	return n
}
