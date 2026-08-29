package runtime

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sandbox"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// Runtime support for the organisational Kit: where a message goes, who holds a
// role, and where a sandbox gets its credentials.

// sendToChannel posts into a comms channel, threading when asked.
//
// channel may be a registered channel id or a platform-native target such as
// "#eng"; the manager owns that resolution. thread is passed as part of the
// target so a channel that understands threads keeps the conversation in one
// place and one that does not simply posts.
func (rt *KarmaxRuntime) sendToChannel(ctx context.Context, channel, thread, content string) error {
	if strings.TrimSpace(channel) == "" {
		return fmt.Errorf("no channel to send to")
	}
	target := channel
	if thread != "" {
		target = channel + ":" + thread
	}
	id, ok := rt.comms.DefaultChannelID()
	if !ok {
		return fmt.Errorf("no comms channel is connected")
	}
	return rt.comms.Send(id, target, content)
}

// postToCase posts into a channel, threading when the case already has one, and
// returns the message id so the first message can become the thread.
func (rt *KarmaxRuntime) postToCase(ctx context.Context, channel, thread, text string) (string, error) {
	id, ok := rt.comms.DefaultChannelID()
	if !ok {
		return "", fmt.Errorf("no comms channel is connected")
	}
	return rt.comms.PostThread(ctx, id, channel, thread, text)
}

// askRole puts one proposal in front of everyone holding a role.
//
// The proposal is already stored; this is only the asking. Whoever answers
// first settles it, which is what stops an approval waiting on one person's
// return from leave.
func (rt *KarmaxRuntime) askRole(ctx context.Context, role string, members []store.OrgMember, id, title, summary string) {
	body := fmt.Sprintf("*%s*\n%s\n\n_approval %s — first answer decides_", title, summary, id)
	for _, m := range members {
		target := m.Member
		if dm, found, err := rt.store.MemberByExternal("slack", m.Member); err == nil && found {
			target = dm.ExternalID
		}
		if err := rt.sendToChannel(ctx, target, "", body); err != nil {
			rt.log.Warn("could not ask a role member for approval",
				zap.String("role", role), zap.String("member", m.Member), zap.Error(err))
		}
	}
}

// emitCaseState publishes a case's move so a notify workflow can hang off it,
// rather than every workflow having to remember to announce itself.
func (rt *KarmaxRuntime) emitCaseState(caseID, state string) {
	c, found, err := rt.store.CaseByID(caseID)
	if err != nil || !found {
		return
	}
	if err := rt.bus.Publish(bus.Event{
		Kind: bus.EventKind(EventCaseStateChanged),
		Payload: map[string]any{
			"case_id": c.ID, "key": c.Key, "agent": c.Agent,
			"title": c.Title, "state": state,
			"thread_channel": c.ThreadChannel, "thread_ts": c.ThreadTS,
		},
	}); err != nil {
		rt.log.Warn("could not publish a case state change", zap.String("case", caseID), zap.Error(err))
	}
}

// EventCaseStateChanged is what dev-notify style workflows listen on.
const EventCaseStateChanged = "case.state_changed"

var (
	sandboxOnce sync.Once
	sandboxDrv  sandbox.Driver
	sandboxErr  error
)

// sandboxDriver resolves the configured driver once. Docker runs the container
// on this host; ECS runs it as a Fargate task, which is what you want as soon
// as the sandbox would be sharing a machine with the daemon supervising it.
// Kubernetes is the same interface and is not written.
func (rt *KarmaxRuntime) sandboxDriver() (sandbox.Driver, error) {
	sandboxOnce.Do(func() {
		name := strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_DRIVER"))
		if name == "" {
			name = "docker"
		}
		sandboxDrv, sandboxErr = sandbox.Open(name)
	})
	return sandboxDrv, sandboxErr
}

func (rt *KarmaxRuntime) sandboxImage() string {
	if v := strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_IMAGE")); v != "" {
		return v
	}
	return "karmax/sandbox:latest"
}

// sandboxCredentials assembles what the container needs to work and nothing
// else.
//
// The coding agent's token is the org's, stored once. The git token is minted
// per run and narrowed to the one repository this task may touch — which is the
// whole reason the GitHub connector uses an App rather than a personal token.
func (rt *KarmaxRuntime) sandboxCredentials(ctx context.Context, repo string, extra map[string]string) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range extra {
		env[k] = v
	}

	tok, err := rt.codingAgentToken()
	if err != nil {
		return nil, err
	}
	env[tok.name] = tok.value

	if repo != "" {
		gh, err := rt.repoToken(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("could not mint a token for %s: %w", repo, err)
		}
		env["GITHUB_TOKEN"] = gh
	}
	return env, nil
}

type namedSecret struct{ name, value string }

// RepoTokenMinter mints a git credential scoped to one repository. The GitHub
// connector installs one at startup when an App is configured.
type RepoTokenMinter func(ctx context.Context, repo string) (string, error)

// SetRepoTokenMinter is how the GitHub connector hands the runtime its
// scoped-token path without the runtime importing it.
func (rt *KarmaxRuntime) SetRepoTokenMinter(m RepoTokenMinter) { rt.repoTokenMinter = m }

// codingAgentToken prefers the subscription token minted by `claude
// setup-token`, and falls back to an API key for orgs billing per token.
func (rt *KarmaxRuntime) codingAgentToken() (namedSecret, error) {
	if c, err := rt.store.Credential("claude-code"); err == nil && c != nil && c.AccessToken != "" {
		return namedSecret{"CLAUDE_CODE_OAUTH_TOKEN", c.AccessToken}, nil
	}
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")); v != "" {
		return namedSecret{"CLAUDE_CODE_OAUTH_TOKEN", v}, nil
	}
	if v := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); v != "" && v != "12345" {
		return namedSecret{"ANTHROPIC_API_KEY", v}, nil
	}
	return namedSecret{}, fmt.Errorf(
		"no coding-agent credential: run `claude setup-token` and save it as the claude-code connector, or set ANTHROPIC_API_KEY")
}

// repoToken mints a git credential scoped to one repository.
//
// Narrowing is the point: the container gets write access to the repo its
// ticket names and to nothing else, and the token expires within the hour. When
// no GitHub App is configured this falls back to a configured token, which is
// broader than it should be and says so.
func (rt *KarmaxRuntime) repoToken(ctx context.Context, repo string) (string, error) {
	if mint := rt.repoTokenMinter; mint != nil {
		return mint(ctx, repo)
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); v != "" {
		rt.log.Warn("sandbox is using a broad GITHUB_TOKEN; configure a GitHub App to scope it to one repo",
			zap.String("repo", repo))
		return v, nil
	}
	return "", fmt.Errorf("no GitHub credential: connect a GitHub App, or set GITHUB_TOKEN")
}
