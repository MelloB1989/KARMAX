package main

import "testing"

// Ctrl-] leaves however the terminal sends it: once tmux or Claude Code in
// the session has asked for extended keys, a Mac's terminal sends it as an
// escape sequence, never the control byte.
func TestTheDetachKeyIsFoundInEveryEncoding(t *testing.T) {
	for _, c := range []struct {
		in        string
		watch     bool
		at, width int
	}{
		{"\x1d", false, 0, 1},
		{"ls\r\x1d", false, 3, 1},
		{"\x1b[93;5u", false, 0, 7},      // kitty / CSI u
		{"ab\x1b[93;5:1u", false, 2, 9},  // with its event type (press)
		{"\x1b[93;69u", false, 0, 8},     // Caps Lock on
		{"\x1b[93:125;5u", false, 0, 11}, // with an alternate key
		{"\x1b[27;5;93~", false, 0, 10},  // xterm modifyOtherKeys
		{"\x1b[93;5:3u", false, -1, 0},   // a release: the press came first
		{"\x1b[93;7u", false, -1, 0},     // Ctrl-Alt-]
		{"\x1b[93u", false, -1, 0},       // ] alone
		{"]", false, -1, 0},
		{"q", false, -1, 0},        // typing q in a session is typing
		{"\x03", false, -1, 0},     // and Ctrl-C goes to the agent
		{"q", true, 0, 1},          // watching, nothing typed reaches it
		{"\x1b", true, 0, 1},       // Esc
		{"\x1b[27u", true, 0, 5},   // Esc, disambiguated
		{"\x03", true, 0, 1},       // Ctrl-C
		{"\x1b[99;5u", true, 0, 7}, // Ctrl-C, CSI u
		{"\x1b[A", true, -1, 0},    // an arrow is not Esc
	} {
		at, width := detachAt([]byte(c.in), c.watch)
		if at != c.at || width != c.width {
			t.Errorf("detachAt(%q, watch %v) = %d, %d; want %d, %d", c.in, c.watch, at, width, c.at, c.width)
		}
	}
}
