// Package tui is fleetctl's full-screen terminal UI: the fleet as the PC2
// console shows it, with live screens, tasks, events, quota, hosts and
// access, branded KARMAX ▸ fleetctl, made by MelloB. It reads the fleet only
// through a fleetapi.Client, so it runs the same against fleetd or a Fake.
// docs/superpowers/specs/2026-10-25-fleetctl-tui-access-design.md, section 5.
//
// Files and who owns them (the plan's tasks 5 and 6):
//
//	tui.go      this contract: Options, Ctx, Tab, messages
//	theme.go    colors and styles (the console's palette)
//	widgets.go  bars, sparklines, ages: shared; add, do not change
//	sprite.go   the activity sprites (task 6)
//	app.go      the program: frame, boot, tab switching, the watch loop (task 5)
//	tab_*.go    one file per tab (Fleet, Tasks, Events: task 5; Screens,
//	            Quota, Hosts, Access: task 6)
package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"time"

	"github.com/MelloB1989/karmax/fleet/internal/clipboard"
	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Options start the TUI.
type Options struct {
	Client  fleetapi.Client
	Version string // fleetctl's, shown in the footer
	NoAnim  bool   // --no-anim, or NO_COLOR: no boot animation, sprites frozen
	// Attach runs a terminal session on a worker in this terminal (fleetctl
	// ssh). The TUI suspends while it runs (tea.Exec) and comes back to the
	// same view.
	Attach func(worker string, mode fleetapi.TermMode) tea.ExecCommand
	Now    func() time.Time // the clock; time.Now when nil (tests fix it)
	// Clipboard is this machine's (files, images): what the Sessions tab
	// pastes into a session with Ctrl+V. nil: none here.
	Clipboard clipboard.Clipboard
	// Local: the TUI runs on fleetd's own machine, so a pasted file's path
	// is already a path there.
	Local bool
	// DeskState is where the Sessions tab keeps its choices (the workspace
	// shown, each one's focus, the sidebar); "" keeps none.
	DeskState string
	// FilesState is where the Files tab keeps its own (the folders shown,
	// where downloads go); "" keeps none.
	FilesState string
}

// Ctx is what every tab draws from and acts through. The app owns it; a
// tab never keeps its own copy of the snapshot.
type Ctx struct {
	Snap   *fleetapi.Snapshot // nil until the first watch answers
	Client fleetapi.Client
	Theme  *Theme
	Now    time.Time
	Frame  int // the animation frame: +1 about six times a second; fixed under NoAnim
	NoAnim bool
	Attach func(worker string, mode fleetapi.TermMode) tea.Cmd
	// Working is the working count of every snapshot the TUI has seen,
	// for the Fleet tab's pulse.
	Working *Samples
	// Selected is the worker the user last selected, shared by tabs so
	// Fleet → Screens keeps the same agent in view.
	Selected string
	// Clipboard, Local and DeskState are the Options' (the Sessions tab's).
	Clipboard  clipboard.Clipboard
	Local      bool
	DeskState  string
	FilesState string
}

// What a tab may be besides a Tab.
type (
	// fullscreener draws the whole terminal, no frame around it.
	fullscreener interface{ Fullscreen() bool }
	// rawer takes every key while Raw, Ctrl-C and the app's own too.
	rawer interface{ Raw() bool }
	// cursorer puts the terminal's cursor somewhere (nil hides it).
	cursorer interface{ Cursor() *tea.Cursor }
	// mouser wants the mouse.
	mouser interface{ Mouse() bool }
	// titler names the terminal window ("" keeps the app's).
	titler interface{ WindowTitle() string }
	// focuser wants to hear when the terminal window gains or loses focus.
	focuser interface{ ReportFocus() bool }
	// backgrounder takes its own messages while another tab is shown.
	backgrounder interface{ Background(msg tea.Msg) bool }
	// waker starts its work as snapshots come, shown or not.
	waker interface{ Wake(c *Ctx) tea.Cmd }
	// claimer takes a key the app would otherwise take (tab), while Claims.
	claimer interface{ Claims(k string) bool }
)

// Tab is one tab of the TUI.
type Tab interface {
	Title() string
	// Visible: Access is for admins; Hosts is not for guests.
	Visible(me fleetapi.Me) bool
	// Update handles a message while the tab is shown (keys, and the
	// messages its own commands return).
	Update(msg tea.Msg, c *Ctx) tea.Cmd
	// View draws the tab's body in exactly w×h cells.
	View(c *Ctx, w, h int) string
	// Help is the tab's keys, for the footer and ?.
	Help() []key.Binding
	// Capturing reports that the tab has a text field focused, so the app
	// does not take its keys (tab switching, q).
	Capturing() bool
}

// Messages the app delivers to every tab.
type (
	// SnapshotMsg is a new snapshot from the watch loop.
	SnapshotMsg struct{ Snap *fleetapi.Snapshot }
	// WatchErrMsg is the watch loop's failure; the app shows a banner and
	// retries, keeping the last snapshot.
	WatchErrMsg struct{ Err error }
	// FrameMsg advances the animation.
	FrameMsg struct{}
	// ToastMsg is a short notice in the footer (an action's result).
	ToastMsg struct {
		Text string
		Err  bool
	}
)
