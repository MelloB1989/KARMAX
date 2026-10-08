package runtime

import (
	"github.com/MelloB1989/karmax/internal/memory"
	"strings"
	"testing"
	"time"
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

// GitLoom hits arrive with empty snippets and the body in Content. Reading
// only the excerpt threw every hit away, so every call-time lookup was empty.
func TestHitTextFallsBackToTheBody(t *testing.T) {
	withBody := memory.SearchResult{Entry: memory.MemoryEntry{Content: "CampX: final report delivered 12 Sep"}}
	if got := hitText(withBody); got != "CampX: final report delivered 12 Sep" {
		t.Fatalf("hitText = %q, want the body when there is no excerpt", got)
	}
	withExcerpt := memory.SearchResult{Excerpt: "short", Entry: memory.MemoryEntry{Content: "long body"}}
	if got := hitText(withExcerpt); got != "short" {
		t.Fatalf("hitText = %q, want the excerpt when there is one", got)
	}
}

// A slow memory layer must not hold up a reply: past the budget the turn goes
// ahead without the head start.
func TestPreAnswerLookupIsBounded(t *testing.T) {
	slow := &voiceMemoryLookup{}
	start := time.Now()
	done := make(chan []string, 1)
	go func() { done <- slow.linesWithin("anything", 5, 50*time.Millisecond) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("linesWithin did not return")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("took %v", time.Since(start))
	}
}
