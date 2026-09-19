package store

// The ledger's one job is that nobody is contacted twice, so that is what is
// tested hardest — including the cases that actually happened: a run
// interrupted halfway, and a second run started against the same campaign.

import (
	"sync"
	"testing"
)

func TestTheSamePersonCannotBeClaimedTwice(t *testing.T) {
	s := newTestStore(t)

	first, err := s.ClaimOutreach("camp-1", "instagram.dm", "1001")
	if err != nil || !first {
		t.Fatalf("the first claim must succeed: %v %v", first, err)
	}
	second, err := s.ClaimOutreach("camp-1", "instagram.dm", "1001")
	if err != nil {
		t.Fatalf("a repeat claim is an ordinary answer, not an error: %v", err)
	}
	if second {
		t.Error("the same person was claimed twice — this is the whole point of the table")
	}
}

func TestAnInterruptedAttemptStillBlocksARetry(t *testing.T) {
	// Claimed, then the process died before settling. We do not know whether
	// that message arrived, and "we do not know" is a reason not to send it
	// again rather than a reason to.
	s := newTestStore(t)
	if ok, _ := s.ClaimOutreach("camp-1", "instagram.dm", "1001"); !ok {
		t.Fatal("setup claim failed")
	}
	again, err := s.ClaimOutreach("camp-1", "instagram.dm", "1001")
	if err != nil || again {
		t.Errorf("a half-finished attempt must still block: %v %v", again, err)
	}
}

func TestDifferentCampaignsDoNotBlockEachOther(t *testing.T) {
	// Somebody who commented on two different posts can hear about both.
	s := newTestStore(t)
	if ok, _ := s.ClaimOutreach("camp-1", "instagram.dm", "1001"); !ok {
		t.Fatal("setup claim failed")
	}
	ok, err := s.ClaimOutreach("camp-2", "instagram.dm", "1001")
	if err != nil || !ok {
		t.Errorf("a different campaign must be able to reach the same person: %v %v", ok, err)
	}
}

func TestTwoRacingClaimsProduceExactlyOneWinner(t *testing.T) {
	// Two agents, or one agent retried under load. The database decides, not
	// whichever of them read the ledger last.
	s := newTestStore(t)
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ok, err := s.ClaimOutreach("camp-race", "instagram.dm", "1001")
			if err == nil && ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("exactly one claim must win, got %d", won)
	}
}

func TestTheCapCountsAttemptsNotSuccesses(t *testing.T) {
	// A cap that only counted successes would let a campaign that is failing
	// hammer away without limit — which is the shape of the run that got an
	// account blocked.
	s := newTestStore(t)
	for _, id := range []string{"1", "2", "3"} {
		if ok, _ := s.ClaimOutreach("camp-1", "instagram.dm", id); !ok {
			t.Fatalf("claim %s failed", id)
		}
	}
	if err := s.SettleOutreach("camp-1", "1", OutreachFailed, "nope"); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountOutreach("camp-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("want all 3 attempts counted, got %d", n)
	}
}

func TestAHardStopOutlivesTheRunThatSawIt(t *testing.T) {
	s := newTestStore(t)
	if ok, _ := s.ClaimOutreach("camp-1", "instagram.dm", "1001"); !ok {
		t.Fatal("setup claim failed")
	}
	if err := s.SettleOutreach("camp-1", "1001", OutreachStopped, "FeedbackRequired"); err != nil {
		t.Fatal(err)
	}

	stopped, why, err := s.CampaignStopped("camp-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("a stopped campaign must stay stopped — the account is what is at risk")
	}
	if why != "FeedbackRequired" {
		t.Errorf("the reason must survive too, got %q", why)
	}

	// And it must not leak into an unrelated campaign.
	if other, _, _ := s.CampaignStopped("camp-2"); other {
		t.Error("one campaign's stop must not halt another")
	}
}

func TestSettlingRecordsTheOutcome(t *testing.T) {
	s := newTestStore(t)
	if ok, _ := s.ClaimOutreach("camp-1", "instagram.dm", "1001"); !ok {
		t.Fatal("setup claim failed")
	}
	if err := s.SettleOutreach("camp-1", "1001", OutreachSent, "delivered"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListOutreach("camp-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != OutreachSent || rows[0].Target != "1001" {
		t.Errorf("want one sent row for 1001, got %+v", rows)
	}
}

func TestAClaimNeedsBothACampaignAndATarget(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.ClaimOutreach("", "instagram.dm", "1001"); err == nil {
		t.Error("an empty campaign would pool unrelated runs into one ledger")
	}
	if _, err := s.ClaimOutreach("camp-1", "instagram.dm", ""); err == nil {
		t.Error("an empty target would claim a row that matches nobody")
	}
}
