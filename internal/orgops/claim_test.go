package orgops

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/MelloB1989/karmax/internal/orgops/fakegithub"
)

func chatMsg(channelID, typ, content string) map[string]any {
	return map[string]any{"karmax_channel_id": channelID, "channel_type": typ, "channel_id": "spaces/eng",
		"content": content, "direction": "inbound"}
}

// pendingApproval opens one real approval and returns its code.
func pendingApproval(t *testing.T, h *harness) string {
	t.Helper()
	pr := h.fake.OpenPR(fakegithub.PR{Repo: "acme/auth-service", Title: "fix: token expiry", Body: "x", Author: "dev2",
		Files: []fakegithub.File{{Filename: "README.md", Patch: "@@ -1 +1,2 @@\n x\n+docs"}}})
	if _, err := h.svc.ReviewPR(context.Background(), h.kit, "acme/auth-service", pr.Number); err != nil {
		t.Fatal(err)
	}
	codes := h.pendingCodes()
	if len(codes) != 1 {
		t.Fatalf("want one pending approval, got %v", codes)
	}
	return codes[0]
}

func TestClaimsMessage(t *testing.T) {
	h := newHarness(t, nil)
	code := pendingApproval(t, h)

	cases := []struct {
		name string
		p    map[string]any
		want bool
	}{
		{"approve a pending code", chatMsg("gchat", "gchat", "approve "+code), true},
		{"approve, lower case and addressed", chatMsg("gchat", "gchat", "@karmax approve "+strings.ToLower(code)), true},
		{"reject a pending code", chatMsg("gchat", "gchat", "reject "+code), true},
		{"approve an unknown code stays with the agent", chatMsg("gchat", "gchat", "approve ZZ9ZZ"), false},
		{"approve with no code", chatMsg("gchat", "gchat", "approve"), false},
		{"fully parsed index", chatMsg("gchat", "gchat", "index all"), true},
		{"fully parsed create", chatMsg("gchat", "gchat", "create repo billing-api"), true},
		{"status", chatMsg("gchat", "gchat", "status"), true},
		{"partial parse: fix without an incident", chatMsg("gchat", "gchat", "fix the build"), false},
		{"partial parse: merge without a PR number", chatMsg("gchat", "gchat", "merge payments-api"), false},
		{"free-form question", chatMsg("gchat", "gchat", "What does payments-api depend on?"), false},
		{"whatsapp is not listened on by default", chatMsg("wa", "whatsapp", "status"), false},
		{"outbound is never claimed", func() map[string]any {
			m := chatMsg("gchat", "gchat", "index all")
			m["direction"] = "outbound"
			return m
		}(), false},
	}
	for _, c := range cases {
		if got := h.svc.ClaimsMessage("", c.p); got != c.want {
			t.Errorf("%s: claimed=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestClaimsMessageHonoursListenChannels(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ListenChannels = []string{"slack"} })
	if h.svc.ClaimsMessage("", chatMsg("gchat", "gchat", "status")) {
		t.Error("a channel not in listen_channels was claimed")
	}
	if !h.svc.ClaimsMessage("", chatMsg("slack", "slack", "status")) {
		t.Error("a listened channel was not claimed")
	}
}

// The router and the command loop both ask about one event. Once the loop's
// approve has consumed the pending row, a late router must still see "claimed".
func TestClaimDecisionSurvivesTheApproveItCaused(t *testing.T) {
	h := newHarness(t, nil)
	code := pendingApproval(t, h)
	msg := chatMsg("gchat", "gchat", "approve "+code)

	if !h.svc.ClaimsMessage("evt-1", msg) {
		t.Fatal("pending approve not claimed")
	}
	cmd, _ := ParseCommand("approve " + code)
	if err := h.svc.HandleCommand(context.Background(), h.kit, cmd, "gchat:spaces/eng", "Maya"); err != nil {
		t.Fatal(err)
	}
	if !h.svc.ClaimsMessage("evt-1", msg) {
		t.Fatal("same event claimed, then released once the approval was consumed")
	}
	// A different event for the now-consumed code goes to the agent.
	if h.svc.ClaimsMessage("evt-2", msg) {
		t.Fatal("a code with nothing pending was claimed")
	}
}

// The router and the command loop get the same event at the same moment. The
// loop's approve consumes the pending row; whichever asks second must still get
// the one decision already made, never a fresh "not claimed".
func TestClaimDecisionIsMadeOnceUnderConcurrency(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := newHarness(t, nil)
		code := pendingApproval(t, h)
		msg := chatMsg("gchat", "gchat", "approve "+code)
		id := fmt.Sprintf("evt-%d", i)

		var wg sync.WaitGroup
		var routerClaimed bool
		wg.Add(2)
		go func() { // the command loop
			defer wg.Done()
			if h.svc.ClaimsMessage(id, msg) {
				cmd, _ := ParseCommand("approve " + code)
				_ = h.svc.HandleCommand(context.Background(), h.kit, cmd, "gchat:spaces/eng", "Maya")
			}
		}()
		go func() { // the agent router
			defer wg.Done()
			routerClaimed = h.svc.ClaimsMessage(id, msg)
		}()
		wg.Wait()
		if !routerClaimed {
			t.Fatalf("run %d: the router handed a claimed approve to the agent", i)
		}
	}
}
