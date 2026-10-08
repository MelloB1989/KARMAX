package karmahelper

import "testing"

func TestWithEarlierPassesKeepsTextSpokenBeforeTools(t *testing.T) {
	cases := []struct{ streamed, final, want string }{
		{"Here's the summary.Good night.", "Good night.", "Here's the summary. Good night."},
		{"Good night.", "Good night.", "Good night."},
		{"Task started.", "", "Task started."},
		{"", "", ""},
		{"unrelated", "Good night.", "Good night."},
	}
	for _, c := range cases {
		if got := withEarlierPasses(c.streamed, c.final); got != c.want {
			t.Errorf("withEarlierPasses(%q, %q) = %q, want %q", c.streamed, c.final, got, c.want)
		}
	}
}
