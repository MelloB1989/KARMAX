// Package vorg instantiates a virtual organisation from a declarative spec.
//
// An organisation here is federation, not multi-tenancy: one instance per role,
// joined by certificates. So a spec does not create tenants — it records roles
// in the org chart, issues each one a scoped certificate, and derives the memory
// namespaces they share. Everything it does is something an operator could do
// by hand with org-chart; the spec exists so the shape is reviewable and
// repeatable rather than assembled from shell history.
//
// This needs the delegation chain to be safe: a virtual org is delegation by
// construction, and without provenance a role asked to do something would pass
// it on as its own work.
package vorg

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is one virtual organisation.
type Spec struct {
	Name  string `yaml:"name"`
	Roles []Role `yaml:"roles"`
	// Wiring says which roles may talk to which. Absent means none — a role
	// that should reach another has to say so.
	Wiring []Link `yaml:"wiring"`
	// SharedMemory is a namespace every role may read. Writing to it is granted
	// per role, because a pod where everyone writes to one namespace is a pod
	// whose memory nobody can trust.
	SharedMemory string `yaml:"shared_memory"`
	// Budget is the daily spend ceiling for the whole org, divided among roles
	// that do not set their own.
	Budget int64 `yaml:"budget"`
	// CertificateTTL is how long issued certificates last. Short by default:
	// re-issuing is cheap and an expiry that never arrives is not an expiry.
	CertificateTTL time.Duration `yaml:"certificate_ttl"`
}

// Role is one seat in the organisation.
type Role struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Charter string `yaml:"charter"`
	// Instance is the mesh key of the KARMAX instance filling this seat. Empty
	// means the seat is defined but unfilled, which is a normal state — the
	// spec is written before everyone has joined.
	Instance   string   `yaml:"instance"`
	Department string   `yaml:"department"`
	Tools      []string `yaml:"tools"`
	// Memory namespaces this role may write. It always reads its own and the
	// shared one.
	Writes []string `yaml:"writes"`
	Budget int64    `yaml:"budget"`
}

// Link is one direction of allowed contact.
type Link struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	// Verbs default to ask, which is the useful one: a role that can only
	// broadcast cannot get an answer.
	Verbs []string `yaml:"verbs"`
}

// Error is a problem with a spec, located.
type Error struct {
	Line    int
	Message string
	Fix     string
}

func (e *Error) Error() string {
	s := fmt.Sprintf("line %d: %s", e.Line, e.Message)
	if e.Fix != "" {
		s += "\n  try: " + e.Fix
	}
	return s
}

// Parse reads and validates a spec.
func Parse(data []byte) (*Spec, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &Error{Line: 1, Message: err.Error()}
	}
	if len(doc.Content) == 0 {
		return nil, &Error{Line: 1, Message: "the spec is empty"}
	}

	var s Spec
	if err := doc.Content[0].Decode(&s); err != nil {
		return nil, &Error{Line: doc.Content[0].Line, Message: err.Error()}
	}
	if s.CertificateTTL == 0 {
		s.CertificateTTL = 7 * 24 * time.Hour
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Spec) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return &Error{Line: 1, Message: "the organisation has no name",
			Fix: "add 'name: research-pod' at the top"}
	}
	if len(s.Roles) == 0 {
		return &Error{Line: 1, Message: "no roles",
			Fix: "an organisation with no roles has nothing to instantiate"}
	}

	seen := map[string]bool{}
	for _, r := range s.Roles {
		if strings.TrimSpace(r.ID) == "" {
			return &Error{Line: 1, Message: "a role has no id",
				Fix: "every role needs an 'id:' — it is what wiring refers to"}
		}
		if seen[r.ID] {
			return &Error{Line: 1, Message: fmt.Sprintf("two roles share the id %q", r.ID),
				Fix: "ids must be unique, since wiring resolves by id"}
		}
		seen[r.ID] = true
	}

	for _, l := range s.Wiring {
		if !seen[l.From] {
			return &Error{Line: 1, Message: fmt.Sprintf("wiring names an unknown role %q as 'from'", l.From),
				Fix: "the roles are " + strings.Join(roleIDs(s.Roles), ", ")}
		}
		if !seen[l.To] {
			return &Error{Line: 1, Message: fmt.Sprintf("wiring names an unknown role %q as 'to'", l.To),
				Fix: "the roles are " + strings.Join(roleIDs(s.Roles), ", ")}
		}
		if l.From == l.To {
			return &Error{Line: 1, Message: fmt.Sprintf("%q is wired to itself", l.From),
				Fix: "a role reaches its own instance directly; remove the link"}
		}
	}
	return nil
}

func roleIDs(roles []Role) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}

// Filled returns the roles that have an instance behind them.
func (s *Spec) Filled() []Role {
	var out []Role
	for _, r := range s.Roles {
		if strings.TrimSpace(r.Instance) != "" {
			out = append(out, r)
		}
	}
	return out
}

// Vacant returns roles defined but not yet filled.
func (s *Spec) Vacant() []Role {
	var out []Role
	for _, r := range s.Roles {
		if strings.TrimSpace(r.Instance) == "" {
			out = append(out, r)
		}
	}
	return out
}

// budgetFor divides the org budget among roles that do not set their own.
func (s *Spec) budgetFor(r Role) int64 {
	if r.Budget > 0 {
		return r.Budget
	}
	if s.Budget <= 0 {
		return 0
	}
	unset := 0
	claimed := int64(0)
	for _, other := range s.Roles {
		if other.Budget > 0 {
			claimed += other.Budget
			continue
		}
		unset++
	}
	if unset == 0 {
		return 0
	}
	remaining := s.Budget - claimed
	if remaining <= 0 {
		return 0
	}
	return remaining / int64(unset)
}
