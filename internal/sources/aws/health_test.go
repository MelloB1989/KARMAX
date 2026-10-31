package aws

import (
	"reflect"
	"strings"
	"testing"
)

// One line for the source as a whole: working while anything reads, and
// naming every login that would bring the rest back.
func TestTheSourceIsSummarisedInOneLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hs     []Health
		status string
		has    []string
	}{
		{"nothing configured", nil, "not_configured", []string{"no AWS profiles"}},
		{"all well", []Health{{Profile: "a", Status: StatusHealthy}, {Profile: "b", Status: StatusSkipped}}, StatusHealthy, []string{"1 account read"}},
		{"some logins out", []Health{{Profile: "a", Status: StatusHealthy}, {Profile: "b", Status: StatusFailed, Fix: "aws sso login --sso-session example-sso"}},
			StatusDegraded, []string{"1 account read", "aws sso login --sso-session example-sso"}},
		{"every login out", []Health{{Profile: "b", Status: StatusFailed, Fix: "aws login"}, {Profile: "c", Status: StatusFailed, Fix: "aws login"}},
			StatusFailed, []string{"no account read", "aws login"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := Summary(tc.hs)
			if status != tc.status {
				t.Errorf("status = %q, want %q (%s)", status, tc.status, detail)
			}
			for _, want := range tc.has {
				if !strings.Contains(detail, want) {
					t.Errorf("detail %q lacks %q", detail, want)
				}
			}
		})
	}
}

// The operator hears about a login once, when it is newly needed, not on
// every poll it stays needed.
func TestOnlyNewlyNeededLoginsAreTold(t *testing.T) {
	if got := NewFixes([]string{"aws login"}, []string{"aws login", "aws sso login --sso-session example-sso"}); !reflect.DeepEqual(got, []string{"aws sso login --sso-session example-sso"}) {
		t.Errorf("new = %v", got)
	}
	if got := NewFixes([]string{"aws login", "aws sso login --sso-session example-sso"}, []string{"aws login"}); len(got) != 0 {
		t.Errorf("a login fixed is not news: %v", got)
	}
}
