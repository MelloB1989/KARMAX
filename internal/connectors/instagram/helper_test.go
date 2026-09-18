package instagram

// Exercising the real child process, not a mock.
//
// The whole point of this connector is a pipe to a Python process, so a fake
// on this side would test the half that was never in doubt. These spawn the
// helper for real and speak the protocol to it; they need an interpreter with
// instagrapi in it and skip when there is none, because a machine that has not
// provisioned the helper yet is a normal machine, not a broken one.
//
// Nothing here talks to Instagram. Every case is either local (ping, an
// unknown method) or a failure that happens before any request goes out.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func testHelper(t *testing.T) *helper {
	t.Helper()
	python := strings.TrimSpace(os.Getenv("KARMAX_INSTAGRAM_PYTHON_PATH"))
	if python == "" {
		t.Skip("set KARMAX_INSTAGRAM_PYTHON_PATH to an interpreter with instagrapi to run this")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !usable(ctx, python) {
		t.Skipf("%s cannot import instagrapi", python)
	}
	h := &helper{}
	t.Cleanup(h.stop)
	return h
}

func TestPingReportsAWorkingHelper(t *testing.T) {
	h := testHelper(t)
	var out struct {
		OK         bool   `json:"ok"`
		Instagrapi string `json:"instagrapi"`
		Python     string `json:"python"`
		LoggedIn   bool   `json:"logged_in"`
	}
	if err := h.call(context.Background(), "ping", nil, &out); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !out.OK {
		t.Error("ping did not report ok")
	}
	if out.LoggedIn {
		t.Error("a fresh helper must not claim to be signed in")
	}
	// "unknown" here means the version lookup regressed to the __version__
	// attribute instagrapi does not have.
	if out.Instagrapi == "" || out.Instagrapi == "unknown" {
		t.Errorf("no usable instagrapi version reported: %q", out.Instagrapi)
	}
	if out.Python == "" {
		t.Error("no python version reported")
	}
}

func TestUnknownMethodIsAnErrorRatherThanAHang(t *testing.T) {
	h := testHelper(t)
	err := h.call(context.Background(), "definitely-not-a-method", nil, nil)
	if err == nil {
		t.Fatal("an unknown method must fail")
	}
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("want a helper Error, got %T: %v", err, err)
	}
	if he.HardStop {
		t.Error("a typo must not be reported as one of Instagram's stop signals")
	}
}

func TestLoginWithoutCredentialsSaysWhichOnesAreMissing(t *testing.T) {
	h := testHelper(t)
	err := h.call(context.Background(), "login", map[string]any{}, nil)
	if err == nil {
		t.Fatal("login with nothing must fail")
	}
	// The operator has to learn there are two routes in, not just that it failed.
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "sessionid") || !strings.Contains(low, "password") {
		t.Errorf("the error must name both ways to sign in, got: %v", err)
	}
}

func TestTheHelperSurvivesManyCallsOnOneProcess(t *testing.T) {
	h := testHelper(t)
	// A desynchronised pipe shows up as the second call reading the first
	// call's reply, so the ids have to keep lining up across a run.
	for i := 0; i < 5; i++ {
		var out struct {
			OK bool `json:"ok"`
		}
		if err := h.call(context.Background(), "ping", nil, &out); err != nil {
			t.Fatalf("ping %d: %v", i, err)
		}
		if !out.OK {
			t.Fatalf("ping %d came back not-ok, which means replies are off by one", i)
		}
	}
}

func TestEnsureRefusesWhenTheConnectorIsOff(t *testing.T) {
	t.Setenv("KARMAX_ENABLE_INSTAGRAM", "")
	c := New()
	_, err := c.ensure(context.Background(), connectorkit.Credentials{
		Config: map[string]string{"username": "someone", "sessionid": "irrelevant"},
	})
	if err == nil {
		t.Fatal("a disabled connector must refuse before spawning anything")
	}
	if !strings.Contains(err.Error(), "KARMAX_ENABLE_INSTAGRAM") {
		t.Errorf("the refusal must say how to turn it on, got: %v", err)
	}
}

func TestExpiredSessionIsExplainedRatherThanEchoed(t *testing.T) {
	// LoginRequired from a sessionid login means the cookie died, which the
	// operator fixes in their browser — not by reading instagrapi's wording.
	err := loginFailed(&Error{Type: "LoginRequired", Message: "login_required", HardStop: true},
		false, true)
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "sign in again in the browser") {
		t.Errorf("want the browser instruction, got: %v", err)
	}
}

func TestAHardStopSaysToLeaveTheAccountAlone(t *testing.T) {
	err := loginFailed(&Error{Type: "FeedbackRequired", Message: "spam", HardStop: true}, false, false)
	low := strings.ToLower(err.Error())
	if !strings.Contains(low, "flagged") {
		t.Errorf("a hard stop must say the account was flagged, got: %v", err)
	}
}
