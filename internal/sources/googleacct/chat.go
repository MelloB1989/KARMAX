package googleacct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources/gchat"
)

// apiMessage is a Chat message as the API writes it.
type apiMessage struct {
	Name         string `json:"name"`
	Text         string `json:"text"`
	ArgumentText string `json:"argumentText"`
	CreateTime   string `json:"createTime"`
	Sender       struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		DisplayName string `json:"displayName"`
	} `json:"sender"`
	Thread struct {
		Name string `json:"name"`
	} `json:"thread"`
	Space struct {
		Name string `json:"name"`
	} `json:"space"`
	Annotations []struct {
		Type        string `json:"type"`
		UserMention struct {
			User struct {
				Name string `json:"name"`
			} `json:"user"`
			Type string `json:"type"`
		} `json:"userMention"`
	} `json:"annotations"`
	DeletionMetadata json.RawMessage `json:"deletionMetadata"`
}

func (m apiMessage) toMessage(space string) gchat.Message {
	out := gchat.Message{Name: m.Name, Space: space, Thread: m.Thread.Name, Text: m.Text,
		Sender: m.Sender.Name, SenderName: m.Sender.DisplayName, SenderType: m.Sender.Type,
		Created: parseStamp(m.CreateTime), Mentions: []string{}}
	if out.Text == "" {
		out.Text = m.ArgumentText
	}
	if m.Space.Name != "" {
		out.Space = m.Space.Name
	}
	for _, a := range m.Annotations {
		if a.Type == "USER_MENTION" && a.UserMention.User.Name != "" && a.UserMention.Type != "ADD" {
			out.Mentions = append(out.Mentions, a.UserMention.User.Name)
		}
	}
	return out
}

type apiPerson struct {
	ResourceName string `json:"resourceName"`
	Names        []struct {
		DisplayName string `json:"displayName"`
	} `json:"names"`
	EmailAddresses []struct {
		Value string `json:"value"`
	} `json:"emailAddresses"`
}

func (p apiPerson) person() gchat.Person {
	var out gchat.Person
	if len(p.Names) > 0 {
		out.Name = p.Names[0].DisplayName
	}
	if len(p.EmailAddresses) > 0 {
		out.Email = p.EmailAddresses[0].Value
	}
	return out
}

// userOf turns a People API resource into the Chat user it is: Google uses
// the same id for both.
func userOf(resource string) string {
	if id, ok := strings.CutPrefix(resource, "people/"); ok && id != "" {
		return "users/" + id
	}
	return ""
}

// Me names the account's own Chat user (gchat.API).
func (c *Client) Me(ctx context.Context, account string) (gchat.Me, error) {
	var me gchat.Me
	err := c.with(ctx, account, "chat", func(l Login) error {
		var p apiPerson
		var err error
		if l.Path == Gws {
			err = c.gws(ctx, l, "people get", &p, "people", "people", "get",
				"--params", jsonArg(map[string]any{"resourceName": "people/me", "personFields": "names,emailAddresses"}))
		} else {
			var wrapped struct {
				Person apiPerson `json:"person"`
			}
			err = c.gog(ctx, l, "people me", &wrapped, "people", "me")
			p = wrapped.Person
		}
		var ce *cliError
		if err != nil && (!errors.As(err, &ce) || ce.auth) {
			return err
		}
		pp := p.person()
		me = gchat.Me{User: userOf(p.ResourceName), Name: pp.Name, Email: strings.ToLower(account)}
		if me.User == "" && l.Path == Gws {
			// The People API can be off in the OAuth app's project. A Chat
			// membership looked up by the account's own address names the
			// same user.
			me.User = c.gwsMemberID(ctx, l, account)
		}
		if me.User == "" && me.Name == "" {
			if err != nil {
				return err
			}
			return fmt.Errorf("could not tell which Chat user %s is", account)
		}
		return nil
	})
	return me, err
}

func (c *Client) gwsMemberID(ctx context.Context, l Login, account string) string {
	var listed struct {
		Spaces []struct {
			Name string `json:"name"`
		} `json:"spaces"`
	}
	if err := c.gws(ctx, l, "chat spaces list", &listed, "chat", "spaces", "list",
		"--params", jsonArg(map[string]any{"pageSize": 5})); err != nil {
		return ""
	}
	for _, sp := range listed.Spaces {
		var m struct {
			Member struct {
				Name string `json:"name"`
			} `json:"member"`
		}
		if err := c.gws(ctx, l, "chat members get", &m, "chat", "spaces", "members", "get",
			"--params", jsonArg(map[string]any{"name": sp.Name + "/members/" + account})); err == nil && m.Member.Name != "" {
			return m.Member.Name
		}
	}
	return ""
}

