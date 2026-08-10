package vorg

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/mesh"
	"github.com/MelloB1989/karmax/internal/store"
)

// Planning before applying.
//
// Instantiating an organisation issues certificates and grants capabilities
// across several machines. That is not something to do and then inspect, so a
// spec is turned into a list of intended changes first, in the operator's
// words, and applied only on a separate decision.

// Action is one thing applying the spec would do.
type Action struct {
	Kind string // "hire", "certificate", "grant", "namespace"
	Role string
	// Describe is the operator-facing sentence.
	Describe string

	member store.OrgMember
	cert   certRequest
	grant  store.Grant
}

type certRequest struct {
	subject string
	scopes  []string
	ttl     time.Duration
}

// Plan is everything applying a spec would change.
type Plan struct {
	Org     string
	OrgName string
	Spec    *Spec
	Actions []Action
	// Vacant names roles with nobody in them, which is a warning rather than an
	// error — a spec is normally written before everyone has joined.
	Vacant []string
}

// Build works out what a spec implies, without changing anything.
func Build(s *Spec, orgKey, orgName string) *Plan {
	p := &Plan{Org: orgKey, OrgName: orgName, Spec: s}

	for _, r := range s.Vacant() {
		p.Vacant = append(p.Vacant, r.ID)
	}

	shared := s.SharedMemory
	if shared == "" {
		shared = store.OrgNamespace(s.Name, "")
	}

	for _, r := range s.Filled() {
		dept := r.Department
		if dept == "" {
			dept = r.ID
		}
		ns := store.OrgNamespace(s.Name, dept)

		p.Actions = append(p.Actions, Action{
			Kind: "hire", Role: r.ID,
			Describe: fmt.Sprintf("record %s as %s in %s, with memory namespace %s",
				short(r.Instance), displayName(r), dept, ns),
			member: store.OrgMember{
				Org: orgKey, Member: r.Instance, Name: displayName(r),
				Department: dept, Role: r.Charter, Namespace: ns,
			},
		})

		// The certificate carries what this role may do here, and the scopes
		// are what the Broker will enforce on the member's side.
		scopes := []string{mesh.ScopeMessage, mesh.ScopeAsk}
		scopes = append(scopes, "memory:"+ns+":write")
		scopes = append(scopes, "memory:"+shared)
		for _, w := range r.Writes {
			scopes = append(scopes, "memory:"+w+":write")
		}
		for _, t := range r.Tools {
			scopes = append(scopes, "tool:"+t)
		}
		if b := s.budgetFor(r); b > 0 {
			scopes = append(scopes, fmt.Sprintf("spend:%d", b))
		}
		sort.Strings(scopes)

		p.Actions = append(p.Actions, Action{
			Kind: "certificate", Role: r.ID,
			Describe: fmt.Sprintf("issue %s a certificate valid %s, asking for: %s",
				displayName(r), s.CertificateTTL, strings.Join(humanScopes(scopes), "; ")),
			cert: certRequest{subject: r.Instance, scopes: scopes, ttl: s.CertificateTTL},
		})
	}

	// Wiring is recorded on this instance as grants, so the org can see who was
	// meant to reach whom. The pairwise connection itself is still each
	// operator's decision — an org cannot connect two people's instances by
	// declaring it, and should not be able to.
	for _, l := range s.Wiring {
		from, to := roleByID(s, l.From), roleByID(s, l.To)
		if from.Instance == "" || to.Instance == "" {
			continue
		}
		verbs := l.Verbs
		if len(verbs) == 0 {
			verbs = []string{mesh.ScopeAsk}
		}
		p.Actions = append(p.Actions, Action{
			Kind: "grant", Role: l.From,
			Describe: fmt.Sprintf("record that %s may %s %s (each operator still has to accept the connection)",
				displayName(from), strings.Join(verbs, "/"), displayName(to)),
			grant: store.Grant{
				Subject: "peer:" + from.Instance, Capability: store.CapChannel,
				Value: "vorg:" + s.Name + ":" + l.To, GrantedBy: "vorg:" + s.Name,
			},
		})
	}
	return p
}

// Report renders a plan for a terminal.
func (p *Plan) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Instantiating %q as the org %s would:\n\n", p.Spec.Name, orDash(p.OrgName, short(p.Org)))
	if len(p.Actions) == 0 {
		b.WriteString("  do nothing — no role has an instance behind it yet.\n")
	}
	for i, a := range p.Actions {
		fmt.Fprintf(&b, "%2d. [%s] %s\n", i+1, a.Kind, a.Describe)
	}
	if len(p.Vacant) > 0 {
		fmt.Fprintf(&b, "\nUnfilled roles: %s\n", strings.Join(p.Vacant, ", "))
		b.WriteString("Add each one's instance key to the spec and apply again; nothing is issued for them now.\n")
	}
	return b.String()
}

// Applier is what a plan needs to change the world. An interface so a plan can
// be tested without a mesh identity or a database.
type Applier interface {
	Hire(store.OrgMember) error
	Issue(subject string, scopes []string, ttl time.Duration) error
	Grant(store.Grant) error
}

// Apply carries out a plan, stopping at the first failure.
//
// Not transactional, and it says so: certificates go to other machines and
// cannot be recalled. Re-applying is safe — every action is an upsert — so the
// recovery from a partial apply is to fix the cause and run it again.
func (p *Plan) Apply(a Applier) error {
	for i, act := range p.Actions {
		var err error
		switch act.Kind {
		case "hire":
			err = a.Hire(act.member)
		case "certificate":
			err = a.Issue(act.cert.subject, act.cert.scopes, act.cert.ttl)
		case "grant":
			err = a.Grant(act.grant)
		}
		if err != nil {
			return fmt.Errorf("step %d (%s for %s): %w — the %d before it were applied, and re-running is safe",
				i+1, act.Kind, act.Role, err, i)
		}
	}
	return nil
}

// humanScopes renders scopes the way an operator reads them.
func humanScopes(scopes []string) []string {
	var out []string
	for _, s := range scopes {
		class, value, ok := strings.Cut(s, ":")
		if !ok {
			out = append(out, "may "+s)
			continue
		}
		switch class {
		case "memory":
			if ns, write := strings.CutSuffix(value, ":write"); write {
				out = append(out, "write memory in "+ns)
			} else {
				out = append(out, "read memory in "+value)
			}
		case "tool":
			out = append(out, "call "+value)
		case "spend":
			out = append(out, "spend up to "+value+" a day")
		default:
			out = append(out, s)
		}
	}
	return out
}

func roleByID(s *Spec, id string) Role {
	for _, r := range s.Roles {
		if r.ID == id {
			return r
		}
	}
	return Role{}
}

func displayName(r Role) string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return r.ID
}

func short(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}

func orDash(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
