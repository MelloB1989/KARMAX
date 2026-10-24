package identity

import "testing"

func TestAWorkerIsItsContainersAgent(t *testing.T) {
	for name, want := range map[string]string{
		"agent-03": "agent-03", "agent-03:2": "agent-03", "agent-03-s2": "agent-03", "agent-12-s10": "agent-12",
		"agent-03-sx": "agent-03-sx", "agent-s2": "agent-s2", "": "",
	} {
		if got := Base(name); got != want {
			t.Errorf("Base(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAKeyIsTheAgentsAndOnlyTheFullTokenMakesIt(t *testing.T) {
	k := Key("full", "agent-03")
	if len(k) != 64 || k == Key("full", "agent-04") || k == Key("other", "agent-03") || k != Key("full", "agent-03") {
		t.Fatalf("keys do not tell agents and tokens apart: %s", k)
	}
	if Key("", "agent-03") != "" {
		t.Fatal("no full token makes a key anyway")
	}
}

func TestVerifyTakesAWorkerOnlyWithItsBaseAgentsKey(t *testing.T) {
	k3 := Key("full", "agent-03")
	cases := []struct {
		header, name string
		ok           bool
	}{
		{Value("agent-03", k3), "agent-03", true},
		{Value("agent-03:2", k3), "agent-03:2", true},
		{Value("agent-03-s2", k3), "agent-03-s2", true},
		{Value("agent-04", k3), "", false},
		{Value("agent-04:2", k3), "", false},
		{Value("agent-03", Key("relay", "agent-03")), "", false},
		{"agent-03", "", false},
		{"agent-03:", "", false},
		{":" + k3, "", false},
		{"", "", false},
	}
	for _, c := range cases {
		name, ok := Verify("full", c.header)
		if name != c.name || ok != c.ok {
			t.Errorf("Verify(%q) = %q, %v; want %q, %v", c.header, name, ok, c.name, c.ok)
		}
	}
	if _, ok := Verify("", Value("agent-03", Key("", "agent-03"))); ok {
		t.Fatal("verified with no full token")
	}
}
