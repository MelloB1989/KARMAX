package memory

import (
	"context"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// Two tiers of memory: what the organisation knows, and what one person's
// dealings with it produced.
//
// The GLOBAL namespace is shared by every agent, recipe and workflow — the
// company's own knowledge, and the only thing available when no particular
// person is being helped.
//
// A MEMBER namespace exists per person and is in play only while acting for
// them. It is not a privacy nicety: an agent that answers a scheduled loop
// using what somebody told it in a private conversation has published that
// conversation, and nobody asked it to.

// MemberNamespaceInfix separates the global namespace from a member id.
const MemberNamespaceInfix = "-member-"

// Scopes resolves which memory a piece of work should read and write.
type Scopes struct {
	factory *ManagerFactory
	store   *store.Store
	agentID string
	global  string
	log     *zap.Logger
}

// NewScopes builds the resolver. global is the local namespace name that every
// agent, recipe and workflow shares.
func NewScopes(f *ManagerFactory, db *store.Store, agentID, global string, log *zap.Logger) *Scopes {
	if global == "" {
		global = agentID
	}
	return &Scopes{factory: f, store: db, agentID: agentID, global: global, log: log}
}

// GlobalNamespace is the shared namespace's local name.
func (s *Scopes) GlobalNamespace() string { return s.global }

// Global is the memory every agent, recipe and workflow shares.
func (s *Scopes) Global() *Manager {
	return s.factory.For(s.agentID, s.global)
}

// MemberNamespace is where one person's memory lives.
//
// An admin's explicit choice in org_members wins, so a team already keeping
// somebody's memory somewhere keeps it there. Otherwise it is derived, which
// means the two tiers work with no setup at all — and a namespace nobody
// configured is still predictable enough to find.
func (s *Scopes) MemberNamespace(member string) string {
	member = strings.TrimSpace(member)
	if member == "" {
		return ""
	}
	if s.store != nil {
		if m, err := s.store.OrgMemberByKey(member); err == nil && m != nil {
			if ns := strings.TrimSpace(m.Namespace); ns != "" {
				return ns
			}
		}
	}
	return s.global + MemberNamespaceInfix + sanitiseMember(member)
}

// ForMember is one person's memory.
func (s *Scopes) ForMember(member string) *Manager {
	ns := s.MemberNamespace(member)
	if ns == "" {
		return s.Global()
	}
	return s.factory.For(s.agentID, ns)
}

// Write returns the memory a new fact belongs in.
//
// The acting member's, when there is one. What the agent learns while helping
// Priya is Priya's until somebody says otherwise: a company fact filed under
// one person is untidy and recoverable, while a private remark filed in the
// shared namespace is readable by every agent, recipe and colleague, and
// cannot be taken back.
//
// Callers that mean "this belongs to the organisation" say so by using Global
// explicitly. There is no way to reach it by accident.
func (s *Scopes) Write(ctx context.Context) *Manager {
	if member := connectorkit.ActorFrom(ctx); member != "" {
		return s.ForMember(member)
	}
	return s.Global()
}

// Read returns every memory this work may consult, global first.
//
// Global always; the acting member's as well, when there is one. Work that is
// nobody's request — a cron tick, a webhook — sees only the shared namespace,
// which is the whole point of separating them.
func (s *Scopes) Read(ctx context.Context) []*Manager {
	out := []*Manager{s.Global()}
	if member := connectorkit.ActorFrom(ctx); member != "" {
		if ns := s.MemberNamespace(member); ns != "" && ns != s.global {
			out = append(out, s.factory.For(s.agentID, ns))
		}
	}
	return out
}

// ActingFor names the member this work is on behalf of, or "".
func (s *Scopes) ActingFor(ctx context.Context) string {
	return connectorkit.ActorFrom(ctx)
}

// sanitiseMember makes a member id safe to put in a namespace.
//
// Namespaces become GitLoom repositories and local directory names, so a
// member id with a slash in it would otherwise write outside where it belongs.
func sanitiseMember(member string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(member) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == '@' || r == '+' || r == '/' || r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		return "unknown"
	}
	return out
}
