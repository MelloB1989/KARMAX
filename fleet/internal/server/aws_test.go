package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// The fake fleet shows no AWS account, as fleetd without aws: in fleet.yaml.
func (f *fake) AWS(context.Context, bool) (*fleetapi.AWSReport, error) { return nil, nil }

// awsFake is the fake fleet with an AWS account to show. Each read waits for
// release when it is set.
type awsFake struct {
	*fake
	mu      sync.Mutex
	reads   []bool // each read's refresh
	started chan struct{}
	release chan struct{}
}

func (a *awsFake) AWS(ctx context.Context, refresh bool) (*fleetapi.AWSReport, error) {
	a.mu.Lock()
	a.reads = append(a.reads, refresh)
	a.mu.Unlock()
	if a.started != nil {
		close(a.started)
	}
	if a.release != nil {
		<-a.release
	}
	return &fleetapi.AWSReport{Account: "123456789012", Regions: []string{"ap-south-1"}, Items: []fleetapi.AWSResource{{Service: "s3", Region: "global", ID: "example-logs"}}, Errors: []string{}}, nil
}

func TestAWSIsForTheOperatorOnly(t *testing.T) {
	a := &awsFake{fake: &fake{}}
	h := Handler(a, Tokens{Full: "full", Relay: "relay", Console: "console"}, nil)
	rec := do(t, h, "GET", fleetapi.PathAWS, "full", "")
	var r fleetapi.AWSReport
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &r) != nil || r.Account != "123456789012" || len(r.Items) != 1 {
		t.Fatalf("full: %d %s", rec.Code, rec.Body)
	}
	for _, tok := range []string{"relay", "console"} {
		if rec := do(t, h, "GET", fleetapi.PathAWS, tok, ""); rec.Code != 403 {
			t.Errorf("%s: %d %s", tok, rec.Code, rec.Body)
		}
	}
	// On the tailnet: an admin device reads it, a guest does not.
	tn := Tailnet(h, tailnetWho, tagDevices(t), &config.Tailnet{Listen: "100.64.0.1:7878", GuestOpen: 2, Dist: t.TempDir()})
	if rec := tdo(t, tn, "GET", fleetapi.PathAWS, opIP, "", ""); rec.Code != 200 {
		t.Errorf("an admin device: %d %s", rec.Code, rec.Body)
	}
	if rec := tdo(t, tn, "GET", fleetapi.PathAWS, ramIP, "", ""); rec.Code != 403 {
		t.Errorf("a guest: %d %s", rec.Code, rec.Body)
	}
	if len(a.reads) != 2 {
		t.Errorf("%d reads, want the operator's two", len(a.reads))
	}
}

func TestAWSRefreshIsPassedOn(t *testing.T) {
	a := &awsFake{fake: &fake{}}
	h := Handler(a, Tokens{Full: "full"}, nil)
	do(t, h, "GET", fleetapi.PathAWS, "full", "")
	do(t, h, "GET", fleetapi.PathAWS+"?refresh=1", "full", "")
	if len(a.reads) != 2 || a.reads[0] || !a.reads[1] {
		t.Errorf("reads %v, want [false true]", a.reads)
	}
}

// Without aws: in fleet.yaml the route says so.
func TestAWSNotSetUp(t *testing.T) {
	_, h := setup(t)
	rec := do(t, h, "GET", fleetapi.PathAWS, "full", "")
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "aws is not set up in fleet.yaml") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// Callers who ask while a read is on its way share it.
func TestAWSCallersShareOneRead(t *testing.T) {
	a := &awsFake{fake: &fake{}, started: make(chan struct{}), release: make(chan struct{})}
	h := Handler(a, Tokens{Full: "full"}, nil)
	var wg sync.WaitGroup
	codes := make([]int, 3)
	ask := func(i int) {
		wg.Go(func() { codes[i] = do(t, h, "GET", fleetapi.PathAWS+"?refresh=1", "full", "").Code })
	}
	ask(0)
	<-a.started
	ask(1)
	ask(2)
	time.Sleep(50 * time.Millisecond) // the two are waiting on the first
	close(a.release)
	wg.Wait()
	if len(a.reads) != 1 || codes[0] != 200 || codes[1] != 200 || codes[2] != 200 {
		t.Errorf("%d reads, codes %v", len(a.reads), codes)
	}
	// The next one reads again.
	a.started = nil
	do(t, h, "GET", fleetapi.PathAWS, "full", "")
	if len(a.reads) != 2 {
		t.Errorf("%d reads after the shared one", len(a.reads))
	}
}

// A caller who gives up does not end the read for the others.
func TestAWSACallerWhoLeavesDoesNotStopTheRead(t *testing.T) {
	a := &awsFake{fake: &fake{}, started: make(chan struct{}), release: make(chan struct{})}
	h := Handler(a, Tokens{Full: "full"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan int)
	go func() {
		req := httptest.NewRequest("GET", fleetapi.PathAWS, nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer full")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		gone <- rec.Code
	}()
	<-a.started
	cancel()
	<-gone
	done := make(chan int)
	go func() { done <- do(t, h, "GET", fleetapi.PathAWS, "full", "").Code }()
	time.Sleep(20 * time.Millisecond)
	close(a.release)
	if code := <-done; code != 200 || len(a.reads) != 1 {
		t.Errorf("the second caller: %d, %d reads", code, len(a.reads))
	}
}
