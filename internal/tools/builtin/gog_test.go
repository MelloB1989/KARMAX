package builtin

import "testing"

// A caller's explicit flag must never be overridden by the defaults we add.
func TestHasFlagRespectsExplicitChoices(t *testing.T) {
	for _, tc := range []struct {
		args []string
		flag string
		want bool
	}{
		{[]string{"gmail", "ls", "--json"}, "--json", true},
		{[]string{"gmail", "ls", "--account=me@x.com"}, "--account", true},
		{[]string{"gmail", "ls", "-a", "me@x.com"}, "-a", true},
		{[]string{"gmail", "ls"}, "--json", false},
		// A value that merely contains the flag name is not the flag.
		{[]string{"gmail", "search", "some --json text"}, "--json", false},
	} {
		if got := hasFlag(tc.args, tc.flag); got != tc.want {
			t.Errorf("hasFlag(%v, %q) = %v, want %v", tc.args, tc.flag, got, tc.want)
		}
	}
}

func TestHasFlagChecksAllAlternatives(t *testing.T) {
	if !hasFlag([]string{"gmail", "ls", "-j"}, "--json", "-j") {
		t.Error("the short form should count as the flag being set")
	}
	if hasFlag([]string{"gmail", "ls"}, "--json", "-j", "--plain") {
		t.Error("no flag was set")
	}
}
