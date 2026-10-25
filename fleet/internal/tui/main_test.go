package tui

import (
	"os"
	"testing"
	"time"
)

// The TUI shows times in the viewer's zone (time.Local); its tests, and
// their golden views, read the same on every machine in UTC.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
