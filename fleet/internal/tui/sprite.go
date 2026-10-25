package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// SpriteWidth is the cells a sprite takes.
const SpriteWidth = 3

// sprites act out each activity (spec section 5, Sprites). Every frame is
// SpriteWidth runes, each one cell wide in every terminal, East Asian ones
// included: no ambiguous-width runes (·, •, ●, ∴, box drawing) and no
// emoji. Braille cells carry the motion: a dot moves within a cell, so
// things rise and fall in a single row. Frame 0 is the pose a frozen
// sprite (--no-anim) holds, so it must read on its own.
var sprites = map[fleetapi.ActivityKind][]string{
	// eyes scanning a line of text, left to right and back
	fleetapi.ActReading: {"ʘʘ⠶", "⠶ʘʘ", "⠶ʘʘ", "ʘʘ⠶", "ʘ⠶ʘ"},
	// keys clattering: a line of text grows, then the cursor blinks
	fleetapi.ActEditing: {"⌨⠤⠄", "⌨⠄ ", "⌨⠤ ", "⌨⠤⠄", "⌨⠤⠤", "⌨⠤⠤"},
	// two gears turning against each other
	fleetapi.ActRunning: {"⚙⠋⠏", "⚙⠙⠇", "⚙⠸⠦", "⚙⠴⠴", "⚙⠦⠸", "⚙⠇⠙"},
	// a rocket lifting off on its exhaust
	fleetapi.ActPushing: {"⇡⣀ ", "⇡⠤⣀", "⇡⠒⠤", "⇡⠉⠒", "⇡ ⠉"},
	// a flask with bubbles rising out of it
	fleetapi.ActTesting: {"⚗⠂⠄", "⚗⡀ ", "⚗⠄⡀", "⚗⠂⠄", "⚗⠁⠂", "⚗ ⠁"},
	// a thought bubble growing
	fleetapi.ActThinking: {"ₒoO", "ₒ  ", "ₒo ", "ₒoO", "ₒoO"},
	// a blinking ?, waiting on a person
	fleetapi.ActWaiting: {" ? ", " ? ", " ? ", "   "},
	// z's drifting up and away
	fleetapi.ActIdle: {"zᶻᶻ", "z  ", "zᶻ ", "zᶻᶻ", " ᶻᶻ", "  ᶻ"},
	// a snowflake with snow falling past it
	fleetapi.ActCooling: {"❆⠁ ", "❆⠂⠁", "❆⠄⠂", "❆⡀⠄", "❆ ⡀"},
	// down: a static ✕
	fleetapi.ActDown: {" ✕ "},
}

// unknownSprite is an activity this build does not know yet.
var unknownSprite = []string{" ⠒ "}

func spriteFrames(k fleetapi.ActivityKind) []string {
	if f, ok := sprites[k]; ok {
		return f
	}
	return unknownSprite
}

// Sprite is the activity's sprite at an animation frame, exactly
// SpriteWidth cells wide. A fixed frame draws the same sprite every time.
func Sprite(t *Theme, k fleetapi.ActivityKind, frame int) string {
	fr := spriteFrames(k)
	i := frame % len(fr)
	if i < 0 {
		i += len(fr)
	}
	return lipgloss.NewStyle().Foreground(t.Activity(k)).Render(Fit(fr[i], SpriteWidth))
}
