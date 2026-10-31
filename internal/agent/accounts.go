package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Which of the operator's accounts the agent acts through.
//
// Events now arrive from several logins per service — two GitHub users, more
// than one Google address, several AWS profiles — and acting through the wrong
// one is a reply from the wrong person. The agent is told, every turn, what is
// connected and which of its tools act as which account. All of it is derived
// (from what is connected, the tools the agent holds, and the accounts events
// have arrived from), so a new service or login appears without anyone
// editing a list.

// AccountInfo is one login KARMAX holds on a service.
type AccountInfo struct {
	Service string // the integration's id before any ":account" — "github", "gog", "slack"
	Account string // empty for the primary login
	Name    string // display name
	// Connected is whether a credential resolved at all.
	Connected bool
	// Problem is why it is not working, when it is not.
	Problem string
}

// SetAccounts installs the source of what is connected. Called each turn, so
// it should be cheap (a cached status read).
func (a *Agent) SetAccounts(fn func() []AccountInfo) {
	a.ops.mu.Lock()
	a.ops.accounts = fn
	a.ops.mu.Unlock()
}

// maxSourceAccounts bounds how many source accounts are remembered.
const maxSourceAccounts = 40

// noteSourceAccount remembers that events arrive from this account.
func (a *Agent) noteSourceAccount(e sources.Event, at time.Time) {
	a.ops.mu.Lock()
	defer a.ops.mu.Unlock()
	if a.ops.seen == nil {
		a.ops.seen = map[string]time.Time{}
	}
	a.ops.seen[e.Ref()] = at
	if len(a.ops.seen) > maxSourceAccounts {
		oldest, when := "", time.Now()
		for k, t := range a.ops.seen {
			if t.Before(when) {
				oldest, when = k, t
			}
		}
		delete(a.ops.seen, oldest)
	}
}

// maxToolsPerAccount keeps one busy integration from taking over the section.
const maxToolsPerAccount = 8

// buildAccountsContext is the accounts section of the turn context.
func (a *Agent) buildAccountsContext() string {
	a.ops.mu.Lock()
	source := a.ops.accounts
	seen := make([]string, 0, len(a.ops.seen))
	for ref := range a.ops.seen {
		seen = append(seen, ref)
	}
	a.ops.mu.Unlock()
	sort.Strings(seen)

	var accounts []AccountInfo
	if source != nil {
		for _, acc := range source() {
			if acc.Connected {
				accounts = append(accounts, acc)
			}
		}
	}
	held := a.ToolManifests()
	var byParam []string
	for _, m := range held {
		if takesAccount(m.Parameters) {
			byParam = append(byParam, m.Name+" (account)")
		}
	}
	sort.Strings(byParam)
	if len(accounts) == 0 && len(seen) == 0 && len(byParam) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Accounts you act through\n\n")
	b.WriteString("All of these are the operator's own. What arrives from one happened to the operator; what you do through one is done as the operator.\n\n")
	if len(accounts) > 0 {
		b.WriteString("Connected to KARMAX:\n")
		services := map[string][]AccountInfo{}
		var order []string
		for _, acc := range accounts {
			if _, ok := services[acc.Service]; !ok {
				order = append(order, acc.Service)
			}
			services[acc.Service] = append(services[acc.Service], acc)
		}
		sort.Strings(order)
		for _, svc := range order {
			var parts []string
			for _, acc := range services[svc] {
				label := acc.Account
				if label == "" {
					label = "primary"
				}
				if names := toolsForAccount(held, svc, acc.Account); len(names) > 0 {
					label += " — " + strings.Join(names, ", ")
				}
				if acc.Problem != "" {
					label += " [not working: " + truncateStr(acc.Problem, 80) + "]"
				}
				parts = append(parts, label)
			}
			name := svc
			if n := services[svc][0].Name; n != "" && !strings.EqualFold(n, svc) {
				name += " (" + n + ")"
			}
			fmt.Fprintf(&b, "- %s: %s\n", name, strings.Join(parts, "; "))
		}
	}
	if len(byParam) > 0 {
		b.WriteString("Tools that pick the account by parameter: " + strings.Join(byParam, ", ") + ".\n")
	}
	if len(seen) > 0 {
		b.WriteString("Events arrive from (source:account): " + strings.Join(seen, ", ") + ".\n")
	}
	b.WriteString("\nAct through the account an event came from, never a different one unless the operator asked. " +
		"When a CLI on this host is logged in to several accounts, select the account explicitly rather than trusting its default " +
		"(for example `GH_TOKEN=\"$(gh auth token --user <login>)\" gh …`, `gog --account <email> …`, `aws --profile <name> …`). " +
		"`karmax tool show <name>` shows whether a tool takes an account.\n\n")
	return b.String()
}

// toolsForAccount names the held tools that belong to one login: a connector
// suffixes an extra account's tools with "@account" and leaves the primary's
// bare.
func toolsForAccount(held []tools.ToolManifest, service, account string) []string {
	var out []string
	prefix := strings.ToLower(service) + "."
	for _, m := range held {
		name := m.Name
		if !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		at := strings.LastIndex(name, "@")
		switch {
		case account == "" && at < 0:
		case account != "" && at >= 0 && strings.EqualFold(name[at+1:], account):
		default:
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) > maxToolsPerAccount {
		out = append(out[:maxToolsPerAccount], fmt.Sprintf("+%d more", len(out)-maxToolsPerAccount))
	}
	return out
}

// takesAccount reports whether a tool's schema has an account parameter.
func takesAccount(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(params, &schema) != nil {
		return false
	}
	_, ok := schema.Properties["account"]
	return ok
}
