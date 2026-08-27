package slack

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
)

// DirectoryUser is one workspace member, shaped for whoever maps a Slack id
// onto an org member — directory_store.go owns that mapping, this just lists.
type DirectoryUser struct {
	ID       string
	Name     string
	RealName string
	Email    string
	IsBot    bool
	Deleted  bool
}

// ListUsers returns every workspace member via users.list. Pagination is the
// SDK's job (GetUsersContext already loops the cursor and backs off on a rate
// limit); this just shapes the result.
func (c *Channel) ListUsers(ctx context.Context) ([]DirectoryUser, error) {
	if c.api == nil {
		return nil, fmt.Errorf("slack: not connected")
	}
	users, err := c.api.GetUsersContext(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DirectoryUser, 0, len(users))
	for _, u := range users {
		out = append(out, toDirectoryUser(u))
	}
	return out, nil
}

func toDirectoryUser(u slack.User) DirectoryUser {
	return DirectoryUser{
		ID:       u.ID,
		Name:     u.Name,
		RealName: u.Profile.RealName,
		Email:    u.Profile.Email,
		IsBot:    u.IsBot,
		Deleted:  u.Deleted,
	}
}
