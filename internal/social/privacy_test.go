package social

import (
	"errors"
	"strings"
	"testing"
)

func guard() Guard {
	return Guard{
		Forbidden: []string{"Siva", "CampX", "TrustStrike", "Kartik Deshmukh", "Newtra EV"},
		MaxRunes:  280,
	}
}

// The posts go out with nobody reading them first, so the rule is enforced
// rather than requested. These are the drafts a model actually produces on a
// day when something notable happened — which is exactly when it reaches for a
// name.
func TestDraftsThatNameSomebodyAreRefused(t *testing.T) {
	for _, draft := range []string{
		"Shipped the security audit for CampX today. Good milestone.",
		"Great call with Siva about the roadmap.",
		"TrustStrike's new scanner found three real issues this morning.",
		"Newtra EV went live 🎉",
		// A name inside a longer sentence, which is how it usually appears.
		"Spent the morning debugging, then a long call with siva about timelines.",
	} {
		if err := guard().Check(draft); err == nil {
			t.Errorf("published a draft that names somebody: %q", draft)
		}
	}
}

// Money, contact details and credentials are refused without needing to know
// anybody — an amount in a post about somebody's day is a leak by construction.
func TestDraftsThatLeakDetailsAreRefused(t *testing.T) {
	for name, draft := range map[string]string{
		"rupees":     "Closed a ₹2,00,000 deal today.",
		"rs":         "Invoice for Rs 30000 finally cleared.",
		"shorthand":  "80k in and the quarter is looking fine.",
		"dollars":    "Signed a $5k retainer this morning.",
		"phone":      "Call me on +91 75692 36628 if it breaks.",
		"jid":        "Message from 919999999999@s.whatsapp.net about the deploy.",
		"email":      "Reach me at nikhil@ghmev.in for the details.",
		"credential": "Debugged an auth bug, the token was sk-abc123def456ghi789 all along.",
		"internal":   "Fixed the dashboard at http://192.168.1.40:9091 finally.",
	} {
		t.Run(name, func(t *testing.T) {
			if err := guard().Check(draft); err == nil {
				t.Errorf("published a draft that leaks: %q", draft)
			}
		})
	}
}

// It has to let the good posts through, or it gets turned off.
func TestOrdinaryPostsArePublished(t *testing.T) {
	for _, draft := range []string{
		"Spent today making a WASM sandbox stop pretending it was safe. Turns out the guard was a comment, not a test.",
		"Shipped a thing that turns 900 lines of hand-rolled HTTP into 300 lines of library calls. Deleting code is the best part of the job.",
		"Reminder to self: a fallback nobody measures is just a slower way to be wrong.",
		"Three hours on a bug that was a five-field cron where six were expected.",
	} {
		if err := guard().Check(draft); err != nil {
			t.Errorf("refused an ordinary post: %q\n  %v", draft, err)
		}
	}
}

// A name must be a whole word, or the guard fires on everything and stops
// being used.
func TestPartialWordsAreNotNames(t *testing.T) {
	for _, draft := range []string{
		"Visited Sivakasi over the weekend.",  // not "Siva"
		"Working on the campaign copy today.", // not "CampX"
	} {
		if err := guard().Check(draft); err != nil {
			t.Errorf("a substring was treated as a name: %q\n  %v", draft, err)
		}
	}
}

// The refusal says what tripped it, because the model gets it back and has to
// write something different next time.
func TestARefusalExplainsItself(t *testing.T) {
	err := guard().Check("Closed the CampX deal for ₹2,00,000 today.")
	if err == nil {
		t.Fatal("not refused")
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("not a Refusal: %T", err)
	}
	if refusal.Reason == "" || len(refusal.Found) == 0 {
		t.Errorf("the refusal does not say what tripped it: %+v", refusal)
	}
	if !strings.Contains(err.Error(), "money") {
		t.Errorf("the message does not name the problem: %v", err)
	}
}

// Empty and oversized drafts never reach a platform.
func TestEmptyAndOversizedDraftsAreRefused(t *testing.T) {
	if err := guard().Check("   "); err == nil {
		t.Error("published an empty draft")
	}
	if err := guard().Check(strings.Repeat("a", 281)); err == nil {
		t.Error("published a draft over the platform limit")
	}
}

// A guard that refuses every post mentioning a screen is a guard that gets
// switched off, and then it is protecting nothing.
func TestScreenResolutionsAreNotMoney(t *testing.T) {
	for _, draft := range []string{
		"Finally fixed the 4k display scaling bug that has annoyed me for a month.",
		"Recorded the demo in 4k video and it was worth it.",
	} {
		if err := guard().Check(draft); err != nil {
			t.Errorf("refused a post about a screen: %q\n  %v", draft, err)
		}
	}
}

// A real address book is full of roles, not only people: "Agent", "Property
// Agent", "Hyd T Service". Treating those as names makes the words "agent" and
// "service" unpublishable, which is most of what somebody in software would
// write about their day.
func TestRolesInTheAddressBookAreNotNames(t *testing.T) {
	g := Guard{
		MaxRunes: 280,
		Forbidden: []string{
			"Agent", "Property Agent", "Hyd T Service", "Pavan Kumar Dishwash Service",
			"Agent Ram Uppal", "CampX",
		},
	}

	for _, draft := range []string{
		"Spent the day teaching an agent to admit when it does not know something.",
		"Pulled the reporting service out of the monolith. The extraction was the easy part.",
		"A property deal fell through and I got my afternoon back.",
	} {
		if err := g.Check(draft); err != nil {
			t.Errorf("refused an honest post: %q\n  %v", draft, err)
		}
	}

	// The person inside that role entry is still protected, and so is the client.
	for _, draft := range []string{
		"Long call with Uppal about the rollout.",
		"Shipped the first report for CampX today.",
	} {
		if err := g.Check(draft); err == nil {
			t.Errorf("published a draft that names somebody: %q", draft)
		}
	}
}

// The operator's own list wins over the dictionary in both directions.
func TestTheOperatorCanOverrideBothWays(t *testing.T) {
	// A name the automatic sources cannot know.
	strict := Guard{MaxRunes: 280, Forbidden: []string{"Srikanth"}}
	if err := strict.Check("Long call with Srikanth about timelines."); err == nil {
		t.Error("a name added by hand was not caught")
	}

	// And a word they are tired of seeing refused.
	relaxed := Guard{MaxRunes: 280, Forbidden: []string{"Newtra EV"}, Allowed: []string{"newtra"}}
	if err := relaxed.Check("Spent the morning on newtra ideas."); err != nil {
		t.Errorf("an allowed word was still treated as a name: %v", err)
	}
}
