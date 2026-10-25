package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/clipperhouse/displaywidth"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

var allActivities = []fleetapi.ActivityKind{
	fleetapi.ActThinking, fleetapi.ActReading, fleetapi.ActEditing, fleetapi.ActRunning, fleetapi.ActPushing,
	fleetapi.ActTesting, fleetapi.ActWaiting, fleetapi.ActIdle, fleetapi.ActCooling, fleetapi.ActDown,
}

// eastAsian measures as a CJK terminal does, where an ambiguous rune is two
// cells: a sprite rune must be one cell there too.
var eastAsian = displaywidth.Options{EastAsianWidth: true}

func TestEverySpriteFrameIsThreeCells(t *testing.T) {
	th := NewTheme(true)
	for _, k := range append(allActivities, "", "something-new") {
		for f := -1; f < 2*len(spriteFrames(k)); f++ {
			s := Sprite(th, k, f)
			if got := lipgloss.Width(s); got != SpriteWidth {
				t.Errorf("Sprite(%q, %d) = %q is %d cells", k, f, ansi.Strip(s), got)
			}
		}
		for _, fr := range spriteFrames(k) {
			for _, r := range fr {
				if lipgloss.Width(string(r)) != 1 || eastAsian.Rune(r) != 1 {
					t.Errorf("%q in the %q sprite is not one cell everywhere", r, k)
				}
			}
			if n := len([]rune(fr)); n != SpriteWidth {
				t.Errorf("a %q frame %q has %d runes", k, fr, n)
			}
		}
	}
}

func TestSpritesMoveOnlyWithFrames(t *testing.T) {
	th := NewTheme(true)
	for _, k := range allActivities {
		if a, b := Sprite(th, k, 0), Sprite(th, k, 0); a != b {
			t.Errorf("%q at a fixed frame changed: %q then %q", k, a, b)
		}
		moved := false
		for f := 1; f < 6; f++ {
			moved = moved || Sprite(th, k, f) != Sprite(th, k, 0)
		}
		if k == fleetapi.ActDown && moved {
			t.Errorf("down moved; it is a static ✕")
		}
		if k != fleetapi.ActDown && !moved {
			t.Errorf("%q never moves", k)
		}
	}
}

func TestEveryActivityHasASprite(t *testing.T) {
	seen := map[string]fleetapi.ActivityKind{}
	for _, k := range allActivities {
		fr := spriteFrames(k)
		if _, ok := sprites[k]; !ok {
			t.Errorf("%q has no sprite of its own", k)
			continue
		}
		if k != fleetapi.ActDown && (len(fr) < 4 || len(fr) > 6) {
			t.Errorf("%q has %d frames, want 4 to 6", k, len(fr))
		}
		if other, dup := seen[fr[0]]; dup {
			t.Errorf("%q and %q look the same at rest: %q", k, other, fr[0])
		}
		seen[fr[0]] = k
	}
}
