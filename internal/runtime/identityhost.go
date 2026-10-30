package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/identity"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"go.uber.org/zap"
)

// The host side of internal/identity: building the directory from
// configuration, keeping what it learns, asking the WhatsApp bridge which
// account it is linked to, and labelling the threads models read.

// newIdentity builds the directory: the operator's name from configuration,
// their accounts from the operator chats, and the aliases learned before.
func newIdentity(getenv func(string) string, operatorChats []string, s *store.Store) *identity.Directory {
	d := identity.New(identity.NameFromEnv(getenv), operatorChats)
	if s != nil {
		d.SetAliasStore(identityAliases{s: s})
	}
	return d
}

// identityAliasGroup holds the operator's learned aliases. No expiry: an
// account does not stop being the operator's.
const identityAliasGroup = "identity-operator-aliases"

type identityAliases struct{ s *store.Store }

func (a identityAliases) LoadAliases() (map[string]string, error) {
	entries, err := a.s.KVList(identityAliasGroup)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		out[e.Key] = e.Value
	}
	return out, nil
}

func (a identityAliases) SaveAlias(alias, account string) error {
	return a.s.KVSet(identityAliasGroup, alias, account, 0)
}

// speakerLabeller labels one message of a thread with who wrote it.
//
// sent is KARMAX's own record of what it sent, which is the only way to tell
// its words from the operator's on the account they share; names looks up a
// contact's label. Either may be nil.
func speakerLabeller(who *identity.Directory, sent func(chat, text string, at time.Time) bool,
	names func(ctx context.Context, jid string) string) builtin.SpeakerLabeller {
	if who == nil {
		return nil
	}
	return func(ctx context.Context, m map[string]any) {
		sender, _ := m["sender_jid"].(string)
		fromMe, _ := m["is_from_me"].(bool)
		role := who.RoleOf(sender)
		karmax := false
		if (fromMe || role == identity.RoleLinked) && sent != nil {
			chat, _ := m["chat_jid"].(string)
			text, _ := m["content"].(string)
			if at, err := time.Parse(time.RFC3339, fmt.Sprint(m["timestamp"])); err == nil {
				karmax = sent(chat, text, at)
			}
		}
		label := ""
		if role == identity.RoleContact && names != nil {
			label = names(ctx, sender)
		}
		r, from := who.Attribute(sender, fromMe, karmax, label)
		m["sender_role"] = r
		m["from"] = from
	}
}

// linkedAccount asks the WhatsApp bridge which account it is linked to: its
// phone JID and its LID.
func linkedAccount(ctx context.Context, base string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bridge status: %s", resp.Status)
	}
	var st struct {
		UserJID string `json:"user_jid"`
		UserLID string `json:"user_lid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range []string{st.UserJID, st.UserLID} {
		if strings.TrimSpace(id) != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("the bridge is not linked to an account yet")
	}
	return ids, nil
}

// learnLinkedAccount asks the bridge for the linked account until it answers,
// then tells the directory and rewrites the shared session brief. In the
// background: the bridge may come up after KARMAX, and nothing waits on this.
func (rt *KarmaxRuntime) learnLinkedAccount(ctx context.Context, base string) {
	if rt.who == nil || strings.TrimSpace(base) == "" {
		return
	}
	go func() {
		wait := 5 * time.Second
		for {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ids, err := linkedAccount(cctx, base)
			cancel()
			if err == nil {
				rt.who.SetLinked(ids...)
				rt.refreshRootBrief()
				rt.log.Info("identity: learned the linked WhatsApp account", zap.Int("ids", len(ids)))
				return
			}
			rt.log.Debug("identity: the bridge did not say which account it is linked to yet", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if wait < 5*time.Minute {
				wait *= 2
			}
		}
	}()
}
