package slack

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
)

// toDirectoryUser carries over exactly the fields directory_store.go needs to
// map a Slack id to an org member.
func TestToDirectoryUser(t *testing.T) {
	u := slack.User{
		ID:      "U1",
		Name:    "maya",
		IsBot:   false,
		Deleted: false,
	}
	u.Profile.RealName = "Maya Iyer"
	u.Profile.Email = "maya@example.com"

	got := toDirectoryUser(u)
	want := DirectoryUser{ID: "U1", Name: "maya", RealName: "Maya Iyer", Email: "maya@example.com"}
	if got != want {
		t.Errorf("toDirectoryUser() = %+v, want %+v", got, want)
	}
}

// ListUsers refuses to guess at a live API rather than panicking on a nil
// client.
func TestListUsersRequiresAConnection(t *testing.T) {
	c := &Channel{}
	if _, err := c.ListUsers(context.Background()); err == nil {
		t.Fatal("ListUsers() with no connection should have failed")
	}
}
