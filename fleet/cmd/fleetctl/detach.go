package main

import (
	"bytes"
	"regexp"
	"strconv"
)

// csiKey is a key in a terminal's extended encodings: kitty's CSI u
// (key[:alternates];modifiers[:event]u) and xterm's modifyOtherKeys
// (27;modifiers;key~). A terminal sends these instead of control bytes once
// an application in the session (tmux, Claude Code) has asked it to.
var csiKey = regexp.MustCompile(`\x1b\[(?:([0-9]+)(?::[0-9:]*)?(?:;([0-9]+)(?::([0-9]+))?)?(?:;[0-9:]*)?u|27;([0-9]+);([0-9]+)~)`)

// detachAt is where in b the key that leaves the session starts, and its
// length; -1 when b has none. Ctrl-] leaves in every mode and encoding. In
// watch mode, where nothing typed reaches the session, q, Esc and Ctrl-C
// leave too.
func detachAt(b []byte, watch bool) (at, width int) {
	at, width = -1, 0
	take := func(i, n int) {
		if at < 0 || i < at {
			at, width = i, n
		}
	}
	if i := bytes.IndexByte(b, detachKey); i >= 0 {
		take(i, 1)
	}
	if watch {
		if i := bytes.IndexAny(b, "qQ\x03"); i >= 0 {
			take(i, 1)
		}
		if bytes.Equal(b, []byte{0x1b}) { // Esc alone; an arrow is Esc and more
			take(0, 1)
		}
	}
	for _, m := range csiKey.FindAllSubmatchIndex(b, -1) {
		key, mods, event := group(b, m, 1), group(b, m, 2), group(b, m, 3)
		if m[2] < 0 { // the modifyOtherKeys form
			key, mods, event = group(b, m, 5), group(b, m, 4), ""
		}
		if event == "3" { // a release: its press came first
			continue
		}
		code, _ := strconv.Atoi(key)
		if (code == ']' && ctrlOnly(mods)) ||
			(watch && (code == 27 && plain(mods) || code == 'c' && ctrlOnly(mods))) {
			take(m[0], m[1]-m[0])
		}
	}
	return at, width
}

func group(b []byte, m []int, i int) string {
	if m[2*i] < 0 {
		return ""
	}
	return string(b[m[2*i]:m[2*i+1]])
}

// mods is the CSI u modifier field: 1 + a bitmask (shift 1, alt 2, ctrl 4,
// super 8, hyper 16, meta 32, caps lock 64, num lock 128), 1 when absent.
func modBits(mods string) int {
	if mods == "" {
		return 0
	}
	n, err := strconv.Atoi(mods)
	if err != nil || n < 1 {
		return -1
	}
	return n - 1
}

// ctrlOnly: Ctrl, and no other modifier but the locks.
func ctrlOnly(mods string) bool { return modBits(mods)&^(64|128) == 4 }

// plain: no modifier but the locks.
func plain(mods string) bool { return modBits(mods)&^(64|128) == 0 }
