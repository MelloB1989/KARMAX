package instagram

// The guarantees, tested as guarantees.
//
// These do not send anything — they stop before the helper is ever reached,
// because everything worth pinning here happens before the message does. What
// is being checked is that there is no argument, no ordering and no failure
// mode that lets a caller past the ledger, the cap or a stop.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// fakeLedger is the store's behaviour without the store: claims are unique per
// campaign+target, exactly as the primary key makes them.
type fakeLedger struct {
	mu       sync.Mutex
	claimed  map[string]bool
	settled  map[string]string
	stopped  map[string]string
	claimErr error
}

func newFake() *fakeLedger {
	return &fakeLedger{claimed: map[string]bool{}, settled: map[string]string{}, stopped: map[string]string{}}
}

func (f *fakeLedger) ClaimOutreach(campaign, _, target string) (bool, error) {
	if f.claimErr != nil {
		return false, f.claimErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := campaign + "/" + target
	if f.claimed[k] {
		return false, nil
	}
	f.claimed[k] = true
	return true, nil
}

func (f *fakeLedger) SettleOutreach(campaign, target, state, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled[campaign+"/"+target] = state
	if state == stateStopped {
		f.stopped[campaign] = detail
	}
	return nil
}

func (f *fakeLedger) CountOutreach(campaign string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.claimed {
		if strings.HasPrefix(k, campaign+"/") {
			n++
		}
	}
	return n, nil
}

func (f *fakeLedger) CampaignStopped(campaign string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.stopped[campaign]
	return ok, d, nil
}

func TestSendingIsOffUntilItIsTurnedOn(t *testing.T) {
	t.Setenv("KARMAX_ENABLE_INSTAGRAM", "true")
	t.Setenv("KARMAX_INSTAGRAM_SEND", "")
	c := New()
	c.SetLedger(newFake())
	for _, tool := range c.Tools() {
		if strings.Contains(tool.Name, "send") || strings.Contains(tool.Name, "reply") {
			t.Errorf("%s must not exist until sending is explicitly enabled", tool.Name)
		}
	}
}

func TestEnablingTheConnectorDoesNotEnableSending(t *testing.T) {
	// Two switches, because reading an inbox and messaging a follower list are
	// different decisions.
	t.Setenv("KARMAX_ENABLE_INSTAGRAM", "true")
	t.Setenv("KARMAX_INSTAGRAM_SEND", "")
	if SendingEnabled() {
		t.Error("the connector being on must not imply sending is on")
	}
}

func TestWithNoLedgerThereAreNoSendTools(t *testing.T) {
	t.Setenv("KARMAX_INSTAGRAM_SEND", "true")
	c := New() // no ledger
	for _, tool := range c.Tools() {
		if strings.Contains(tool.Name, "send") {
			t.Error("without somewhere to record sends, the send tool must not appear")
		}
	}
}

func TestASendWithoutACampaignIsRefused(t *testing.T) {
	c := New()
	c.SetLedger(newFake())
	_, err := c.sendDM(context.Background(), connectorkit.Credentials{},
		map[string]any{"user_id": "1001", "text": "hello"})
	if err == nil || !strings.Contains(err.Error(), "campaign") {
		t.Errorf("a send with nothing to count against must be refused, got: %v", err)
	}
}

func TestTheSamePersonIsSkippedRatherThanMessagedTwice(t *testing.T) {
	f := newFake()
	c := New()
	c.SetLedger(f)
	// Pre-claim, as a previous run would have.
	if ok, _ := f.ClaimOutreach("camp", "instagram.dm", "1001"); !ok {
		t.Fatal("setup claim failed")
	}

	out, err := c.sendDM(context.Background(), connectorkit.Credentials{},
		map[string]any{"campaign": "camp", "user_id": "1001", "text": "hello"})
	if err != nil {
		t.Fatalf("a duplicate is an ordinary outcome, not an error: %v", err)
	}
	m, _ := out.(map[string]any)
	if m["skipped"] != true {
		t.Errorf("want a skip, got %+v", m)
	}
	// And crucially it must not have reached the helper — no session was even
	// established, so a send here would have failed loudly instead of quietly.
}

func TestAStoppedCampaignRefusesEverythingAfterIt(t *testing.T) {
	f := newFake()
	f.stopped["camp"] = "FeedbackRequired"
	c := New()
	c.SetLedger(f)

	_, err := c.sendDM(context.Background(), connectorkit.Credentials{},
		map[string]any{"campaign": "camp", "user_id": "9999", "text": "hello"})
	if err == nil {
		t.Fatal("a stopped campaign must refuse")
	}
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "does not resume") {
		t.Errorf("the refusal must say it is final, got: %v", err)
	}
	// A fresh recipient, never claimed — so this is the stop doing the work,
	// not the ledger.
	if f.claimed["camp/9999"] {
		t.Error("a stopped campaign must refuse before claiming anybody new")
	}
}

