package social

import "testing"

// The whole point of the dictionary: it must separate topics from names on the
// real examples that broke the guard, without being taught each one.
func TestTheDictionarySeparatesTopicsFromNames(t *testing.T) {
	topics := []string{
		"automation", "alerts", "personal", "project", "service", "call",
		"funding", "college", "drive", "events", "deployment", "security",
		"memory", "gateway", "sandbox", "runtime", "workflow", "harness",
	}
	for _, w := range topics {
		if !Topic(w) {
			t.Errorf("%q is a topic and would be treated as a name", w)
		}
	}

	names := []string{"campx", "srikanth", "newtra", "truststrike", "wacli"}
	for _, w := range names {
		if Topic(w) {
			t.Errorf("%q is a name and would stop being protected", w)
		}
	}
}
