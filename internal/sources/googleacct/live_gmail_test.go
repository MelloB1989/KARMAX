package googleacct

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/sources/gmail"
)

type liveStore map[string]gmail.State

func (m liveStore) Load(a string) (gmail.State, bool, error) { st, ok := m[a]; return st, ok, nil }
func (m liveStore) Save(a string, st gmail.State) error      { m[a] = st; return nil }

func liveSeen() sources.Seen {
	kv := map[string]string{}
	return sources.Seen{Group: "live",
		Get: func(g, k string) (string, bool, error) { v, ok := kv[g+"/"+k]; return v, ok, nil },
		Set: func(g, k, v string, _ time.Duration) error { kv[g+"/"+k] = v; return nil }}
}

// The Gmail source against this machine's real Google logins, read-only:
// a first look over a throwaway store, then the last stretch of history
// replayed as though KARMAX had been watching for two days. Nothing is
// published, sent or changed. Prints counts and flags — never an address,
// a sender, a subject or a snippet. Skipped unless KARMAX_LIVE_GMAIL is set:
//
//	KARMAX_LIVE_GMAIL=1 go test ./internal/sources/googleacct/ -run LiveGmail -v
func TestLiveGmailAgainstTheRealLogins(t *testing.T) {
	if os.Getenv("KARMAX_LIVE_GMAIL") == "" {
		t.Skip("set KARMAX_LIVE_GMAIL=1 to read this machine's real Gmail (read-only, counts only)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h := NewHost(hostpaths.Gog(), os.Getenv)
	c := NewClient(h, time.Local)
	d := h.Accounts(ctx)
	t.Logf("accounts: %d, discovery errors: %d", len(d.Accounts), len(d.Errors))
	for i, a := range d.Accounts {
		name := fmt.Sprintf("account %d", i+1)
		if a.Lacks(GmailRead) {
			t.Logf("%s: no login was granted Gmail", name)
			continue
		}
		t.Logf("%s: read bodies %v, label %v, send %v", name, !a.Lacks(GmailBody), !a.Lacks(GmailModify), !a.Lacks(GmailSend))

		run := func(label string, store liveStore) {
			var evs []sources.Event
			w := gmail.New(gmail.Config{Own: d.Emails}, c, store, liveSeen())
			start := time.Now()
			res, err := w.Tick(ctx, a.Email, func(e sources.Event) error { evs = append(evs, e); return nil })
			if err != nil {
				t.Errorf("%s %s: %s", name, label, strings.ReplaceAll(err.Error(), a.Email, name))
				return
			}
			t.Logf("%s %s (%s): first %v expired %v more %v; listed %d, read %d, failed %d, published %d (urgent %d, backlog %d); skipped %v",
				name, label, time.Since(start).Round(time.Second), res.First, res.Expired, res.More, res.Listed, res.Read,
				len(res.Failed), res.Published, res.Urgent, res.Backlog, res.Skipped)
			flags := map[string]int{}
			for _, e := range evs {
				flags["type="+e.Type]++
				for _, k := range []string{"important", "addressed_directly", "operator_in_thread", "automated", "unread"} {
					if e.Data[k] == true {
						flags[k]++
					}
				}
				if e.Data["attachments"] != nil {
					flags["with_attachments"]++
				}
				labels, _ := e.Data["labels"].([]string)
				cat := "no category"
				for _, l := range labels {
					if strings.HasPrefix(l, "CATEGORY_") {
						cat = strings.ToLower(strings.TrimPrefix(l, "CATEGORY_"))
					}
				}
				flags["category="+cat]++
				if e.Subject() == "" || e.Category() != "people" {
					t.Errorf("an event without a memory subject or category")
				}
			}
			keys := make([]string, 0, len(flags))
			for k := range flags {
				keys = append(keys, fmt.Sprintf("%s:%d", k, flags[k]))
			}
			sort.Strings(keys)
			t.Logf("%s %s published: %s", name, label, strings.Join(keys, " "))
		}

		first := liveStore{}
		run("first look", first)
		hid, err := strconv.ParseUint(first[a.Email].HistoryID, 10, 64)
		if err != nil || hid < 5000 {
			continue
		}
		run("history replay", liveStore{a.Email: {FirstSeen: time.Now().Add(-48 * time.Hour),
			HistoryID: fmt.Sprint(hid - 4000), LastTick: time.Now().Add(-time.Hour)}})
	}
}