// Spaces lists the account's spaces and DMs (gchat.API).
func (c *Client) Spaces(ctx context.Context, account string) ([]gchat.Space, error) {
	var out []gchat.Space
	err := c.with(ctx, account, "chat", func(l Login) error {
		out = nil
		if l.Path == Gws {
			return c.gwsPages(ctx, l, "chat spaces list", func(raw []byte) error {
				var p struct {
					Spaces []struct {
						Name           string `json:"name"`
						DisplayName    string `json:"displayName"`
						SpaceType      string `json:"spaceType"`
						LastActiveTime string `json:"lastActiveTime"`
					} `json:"spaces"`
				}
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				for _, s := range p.Spaces {
					out = append(out, gchat.Space{Name: s.Name, DisplayName: s.DisplayName, Type: s.SpaceType,
						LastActive: parseStamp(s.LastActiveTime)})
				}
				return nil
			}, "chat", "spaces", "list", "--params", jsonArg(map[string]any{"pageSize": 1000}))
		}
		var listed struct {
			Spaces []struct {
				Resource string `json:"resource"`
				Name     string `json:"name"`
				Type     string `json:"type"`
			} `json:"spaces"`
		}
		if err := c.gog(ctx, l, "chat spaces list", &listed, "chat", "spaces", "list", "--all"); err != nil {
			return err
		}
		for _, s := range listed.Spaces {
			out = append(out, gchat.Space{Name: s.Resource, DisplayName: s.Name, Type: s.Type})
		}
		return nil
	})
	return out, err
}

// gogMessage is gog's reshaped message: sender is one string, a display name
// when Google gave one and the user resource otherwise.
type gogMessage struct {
	Resource   string `json:"resource"`
	Sender     string `json:"sender"`
	Text       string `json:"text"`
	CreateTime string `json:"createTime"`
	Thread     string `json:"thread"`
}

func (m gogMessage) toMessage(space string) gchat.Message {
	out := gchat.Message{Name: m.Resource, Space: space, Thread: m.Thread, Text: m.Text, Created: parseStamp(m.CreateTime)}
	if strings.HasPrefix(m.Sender, "users/") {
		out.Sender = m.Sender
	} else {
		out.SenderName = m.Sender
	}
	return out // Mentions stay nil: gog does not say, so the text is read instead
}

// Messages returns messages created after `after`, oldest first (gchat.API).
func (c *Client) Messages(ctx context.Context, account, space string, after time.Time, max int) ([]gchat.Message, error) {
	var out []gchat.Message
	err := c.with(ctx, account, "chat", func(l Login) error {
		out = nil
		if l.Path == Gws {
			params := map[string]any{"parent": space, "pageSize": max}
			if !after.IsZero() {
				params["filter"] = fmt.Sprintf("createTime > %q", after.UTC().Format(time.RFC3339Nano))
			}
			var p struct {
				Messages []apiMessage `json:"messages"`
			}
			if err := c.gws(ctx, l, "chat messages list", &p, "chat", "spaces", "messages", "list",
				"--params", jsonArg(params)); err != nil {
				return err
			}
			for _, m := range p.Messages {
				if len(m.DeletionMetadata) > 0 && string(m.DeletionMetadata) != "null" {
					continue
				}
				out = append(out, m.toMessage(space))
			}
			return nil
		}
		var listed struct {
			Messages []gogMessage `json:"messages"`
		}
		if err := c.gog(ctx, l, "chat messages list", &listed, "chat", "messages", "list", space,
			"--max", fmt.Sprint(max), "--order", "createTime desc"); err != nil {
			return err
		}
		for i := len(listed.Messages) - 1; i >= 0; i-- {
			m := listed.Messages[i].toMessage(space)
			if m.Created.After(after) {
				out = append(out, m)
			}
		}
		return nil
	})
	return out, err
}

// LastRead is when the operator last read a space (gchat.API).
func (c *Client) LastRead(ctx context.Context, account, space string) (time.Time, error) {
	var at time.Time
	err := c.with(ctx, account, "chat", func(l Login) error {
		if l.Path == Gws {
			var st struct {
				LastReadTime string `json:"lastReadTime"`
			}
			if err := c.gws(ctx, l, "chat read state", &st, "chat", "users", "spaces", "getSpaceReadState",
				"--params", jsonArg(map[string]any{"name": "users/me/" + space + "/spaceReadState"})); err != nil {
				return err
			}
			at = parseStamp(st.LastReadTime)
			return nil
		}
		// gog reads the read state only to filter by it: the oldest unread
		// message is just after the operator's last read.
		var listed struct {
			Messages []gogMessage `json:"messages"`
		}
		if err := c.gog(ctx, l, "chat unread", &listed, "chat", "messages", "list", space,
			"--unread", "--max", "1", "--order", "createTime asc"); err != nil {
			return err
		}
		if len(listed.Messages) == 0 {
			at = time.Now()
			return nil
		}
		at = parseStamp(listed.Messages[0].CreateTime).Add(-time.Nanosecond)
		return nil
	})
	return at, err
}

