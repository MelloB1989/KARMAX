package comms

import (
	"context"
	"errors"
	"strings"
	"time"
)

// MessageDirection indicates whether a message was received or sent.
type MessageDirection string

const (
	Inbound  MessageDirection = "inbound"
	Outbound MessageDirection = "outbound"
)

// Message represents a single communication message across any channel.
type Message struct {
	ID          string
	ChannelID   string
	ChannelType string
	SenderID    string
	SenderName  string
	Content     string
	Direction   MessageDirection
	ReplyToID   string
	Attachments []Attachment
	Metadata    map[string]any
	Timestamp   time.Time
}

// Attachment holds file data associated with a message.
type Attachment struct {
	Filename string
	URL      string
	MimeType string
	Data     []byte
}

// Embed represents a rich embed message (e.g., Discord embeds).
type Embed struct {
	Title       string
	Description string
	Color       int
	Fields      []EmbedField
	Footer      string
}

// EmbedField is a single field within an Embed.
type EmbedField struct {
	Name   string
	Value  string
	Inline bool
}

// Channel is the interface every communication backend must implement.
type Channel interface {
	ID() string
	Type() string
	Start(ctx context.Context) error
	Stop() error
	Send(ctx context.Context, target string, content string) error
	SendEmbed(ctx context.Context, target string, embed Embed) error
	SendFile(ctx context.Context, target string, filename string, data []byte) error
	IncomingMessages() <-chan Message
}

// UnresolvedTargetError says the recipient could not be found — a bad address,
// not a broken channel.
//
// The distinction decides whether the operator is woken. A send that fails
// because the transport is down is a system fault worth a critical alert on
// another channel; a send that fails because the caller named something that is
// not a person is a caller mistake, and paging the operator about it turns one
// bad tool call into an alert on their phone.
type UnresolvedTargetError struct {
	Target string
	Reason string
}

func (e *UnresolvedTargetError) Error() string {
	if e.Reason == "" {
		return "no such recipient: " + e.Target
	}
	return "no such recipient " + e.Target + ": " + e.Reason
}

// TargetCandidate is one conversation a name might have meant.
type TargetCandidate struct {
	JID     string `json:"jid"`
	Name    string `json:"name"`
	Phone   string `json:"phone,omitempty"`
	IsGroup bool   `json:"is_group"`
}

// AmbiguousTargetError says a name matched several conversations.
//
// Distinct from UnresolvedTargetError because the fix is different: nothing
// matched means find another address, several matched means pick one. Sending
// to a guess is the one option not on the table — a message to the wrong "Dev"
// cannot be taken back — so the candidates travel with the error and whoever
// asked gets to choose.
type AmbiguousTargetError struct {
	Target     string
	Candidates []TargetCandidate
}

func (e *AmbiguousTargetError) Error() string {
	names := make([]string, 0, len(e.Candidates))
	for i, c := range e.Candidates {
		if i == 5 {
			break
		}
		label := c.Name
		if label == "" {
			label = c.JID
		}
		names = append(names, label+" <"+c.JID+">")
	}
	return "\"" + e.Target + "\" matches several conversations: " + strings.Join(names, ", ")
}

// isAddressingMistake reports whether a send failed because of who it was
// addressed to rather than because the channel is broken.
//
// One list, so a new addressing error cannot be added without the send path
// learning about it — the first version checked only "matched nothing" and let
// "matched several" page the operator, which is the same failure wearing a
// different type.
func isAddressingMistake(err error) bool {
	var unresolved *UnresolvedTargetError
	var ambiguous *AmbiguousTargetError
	return errors.As(err, &unresolved) || errors.As(err, &ambiguous)
}
