package runtime

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/MelloB1989/karmax/internal/bus"
	ghconn "github.com/MelloB1989/karmax/internal/connectors/github"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/integrations"
	"github.com/MelloB1989/karmax/internal/sources"
	sourcegh "github.com/MelloB1989/karmax/internal/sources/github"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// The GitHub source, wired: every login the gh CLI holds is polled for
// notifications, each new thread reaches the agent as a webhook.fired event on
// route github:<login>, and the github.* tools act as whichever login the work
// belongs to. See internal/sources/github and internal/connectors/github/act.go.
//
//	KARMAX_SOURCE_GITHUB=off  stops the polling; the acting tools stay

// githubSourceOn reports whether the GitHub source polls.
func githubSourceOn(getenv func(string) string) bool {
	return !strings.EqualFold(strings.TrimSpace(getenv("KARMAX_SOURCE_GITHUB")), "off")
}

// registerGitHubSource registers the acting tools and, unless switched off,
// the polling loop. Called before agents bind their tools.
func registerGitHubSource(getenv func(string) string, s *store.Store, b *bus.Log, toolReg *tools.Registry,
	integrationReg *integration.Registry, agentID string, log *zap.Logger) {
	logins := integrations.GhLogins()
	registerGitHubTools(toolReg, &ghconn.Actor{Logins: logins, OnAct: func(act ghconn.Act) {
		// "Act freely, tell after": whatever was said as the operator is
		// told to them; the mirror decides whether WhatsApp hears it now.
		title, body := githubActNotice(act)
		builtin.PushAppNotification(s, agentID, "info", title, body)
	}})

	if !githubSourceOn(getenv) {
		log.Info("github source is off (KARMAX_SOURCE_GITHUB=off)")
		return
	}
	poller := &sourcegh.Poller{
		Logins:  logins,
		KV:      sourcegh.KV{Get: s.KVGet, Set: s.KVSet},
		Publish: func(e sources.Event) error { return b.Publish(e.BusEvent(agentID)) },
	}
	loopkit.Register(loopkit.Loop{
		Name: "github-source",
		Description: "Every few minutes reads the notifications of every GitHub account gh is logged in to and hands each " +
			"new or updated thread to the agent as an event on that account: review requests, mentions, assignments, " +
			"security alerts and failing runs marked urgent. Never marks anything read.",
		Schedule: loopkit.Every("3m"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			return githubTick(ctx, k.Logf, poller, logins, integrationReg, nil)
		},
	})
}

// githubTick polls once. A login seen for the first time gets a health row,
// and a failing one has its row re-checked now, so health shows it before the
// next half-hourly sweep does.
func githubTick(ctx context.Context, logf func(string, ...any), p *sourcegh.Poller, logins ghconn.Logins,
	reg *integration.Registry, hc *http.Client) error {
	res, err := p.Tick(ctx)
	var failing []string
	for _, a := range res.Accounts {
		id := "gh:" + a.Account
		if reg != nil {
			if _, ok := reg.Get(id); !ok {
				integrations.RegisterGitHubLogin(reg, logins, a.Login, hc)
			}
		}
		if a.Err != nil {
			failing = append(failing, a.Account+" ("+a.Err.Error()+")")
			if reg != nil {
				reg.Check(ctx, id)
			}
		}
	}
	if n := res.Published(); n > 0 || len(failing) > 0 {
		line := fmt.Sprintf("github-source: %d event(s) from %d account(s)", n, len(res.Accounts))
		if len(failing) > 0 {
			line += "; not readable: " + strings.Join(failing, "; ")
		}
		logf("%s", line)
	}
	return err
}

// githubActNotice is what the operator is told after KARMAX acted as them.
func githubActNotice(a ghconn.Act) (title, body string) {
	ref := a.Repo
	if a.Number > 0 {
		ref += "#" + strconv.Itoa(a.Number)
	}
	title = fmt.Sprintf("On GitHub as %s: %s %s", a.Account, a.Action, ref)
	body = a.URL
	if said := strings.Join(strings.Fields(a.Body), " "); said != "" {
		if r := []rune(said); len(r) > 280 {
			said = string(r[:280]) + "…"
		}
		body = "“" + said + "”\n" + a.URL
	}
	return title, body
}

// registerGitHubTools adds the acting tools to the registry; an agent is
// granted them with `github.*` in its tools.
func registerGitHubTools(reg *tools.Registry, actor *ghconn.Actor) {
	for _, t := range actor.Tools() {
		reg.Register(selfCredentialedTool{t})
	}
}

// selfCredentialedTool runs a connectorkit tool that finds its own
// credentials, as the gh-backed GitHub tools do.
type selfCredentialedTool struct{ t connectorkit.Tool }

func (s selfCredentialedTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: s.t.Name, Description: s.t.Description, Parameters: s.t.Parameters}
}

func (s selfCredentialedTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	out, err := s.t.Call(ctx, connectorkit.Credentials{}, input)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	return tools.SuccessResult(out), nil
}
