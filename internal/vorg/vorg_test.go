package vorg

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
)

const podSpec = `
name: research-pod
shared_memory: pod-shared
budget: 300000
certificate_ttl: 168h
roles:
  - id: researcher
    name: Ada
    instance: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
    charter: finds and reads sources
    department: research
    tools: [github.issues]
  - id: writer
    name: Bea
    instance: BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
    charter: turns findings into a brief
    budget: 100000
  - id: reviewer
    charter: checks the brief before it goes out
wiring:
  - from: researcher
    to: writer
    verbs: [ask]
`

func TestASpecParsesAndSeparatesFilledFromVacant(t *testing.T) {
	s, err := Parse([]byte(podSpec))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "research-pod" || len(s.Roles) != 3 {
		t.Fatalf("parsed %+v", s)
	}
	if len(s.Filled()) != 2 || len(s.Vacant()) != 1 {
		t.Errorf("filled %d, vacant %d", len(s.Filled()), len(s.Vacant()))
	}
	if s.Vacant()[0].ID != "reviewer" {
		t.Errorf("vacant = %+v", s.Vacant())
	}
	if s.CertificateTTL != 168*time.Hour {
		t.Errorf("ttl = %s", s.CertificateTTL)
	}
}

func TestSpecErrorsSaySomethingUseful(t *testing.T) {
	for _, tc := range []struct{ name, yaml, wants string }{
		{"no name", "roles:\n  - id: a\n", "no name"},
		{"no roles", "name: x\n", "no roles"},
		{"a role with no id", "name: x\nroles:\n  - name: nobody\n", "no id"},
		{"duplicate ids", "name: x\nroles:\n  - id: a\n  - id: a\n", "share the id"},
		{"wiring to nowhere", "name: x\nroles:\n  - id: a\nwiring:\n  - from: a\n    to: ghost\n", "unknown role"},
		{"a role wired to itself", "name: x\nroles:\n  - id: a\nwiring:\n  - from: a\n    to: a\n", "wired to itself"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatal("accepted an invalid spec")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not mention %q", err, tc.wants)
			}
		})
	}
}

func TestABudgetIsDividedAmongRolesThatDoNotSetOne(t *testing.T) {
	s, err := Parse([]byte(podSpec))
	if err != nil {
		t.Fatal(err)
	}
	// 300000 total, the writer claims 100000, leaving 200000 for the two that
	// did not ask — 100000 each.
	for _, r := range s.Roles {
		got := s.budgetFor(r)
		want := int64(100000)
		if got != want {
			t.Errorf("%s got %d, want %d", r.ID, got, want)
		}
	}
}

func TestAPlanChangesNothingUntilApplied(t *testing.T) {
	s, _ := Parse([]byte(podSpec))
	p := Build(s, "ORGKEY", "Vector")

	// Two filled roles: hire + certificate each, plus one wiring grant.
	if len(p.Actions) != 5 {
		t.Fatalf("planned %d actions:\n%s", len(p.Actions), p.Report())
	}
	if len(p.Vacant) != 1 || p.Vacant[0] != "reviewer" {
		t.Errorf("vacant = %v", p.Vacant)
	}

	report := p.Report()
	for _, want := range []string{
		"record", "issue", "Ada", "Bea",
		// The report has to be readable, not a scope dump.
		"write memory in", "call github.issues", "spend up to",
		// And it must not imply the org can connect two people by declaring it.
		"each operator still has to accept",
		"Unfilled roles: reviewer",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// recorder is an Applier that remembers rather than acts.
type recorder struct {
	hired  []store.OrgMember
	issued []string
	grants []store.Grant
	failAt int
	calls  int
}

func (r *recorder) next() error {
	r.calls++
	if r.failAt > 0 && r.calls == r.failAt {
		return fmt.Errorf("simulated failure")
	}
	return nil
}

func (r *recorder) Hire(m store.OrgMember) error {
	if err := r.next(); err != nil {
		return err
	}
	r.hired = append(r.hired, m)
	return nil
}

func (r *recorder) Issue(subject string, scopes []string, ttl time.Duration) error {
	if err := r.next(); err != nil {
		return err
	}
	r.issued = append(r.issued, subject+" "+strings.Join(scopes, ","))
	return nil
}

func (r *recorder) Grant(g store.Grant) error {
	if err := r.next(); err != nil {
		return err
	}
	r.grants = append(r.grants, g)
	return nil
}

func TestApplyingCarriesOutThePlan(t *testing.T) {
	s, _ := Parse([]byte(podSpec))
	p := Build(s, "ORGKEY", "Vector")

	var rec recorder
	if err := p.Apply(&rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.hired) != 2 || len(rec.issued) != 2 || len(rec.grants) != 1 {
		t.Fatalf("hired %d, issued %d, granted %d", len(rec.hired), len(rec.issued), len(rec.grants))
	}

	// Each role reads the shared namespace and writes its own — the split the
	// spec exists to enforce.
	joined := strings.Join(rec.issued, "\n")
	if !strings.Contains(joined, "memory:pod-shared") {
		t.Error("nobody was granted the shared namespace")
	}
	if !strings.Contains(joined, "memory:org-research-pod-research:write") {
		t.Error("the researcher cannot write its own namespace")
	}
	if strings.Contains(joined, "memory:pod-shared:write") {
		t.Error("a role was granted write on the shared namespace by default")
	}
	// The unfilled role gets nothing at all.
	if strings.Contains(joined, "reviewer") {
		t.Error("an unfilled role was issued a certificate")
	}
}

func TestAPartialApplyReportsWhatGotThrough(t *testing.T) {
	s, _ := Parse([]byte(podSpec))
	p := Build(s, "ORGKEY", "Vector")

	rec := recorder{failAt: 3}
	err := p.Apply(&rec)
	if err == nil {
		t.Fatal("a failing apply reported success")
	}
	// Certificates go to other machines and cannot be recalled, so the message
	// has to say where it stopped and that re-running is safe.
	for _, want := range []string{"step 3", "2 before it were applied", "re-running is safe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}
