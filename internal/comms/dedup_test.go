package comms

import (
	"testing"
	"time"
)

// The three pairs actually sent to one contact — six messages, no replies.
// Two pairs identical, one reworded, each pair under a minute apart.
func TestTheDuplicatesThatWereActuallySent(t *testing.T) {
	now := time.Date(2026, 8, 12, 17, 24, 56, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		content string
		prev    []pastMessage
	}{
		{
			name:    "identical 30s apart",
			content: "Hi Lucia, just wanted to let you know — the process is done. Please let us know if there's anything else needed from our end.",
			prev: []pastMessage{{
				Content: "Hi Lucia, just wanted to let you know — the process is done. Please let us know if there's anything else needed from our end.",
				At:      now.Add(-30 * time.Second),
			}},
		},
		{
			name:    "identical 31s apart",
			content: "Hi Lucia, just letting you know the process is done.",
			prev: []pastMessage{{
				Content: "Hi Lucia, just letting you know the process is done.",
				At:      now.Add(-31 * time.Second),
			}},
		},
		{
			name:    "same thing reworded 16s apart",
			content: "Hi Lucia, my other phone number has now been linked and updated. If it works for you, I can come by today around 2-3pm.",
			prev: []pastMessage{{
				Content: "Hi Lucia, just a quick update — my other phone number has now been linked and updated. If needed, I can come by today around 2pm.",
				At:      now.Add(-16 * time.Second),
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why, dup := isRepeat(tc.content, tc.prev, now, false)
			if !dup {
				t.Fatalf("isRepeat() = false; this message was sent twice in the real thread")
			}
			if why == "" {
				t.Error("a refusal must say why")
			}
		})
	}
}

// Suppression that swallows legitimate messages is worse than the duplicates.
func TestLegitimateMessagesStillGoOut(t *testing.T) {
	now := time.Date(2026, 8, 12, 17, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		content string
		prev    []pastMessage
	}{
		{
			name:    "a genuine follow-up days later",
			content: "Hi Lucia, just wanted to let you know the process is done.",
			prev: []pastMessage{{
				Content: "Hi Lucia, just wanted to let you know the process is done.",
				At:      now.Add(-72 * time.Hour),
			}},
		},
		{
			name:    "a different message minutes later",
			content: "Also, could you confirm the joining date for the new hire?",
			prev: []pastMessage{{
				Content: "Hi Lucia, just wanted to let you know the process is done.",
				At:      now.Add(-2 * time.Minute),
			}},
		},
		{
			name:    "short acks are not each other",
			content: "ok done",
			prev:    []pastMessage{{Content: "sure thing", At: now.Add(-1 * time.Minute)}},
		},
		{
			name:    "a rephrase much later is a real nudge",
			content: "Hi Lucia, following up on the process — is anything still pending from our end?",
			prev: []pastMessage{{
				Content: "Hi Lucia, just wanted to let you know the process is done, anything pending from our end?",
				At:      now.Add(-5 * time.Hour),
			}},
		},
		{
			name:    "nothing sent before",
			content: "Hi Lucia, the process is done.",
			prev:    nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if why, dup := isRepeat(tc.content, tc.prev, now, false); dup {
				t.Fatalf("isRepeat() = true (%s); this message should have gone out", why)
			}
		})
	}
}

// Two callers racing must not both get through: the store cannot help when
// neither send has landed in it yet.
func TestConcurrentIdenticalSendsOnlyReserveOnce(t *testing.T) {
	g := newSendGuard()
	now := time.Now()
	key := "15555550103@s.whatsapp.net\x00the process is done"
	if !g.reserve(key, now) {
		t.Fatal("the first send must be allowed")
	}
	if g.reserve(key, now) {
		t.Fatal("the second identical send must be refused while the first is in flight")
	}
	g.release(key)
	if !g.reserve(key, now) {
		t.Fatal("once the first send finishes the key must be free again")
	}
}

// The incident: three chat messages from one person arrived seconds apart,
// each an urgent page. The pages share their frame and the sender's name, so
// the rephrase test called the second and third repeats and they never went.
func TestTwoNoticesAboutDifferentThingsBothGoOut(t *testing.T) {
	now := time.Date(2026, 11, 21, 8, 49, 6, 0, time.UTC)
	first := "⚠️ Urgent — Chat DM from Omar Haddad: team subscription (gchat · ops@example.com)\nOmar sent \"team subscription\" with no question in it. I have not replied."
	second := "⚠️ Urgent — Chat DM from Omar Haddad: premium seat (gchat · ops@example.com)\nOmar sent \"premium seat\" with no question in it. I have not replied."
	prev := []pastMessage{{Content: first, At: now.Add(-3 * time.Second)}}

	if why, dup := isRepeat(second, prev, now, true); dup {
		t.Errorf("a page about a different message was refused (%s); the operator never saw it", why)
	}
	if _, dup := isRepeat(first, prev, now, true); !dup {
		t.Error("the same page twice is still a repeat")
	}
	// The same pair as free prose is what the rephrase test exists for.
	if _, dup := isRepeat(second, prev, now, false); !dup {
		t.Error("this pair no longer trips the rephrase test, so it does not show what a notice is spared")
	}
}
