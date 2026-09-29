package runtime

import (
	"strings"
	"testing"
)

func TestSpeakableStripsWhatASynthesiserWouldReadAloud(t *testing.T) {
	// The agent writes for a screen everywhere else and its habits come with
	// it: asterisks read as "asterisk", bullets become a monotone.
	in := "**Done.**\n\n- Closed the PR\n- Sent the message\n\nUse `karmax cost` next."
	got := speakable(in)

	for _, unwanted := range []string{"*", "`", "\n", "- "} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q survived into speech: %s", unwanted, got)
		}
	}
	// The words themselves must survive — this is a cleanup, not a summary.
	for _, want := range []string{"Done", "Closed the PR", "Sent the message"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q from: %s", want, got)
		}
	}
}

func TestIsLoopbackRefusesTheNetwork(t *testing.T) {
	// The relay speaks with the operator's memory and tools and authenticates
	// nobody, so reachability is the whole of its security.
	for _, ok := range []string{"127.0.0.1:54321", "[::1]:9999"} {
		if !isLoopback(ok) {
			t.Errorf("%s should be allowed", ok)
		}
	}
	for _, bad := range []string{"192.168.29.222:5000", "100.113.69.78:443", "10.0.3.1:80"} {
		if isLoopback(bad) {
			t.Errorf("%s must be refused", bad)
		}
	}
}

func TestVoiceSwitch(t *testing.T) {
	for _, tc := range []struct {
		voice, sarvam string
		want          bool
	}{
		{"on", "", true},
		{"ON", "", true},
		{"off", "key", false},
		{"", "key", true},
		{"", "", false},
	} {
		t.Setenv("KARMAX_VOICE", tc.voice)
		t.Setenv("SARVAM_API_KEY", tc.sarvam)
		if got := voiceEnabled(); got != tc.want {
			t.Errorf("KARMAX_VOICE=%q SARVAM=%q: enabled = %v, want %v", tc.voice, tc.sarvam, got, tc.want)
		}
	}
}
