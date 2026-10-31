package aws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"
)

// A history entry says the states it moved between in its JSON; the
// one-line summary is the fallback when that does not parse.
func TestAHistoryEntryNamesBothStates(t *testing.T) {
	data := `{"version":"1.0","oldState":{"stateValue":"OK","stateReason":"fine"},
		"newState":{"stateValue":"ALARM","stateReason":"Threshold Crossed: 1 datapoint [93.2] was greater than the threshold (80.0)."}}`
	from, to, reason := parseHistory(data, "Alarm updated from OK to ALARM")
	if from != "OK" || to != "ALARM" || reason == "" {
		t.Fatalf("got %q -> %q (%q)", from, to, reason)
	}
	from, to, _ = parseHistory("not json", "Alarm updated from ALARM to OK")
	if from != "ALARM" || to != "OK" {
		t.Fatalf("from the summary: %q -> %q", from, to)
	}
}

// What went wrong decides what health says: log in again, ask for access, or
// just wait for the network.
func TestErrorsAreToldApartByWhatFixesThem(t *testing.T) {
	op := func(err error) error {
		return &smithy.OperationError{ServiceID: "CloudWatch", OperationName: "DescribeAlarms", Err: err}
	}
	for _, tc := range []struct {
		name         string
		err          error
		auth, denied bool
	}{
		{"denied", op(&smithy.GenericAPIError{Code: "AccessDeniedException", Message: "not authorized"}), false, true},
		{"cost explorer off", op(&smithy.GenericAPIError{Code: "AccessDeniedException", Message: "User not enabled for cost explorer access"}), false, true},
		{"expired token", op(&smithy.GenericAPIError{Code: "ExpiredTokenException"}), true, false},
		{"unknown key", op(&smithy.GenericAPIError{Code: "InvalidClientTokenId"}), true, false},
		{"no credentials", op(&v4.SigningError{Err: errors.New("failed to retrieve credentials: the SSO session has expired")}), true, false},
		{"throttled", op(&smithy.GenericAPIError{Code: "ThrottlingException"}), false, false},
		{"network", op(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), false, false},
		{"network while logging in", op(&v4.SigningError{Err: fmt.Errorf("failed to retrieve credentials: %w", &net.OpError{Op: "dial", Err: errors.New("no route")})}), false, false},
		{"timeout", context.DeadlineExceeded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.err)
			if isAuth(got) != tc.auth || isDenied(got) != tc.denied {
				t.Fatalf("classify(%v) = %T, want auth=%v denied=%v", tc.err, got, tc.auth, tc.denied)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("the original error is lost")
			}
		})
	}
}

// A profile the SDK finds no credentials for is a login to set up, not a crash.
func TestAProfileWithNoCredentialsIsALoginToSetUp(t *testing.T) {
	_, err := (&sdkAPI{}).Identity(context.Background())
	if !isAuth(err) {
		t.Fatalf("err = %v, want a login error", err)
	}
}

// Credits and refunds are left out of what an account is said to spend, or an
// account running on credits would read as costing nothing, spikes included.
func TestSpendIsCountedBeforeCredits(t *testing.T) {
	in := costInput("2026-10-01", "2026-11-01", nil, false)
	if in.Filter == nil || in.Filter.Not == nil || in.Filter.Not.Dimensions == nil ||
		in.Filter.Not.Dimensions.Key != "RECORD_TYPE" {
		t.Fatalf("filter = %+v, want credits and refunds left out", in.Filter)
	}
	got := in.Filter.Not.Dimensions.Values
	if len(got) != 2 || got[0] != "Credit" || got[1] != "Refund" {
		t.Errorf("left out = %v", got)
	}
	if with := costInput("2026-10-01", "2026-11-01", nil, true); with.Filter != nil {
		t.Errorf("credits included still filters: %+v", with.Filter)
	}
}
