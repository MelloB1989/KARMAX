package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Stand-ins for task 6's tabs, so the frame runs before they merge. The
// merge deletes this file.

func newScreens() Tab { return &stubTab{title: "Screens"} }
func newQuota() Tab   { return &stubTab{title: "Quota"} }
func newHosts() Tab {
	return &stubTab{title: "Hosts", visible: func(me fleetapi.Me) bool { return me.Role != fleetapi.RoleGuest }}
}
func newAccess() Tab {
	return &stubTab{title: "Access", visible: func(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }}
}

// stubTab is a tab that says it is coming.
type stubTab struct {
	title   string
	visible func(fleetapi.Me) bool
}

func (s *stubTab) Title() string { return s.title }
func (s *stubTab) Visible(me fleetapi.Me) bool {
	return s.visible == nil || s.visible(me)
}
func (s *stubTab) Update(tea.Msg, *Ctx) tea.Cmd { return nil }
func (s *stubTab) View(c *Ctx, w, h int) string {
	return center(c.Theme.Muted.Render(s.title+" is coming"), w, h)
}
func (s *stubTab) Help() []key.Binding { return nil }
func (s *stubTab) Capturing() bool     { return false }