func TestTheCapIsEnforcedBeforeClaiming(t *testing.T) {
	t.Setenv("KARMAX_INSTAGRAM_CAP", "2")
	f := newFake()
	c := New()
	c.SetLedger(f)
	for _, id := range []string{"1", "2"} {
		if ok, _ := f.ClaimOutreach("camp", "instagram.dm", id); !ok {
			t.Fatal("setup claim failed")
		}
	}

	_, err := c.sendDM(context.Background(), connectorkit.Credentials{},
		map[string]any{"campaign": "camp", "user_id": "3", "text": "hello"})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("the cap must stop a third send, got: %v", err)
	}
	if f.claimed["camp/3"] {
		t.Error("hitting the cap must not consume the next recipient's claim")
	}
}

func TestTheCapCannotBeRaisedWithoutLimit(t *testing.T) {
	// An operator who wants a thousand is asking for the account back.
	t.Setenv("KARMAX_INSTAGRAM_CAP", "100000")
	c := New()
	if got := c.cap(); got > 200 {
		t.Errorf("the cap must stay bounded, got %d", got)
	}
}

func TestAHardStopHaltsTheWholeCampaignNotJustTheMessage(t *testing.T) {
	f := newFake()
	c := New()
	c.SetLedger(f)

	err := c.settle("camp", "1001", &Error{Type: "FeedbackRequired", HardStop: true})
	if err == nil {
		t.Fatal("a hard stop must surface as an error")
	}
	stopped, why, _ := f.CampaignStopped("camp")
	if !stopped || why != "FeedbackRequired" {
		t.Errorf("the campaign must be marked stopped with its reason, got %v %q", stopped, why)
	}
	if !strings.Contains(err.Error(), "Do not retry") {
		t.Errorf("the error must say not to retry, got: %v", err)
	}
}

func TestAnOrdinaryFailureDoesNotStopTheCampaign(t *testing.T) {
	// A network blip is not Instagram refusing the account, and treating it as
	// one would halt a campaign for no reason.
	f := newFake()
	c := New()
	c.SetLedger(f)

	_ = c.settle("camp", "1001", &Error{Type: "ClientError", HardStop: false})
	if stopped, _, _ := f.CampaignStopped("camp"); stopped {
		t.Error("only a hard stop halts a campaign")
	}
	if f.settled["camp/1001"] != stateFailed {
		t.Errorf("want it recorded as failed, got %q", f.settled["camp/1001"])
	}
}

func TestPacingHappensAndCannotBeArguedAway(t *testing.T) {
	// There is no parameter for this. The only way to observe it is that it
	// takes time, which is the point.
	start := time.Now()
	if err := pace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < pacingMin {
		t.Errorf("a send was paced by only %v, under the %v floor", elapsed, pacingMin)
	}
}

func TestPacingStillRespectsCancellation(t *testing.T) {
	// A paced send must not make a cancelled task hang for ten seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := pace(ctx); err == nil {
		t.Error("a cancelled context must abort the wait")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("cancellation must not wait out the full pause")
	}
}
