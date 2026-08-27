package connectorkit

import "context"

// Who the agent is acting for, on this turn.
//
// Most connectors do not care: a Slack bot token is the same token whoever
// asked. Google is different — the org registers one OAuth app and every
// employee authorises their own mailbox against it, so "read my calendar"
// has as many answers as there are employees, and answering with the wrong
// one is not a permissions bug to tighten later. It is the wrong answer,
// returned confidently, to a question about someone's private data.
//
// Carried in the context rather than as a tool argument on purpose. A tool
// argument is something the MODEL fills in, and the model must not be able to
// choose whose mailbox it reads: the actor is established by whoever sent the
// message, before the model sees anything.

type actorKey struct{}

// WithActor marks a context as acting for one org member.
func WithActor(ctx context.Context, member string) context.Context {
	if member == "" {
		return ctx
	}
	return context.WithValue(ctx, actorKey{}, member)
}

// ActorFrom returns the member this context acts for, or "" when the work is
// not on anybody's behalf — a scheduled loop, a webhook, a console action.
func ActorFrom(ctx context.Context) string {
	member, _ := ctx.Value(actorKey{}).(string)
	return member
}
