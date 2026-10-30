package identity

import (
	"strings"
	"testing"
)

// The operator's accounts as an operator would configure them: a phone number
// and a LID, with none of the device suffixes WhatsApp later adds.
func priya() *Directory {
	d := New("Priya", []string{"15550001111", "123456789@lid"})
	d.SetLinked("15550002222:63@s.whatsapp.net", "987654321:63@lid")
	return d
}

// Who a sender is comes from the account, in every form WhatsApp writes it.
//
// A group message arrives under the sender's LID with a device suffix
// ("123456789:4@lid"), never in the bare form the operator configured. Missing
// that is how the operator, speaking from their own account, read as a stranger.
func TestTheAccountDecidesWhoIsSpeaking(t *testing.T) {
	d := priya()
	for _, c := range []struct {
		id   string
		want Role
	}{
		{"15550001111", RoleOperator},
		{"15550001111@s.whatsapp.net", RoleOperator},
		{"123456789@lid", RoleOperator},
		{"123456789:4@lid", RoleOperator},
		{"15550002222@s.whatsapp.net", RoleLinked},
		{"987654321:63@lid", RoleLinked},
		{"15550003333@s.whatsapp.net", RoleContact},
		{"555000444@lid", RoleContact},
		{"", RoleUnknown},
		{"   ", RoleUnknown},
	} {
		if got := d.RoleOf(c.id); got != c.want {
			t.Errorf("RoleOf(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

// A label never names the operator: the configured name does.
//
// The operator's own account can carry any label — a contact saved as a legacy
// account name, a business name, a push name. Whatever it says, the operator
// is called by the one name configured for them.
func TestTheOperatorIsNamedByConfigNotByLabel(t *testing.T) {
	d := priya()
	if got := d.NameOf("123456789:4@lid", "Omar"); got != "Priya" {
		t.Errorf("operator named %q from a label; want the configured name", got)
	}
	if got := d.NameOf("15550003333@s.whatsapp.net", "Omar"); got != "Omar" {
		t.Errorf("contact named %q; want their own label", got)
	}
	if got := d.NameOf("15550003333@s.whatsapp.net", ""); got != "+15550003333" {
		t.Errorf("unlabelled contact named %q; want their number", got)
	}
	if got := d.NameOf("555000444@lid", " "); got != "an unnamed contact (555000444)" {
		t.Errorf("unlabelled LID contact named %q", got)
	}
}

// With no name configured the operator is "the operator", never a guess.
func TestAnUnnamedOperatorIsNotGivenAName(t *testing.T) {
	d := New("", []string{"15550001111"})
	if got := d.NameOf("15550001111", "Omar"); got != "the operator" {
		t.Errorf("unnamed operator called %q", got)
	}
	brief := d.Brief()
	if !strings.Contains(brief, "not configured") {
		t.Errorf("the brief should say no name is configured:\n%s", brief)
	}
}

// Describe is the one line a prompt states about a sender.
func TestDescribeStatesTheRolePlainly(t *testing.T) {
	d := priya()
	op := d.Describe("123456789:4@lid", "Omar")
	if !strings.Contains(op, "your operator, Priya") || strings.Contains(op, "Omar") {
		t.Errorf("operator described as %q", op)
	}
	linked := d.Describe("15550002222@s.whatsapp.net", "")
	if !strings.Contains(linked, "linked account") || !strings.Contains(linked, "Priya") {
		t.Errorf("linked account described as %q", linked)
	}
	other := d.Describe("15550003333@s.whatsapp.net", "Omar")
	if !strings.Contains(other, "Omar") || !strings.Contains(other, "NOT your operator") {
		t.Errorf("contact described as %q", other)
	}
	if got := d.Describe("", ""); !strings.Contains(got, "unknown") {
		t.Errorf("no sender described as %q", got)
	}
}

// The brief is what every turn is told: who the operator is, which accounts are
// theirs, which one KARMAX sends from, and that nothing overrides it.
func TestBriefNamesEveryAccountAndOutranksEverythingElse(t *testing.T) {
	brief := priya().Brief()
	for _, want := range []string{
		"Priya",
		"15550001111", "123456789@lid", // the operator's accounts
		"15550002222", "987654321@lid", // the linked account, without device suffixes
		"contact label", "remembered", // what does not override it
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, ":63") {
		t.Errorf("brief kept a device suffix:\n%s", brief)
	}
}

// The operator's LID is learned from their own direct chat.
//
// An operator configured by phone number speaks in groups under a LID the
// configuration never mentions. Their own direct chat is the one place where
// only they can be the sender, so whatever id a message there carries is them.
func TestTheOperatorsOwnChatTeachesTheirOtherIDs(t *testing.T) {
	store := &memAliases{}
	d := New("Priya", []string{"15550001111"})
	d.SetAliasStore(store)

	if !d.LearnAlias("15550001111@s.whatsapp.net", "123456789:4@lid") {
		t.Fatal("a message in the operator's own chat did not teach their LID")
	}
	if d.RoleOf("123456789@lid") != RoleOperator {
		t.Error("the learned LID is not the operator")
	}
	if store.saved["123456789@lid"] == "" {
		t.Error("the learned LID was not saved")
	}

	// Remembered across a restart.
	again := New("Priya", []string{"15550001111"})
	again.SetAliasStore(store)
	if again.RoleOf("123456789:7@lid") != RoleOperator {
		t.Error("a saved alias was forgotten on restart")
	}
}

// Nothing but the operator's own chat teaches an alias.
func TestOtherChatsTeachNothing(t *testing.T) {
	d := priya()
	if d.LearnAlias("15550003333@s.whatsapp.net", "555000444@lid") {
		t.Error("a stranger's chat taught an operator alias")
	}
	if d.RoleOf("555000444@lid") == RoleOperator {
		t.Error("a stranger became the operator")
	}
	// The linked account is the linked account, whatever chat it shows up in.
	if d.LearnAlias("15550001111", "987654321@lid") {
		t.Error("the linked account was learned as an operator alias")
	}
	if d.LearnAlias("15550001111", "") || d.LearnAlias("", "555000444@lid") {
		t.Error("an empty id taught an alias")
	}
}

func TestNameFromEnvPrefersTheGeneralSetting(t *testing.T) {
	env := map[string]string{
		"KARMAX_LOOP_WA_MONITOR_OPERATOR_NAME": "Omar",
		"KARMAX_OPERATOR_NAME":                 " Priya ",
	}
	if got := NameFromEnv(func(k string) string { return env[k] }); got != "Priya" {
		t.Errorf("NameFromEnv = %q", got)
	}
	delete(env, "KARMAX_OPERATOR_NAME")
	if got := NameFromEnv(func(k string) string { return env[k] }); got != "Omar" {
		t.Errorf("fallback NameFromEnv = %q", got)
	}
}

// A nil directory knows nothing and says nothing, rather than panicking in a
// path that runs before configuration.
func TestNilDirectoryIsSafe(t *testing.T) {
	var d *Directory
	if d.RoleOf("15550001111") != RoleUnknown || d.Brief() != "" || d.Name() != "" {
		t.Error("nil directory claimed knowledge")
	}
	if d.LearnAlias("a", "b") {
		t.Error("nil directory learned")
	}
	_ = d.Describe("15550001111", "Omar")
	_ = d.NameOf("15550001111", "Omar")
}

type memAliases struct{ saved map[string]string }

func (m *memAliases) LoadAliases() (map[string]string, error) {
	out := map[string]string{}
	for k, v := range m.saved {
		out[k] = v
	}
	return out, nil
}

func (m *memAliases) SaveAlias(alias, account string) error {
	if m.saved == nil {
		m.saved = map[string]string{}
	}
	m.saved[alias] = account
	return nil
}
