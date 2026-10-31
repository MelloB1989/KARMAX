package policy

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The operator's decisions are the default: act, then report; WhatsApp for
// hard-floor approvals, urgent items and what was sent as them — nothing else.
func TestTheDefaultIsActThenReport(t *testing.T) {
	p := New(Config{})
	if !p.ActFirst() {
		t.Error("the default must be to act, then report")
	}
	for c, want := range map[Class]bool{Approval: true, Urgent: true, Sent: true, Notification: false} {
		if got := p.ToWhatsApp(c); got != want {
			t.Errorf("ToWhatsApp(%s) = %v, want %v", c, got, want)
		}
	}
	if len(p.HardFloor()) != 3 {
		t.Errorf("hard floor = %v, want deletion, payments and secrets", p.HardFloor())
	}
}

// Every knob is the operator's to turn in karmax.yaml.
func TestThePolicyIsConfigurable(t *testing.T) {
	var cfg struct {
		Policy Config `yaml:"policy"`
	}
	raw := `
policy:
  autonomy: ask
  hard_floor: ["merging to main"]
  whatsapp: [approvals]
  urgency_threshold: 0.7
`
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	p := New(cfg.Policy)
	if p.ActFirst() {
		t.Error("autonomy: ask was ignored")
	}
	if p.ToWhatsApp(Urgent) || p.ToWhatsApp(Sent) || !p.ToWhatsApp(Approval) {
		t.Errorf("whatsapp classes not honoured: %+v", p)
	}
	if got := p.HardFloor(); len(got) != 1 || got[0] != "merging to main" {
		t.Errorf("hard floor = %v", got)
	}
	if !p.IsUrgent(false, 0.7) || p.IsUrgent(false, 0.69) {
		t.Error("urgency threshold not honoured")
	}
}

// An explicitly empty list means nothing reaches WhatsApp, not "use the default".
func TestAnEmptyWhatsAppListSilencesWhatsApp(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("whatsapp: []"), &cfg); err != nil {
		t.Fatal(err)
	}
	p := New(cfg)
	for _, c := range []Class{Approval, Urgent, Sent, Notification} {
		if p.ToWhatsApp(c) {
			t.Errorf("%s still reaches WhatsApp with an empty list", c)
		}
	}
}

// Urgent is the source's flag, or the screener's urgency at the threshold.
func TestUrgentIsTheFlagOrTheThreshold(t *testing.T) {
	p := New(Config{})
	if !p.IsUrgent(true, 0) {
		t.Error("a flagged event is urgent whatever its score")
	}
	if !p.IsUrgent(false, 0.9) || p.IsUrgent(false, 0.89) {
		t.Error("the default threshold is 0.9")
	}
}

// Something that happened hours before it reached KARMAX is not "now", however
// urgent it was then: a source's catch-up after a restart or at first sight
// must not page the operator.
func TestOnlyFreshItemsCanPage(t *testing.T) {
	now := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	p := New(Config{})
	if !p.Fresh(now.Add(-90*time.Minute), now) || p.Fresh(now.Add(-3*time.Hour), now) {
		t.Error("the default freshness window is two hours")
	}
	if !p.Fresh(time.Time{}, now) || !p.Fresh(now.Add(time.Hour), now) {
		t.Error("an undated or future item (a meeting starting) is fresh")
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("urgent_max_age: 30m"), &cfg); err != nil {
		t.Fatal(err)
	}
	if New(cfg).Fresh(now.Add(-45*time.Minute), now) {
		t.Error("urgent_max_age not honoured")
	}
}

// The model is told the policy in words it can act on, generated from the
// config so it never disagrees with what the code enforces.
func TestTheBriefSaysWhatTheCodeEnforces(t *testing.T) {
	brief := New(Config{HardFloor: []string{"paying invoices"}}).Brief()
	for _, want := range []string{"Act, then report", "paying invoices", "propose", "floor", "SENT:", "urgent"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief omits %q:\n%s", want, brief)
		}
	}
	ask := New(Config{Autonomy: "ask"}).Brief()
	if strings.Contains(ask, "Act, then report") {
		t.Errorf("an ask-first policy was briefed as act-first:\n%s", ask)
	}
	none := New(Config{WhatsApp: []string{}}).Brief()
	if strings.Contains(none, "SENT:") {
		t.Errorf("with no sent reports the model was still asked to declare sends:\n%s", none)
	}
}
