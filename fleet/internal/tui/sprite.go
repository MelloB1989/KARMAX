package tui

import (
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// SpriteWidth is the cells a sprite takes.
const SpriteWidth = 3

// Sprite is the activity's sprite at an animation frame, exactly
// SpriteWidth cells wide. Task 6 replaces this placeholder with the
// animated set (spec section 5, Sprites); callers depend only on the
// signature and the width.
func Sprite(t *Theme, k fleetapi.ActivityKind, frame int) string {
	g := map[fleetapi.ActivityKind]string{
		fleetapi.ActReading: "◉‿◉", fleetapi.ActEditing: "⌨··", fleetapi.ActRunning: "⚙··",
		fleetapi.ActPushing: "🚀 ", fleetapi.ActTesting: "⚗··", fleetapi.ActThinking: "∴··",
		fleetapi.ActWaiting: "?··", fleetapi.ActIdle: "zzz", fleetapi.ActCooling: "❄··", fleetapi.ActDown: "✕··",
	}[k]
	if g == "" {
		g = "···"
	}
	return lipgloss.NewStyle().Foreground(t.Activity(k)).Render(Fit(g, SpriteWidth))
}