// Person names a Chat user (gchat.API).
func (c *Client) Person(ctx context.Context, account, user string) (gchat.Person, error) {
	id, ok := strings.CutPrefix(user, "users/")
	if !ok || id == "" {
		return gchat.Person{}, fmt.Errorf("not a user: %s", user)
	}
	var out gchat.Person
	err := c.with(ctx, account, "chat", func(l Login) error {
		var p apiPerson
		if l.Path == Gws {
			if err := c.gws(ctx, l, "people get", &p, "people", "people", "get",
				"--params", jsonArg(map[string]any{"resourceName": "people/" + id, "personFields": "names,emailAddresses"})); err != nil {
				return err
			}
		} else {
			var wrapped struct {
				Person *apiPerson `json:"person"`
			}
			raw := json.RawMessage{}
			if err := c.gog(ctx, l, "people get", &raw, "people", "get", "people/"+id); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Person != nil {
				p = *wrapped.Person
			} else {
				_ = json.Unmarshal(raw, &p)
			}
		}
		out = p.person()
		return nil
	})
	return out, err
}

// Sent is a message KARMAX sent.
type Sent struct {
	Name   string `json:"name"`
	Space  string `json:"space"`
	Thread string `json:"thread,omitempty"`
}

func sentOf(m apiMessage, space string) Sent {
	if m.Space.Name != "" {
		space = m.Space.Name
	}
	return Sent{Name: m.Name, Space: space, Thread: m.Thread.Name}
}

func (c *Client) gwsSend(ctx context.Context, l Login, space, thread, text string) (apiMessage, error) {
	params := map[string]any{"parent": space}
	body := map[string]any{"text": text}
	if thread != "" {
		params["messageReplyOption"] = "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
		body["thread"] = map[string]any{"name": thread}
	}
	var m apiMessage
	err := c.gws(ctx, l, "chat messages create", &m, "chat", "spaces", "messages", "create",
		"--params", jsonArg(params), "--json", jsonArg(body))
	return m, err
}

// Send posts text to a space as the account, in a thread when one is given.
func (c *Client) Send(ctx context.Context, account, space, thread, text string) (Sent, error) {
	if strings.TrimSpace(text) == "" {
		return Sent{}, fmt.Errorf("nothing to send")
	}
	if !strings.HasPrefix(space, "spaces/") {
		return Sent{}, fmt.Errorf("space must look like spaces/AAAA, not %q", space)
	}
	var out Sent
	err := c.with(ctx, account, "chat", func(l Login) error {
		if l.Path == Gws {
			m, err := c.gwsSend(ctx, l, space, thread, text)
			out = sentOf(m, space)
			return err
		}
		args := []string{"chat", "messages", "send", space, "--text", text}
		if thread != "" {
			args = append(args, "--thread", thread)
		}
		var wrapped struct {
			Message apiMessage `json:"message"`
		}
		err := c.gog(ctx, l, "chat messages send", &wrapped, args...)
		out = sentOf(wrapped.Message, space)
		return err
	})
	return out, err
}

// SendDirect writes to a person by address, in the DM the account has with
// them (made when there is none yet).
func (c *Client) SendDirect(ctx context.Context, account, to, text string) (Sent, error) {
	to = strings.TrimSpace(to)
	if !strings.Contains(to, "@") {
		return Sent{}, fmt.Errorf("a direct message needs the person's address, not %q", to)
	}
	if strings.TrimSpace(text) == "" {
		return Sent{}, fmt.Errorf("nothing to send")
	}
	var out Sent
	err := c.with(ctx, account, "chat", func(l Login) error {
		if l.Path == Gog {
			var wrapped struct {
				Message apiMessage `json:"message"`
			}
			err := c.gog(ctx, l, "chat dm send", &wrapped, "chat", "dm", "send", to, "--text", text)
			out = sentOf(wrapped.Message, "")
			return err
		}
		var sp struct {
			Name string `json:"name"`
		}
		err := c.gws(ctx, l, "chat find dm", &sp, "chat", "spaces", "findDirectMessage",
			"--params", jsonArg(map[string]any{"name": "users/" + to}))
		var ce *cliError
		if err != nil && errors.As(err, &ce) && ce.gone {
			err = c.gws(ctx, l, "chat setup dm", &sp, "chat", "spaces", "setup", "--json", jsonArg(map[string]any{
				"space":       map[string]any{"spaceType": "DIRECT_MESSAGE"},
				"memberships": []any{map[string]any{"member": map[string]any{"name": "users/" + to, "type": "HUMAN"}}},
			}))
		}
		if err != nil {
			return err
		}
		m, err := c.gwsSend(ctx, l, sp.Name, "", text)
		out = sentOf(m, sp.Name)
		return err
	})
	return out, err
}
