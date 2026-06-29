package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runLoopsCmd launches the `karmax loops` TUI for installing/removing loopkit
// loops. `karmax loops list` prints them headlessly (for scripting/CI).
func runLoopsCmd(args []string) {
	root, err := loopinstall.RepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(args) > 0 && args[0] == "list" {
		loops := loopkit.Registered()
		fmt.Printf("Active loops (%d):\n", len(loops))
		for _, l := range loops {
			fmt.Printf("  • %-20s [%s]  %s\n", l.Name, l.Schedule.CronExpr(), l.Description)
		}
		mods, _ := loopinstall.InstalledModules(root)
		fmt.Printf("\nInstalled modules (%d):\n", len(mods))
		for _, m := range mods {
			fmt.Printf("  - %s\n", m)
		}
		return
	}
	if _, err := tea.NewProgram(newLoopsModel(root)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		os.Exit(1)
	}
}

var (
	stAmber = lipgloss.NewStyle().Foreground(lipgloss.Color("#d9a441"))
	stMuted = lipgloss.NewStyle().Foreground(lipgloss.Color("#8a93a0"))
	stRed   = lipgloss.NewStyle().Foreground(lipgloss.Color("#e06c75"))
	stGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("#98c379"))
	stBold  = lipgloss.NewStyle().Bold(true)
)

type tuiScreen int

const (
	scrMenu tuiScreen = iota
	scrActive
	scrInstall
	scrRemove
	scrWorking
	scrResult
)

type opMsg struct {
	title string
	log   string
	err   error
}

type loopsModel struct {
	root      string
	screen    tuiScreen
	menu      []string
	cursor    int
	loops     []loopkit.Loop
	installed []string
	input     textinput.Model
	spin      spinner.Model
	working   string
	result    string
	resultErr bool
}

func newLoopsModel(root string) loopsModel {
	ti := textinput.New()
	ti.Placeholder = "github.com/you/karmax-cool-loop"
	ti.CharLimit = 240
	ti.Width = 52
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = stAmber
	m := loopsModel{
		root:  root,
		menu:  []string{"Active loops", "Install a loop", "Remove a loop", "Restart KARMAX", "Quit"},
		input: ti,
		spin:  sp,
	}
	m.reload()
	return m
}

func (m *loopsModel) reload() {
	m.loops = loopkit.Registered()
	mods, _ := loopinstall.InstalledModules(m.root)
	sort.Strings(mods)
	m.installed = mods
}

func (m loopsModel) Init() tea.Cmd { return m.spin.Tick }

func installCmd(root, module string) tea.Cmd {
	return func() tea.Msg {
		log, err := loopinstall.Install(root, module)
		return opMsg{title: "Install " + module, log: log, err: err}
	}
}

func removeCmd(root, module string) tea.Cmd {
	return func() tea.Msg {
		log, err := loopinstall.Remove(root, module)
		return opMsg{title: "Remove " + module, log: log, err: err}
	}
}

func restartCmd() tea.Cmd {
	return func() tea.Msg {
		out, err := loopinstall.Restart()
		return opMsg{title: "Restart KARMAX", log: out, err: err}
	}
}

func (m loopsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case opMsg:
		m.screen = scrResult
		if msg.err != nil {
			m.resultErr = true
			m.result = stRed.Render(msg.title+" failed") + "\n\n" + msg.err.Error() + "\n\n" + stMuted.Render(tail(msg.log, 1400))
		} else {
			m.resultErr = false
			m.reload()
			note := "Rebuilt successfully. Choose “Restart KARMAX” to load it."
			if msg.title == "Restart KARMAX" {
				note = "KARMAX restarted — your loops are live."
			}
			m.result = stGreen.Render(msg.title+" ✓") + "\n\n" + stMuted.Render(tail(msg.log, 700)) + "\n\n" + note
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	if m.screen == scrInstall {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m loopsModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.screen {
	case scrMenu:
		switch key {
		case "q":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.menu)-1 {
				m.cursor++
			}
		case "enter":
			switch m.menu[m.cursor] {
			case "Active loops":
				m.screen = scrActive
			case "Install a loop":
				m.input.SetValue("")
				m.input.Focus()
				m.screen = scrInstall
				return m, textinput.Blink
			case "Remove a loop":
				m.cursor = 0
				m.screen = scrRemove
			case "Restart KARMAX":
				m.working = "restarting KARMAX…"
				m.screen = scrWorking
				return m, tea.Batch(m.spin.Tick, restartCmd())
			case "Quit":
				return m, tea.Quit
			}
		}
	case scrActive:
		if key == "q" || key == "esc" || key == "enter" {
			m.screen = scrMenu
		}
	case scrInstall:
		switch key {
		case "esc":
			m.input.Blur()
			m.screen = scrMenu
			return m, nil
		case "enter":
			mod := strings.TrimSpace(m.input.Value())
			m.input.Blur()
			if mod == "" {
				m.screen = scrMenu
				return m, nil
			}
			m.working = "installing " + mod + " — go get + rebuild (~30s)…"
			m.screen = scrWorking
			return m, tea.Batch(m.spin.Tick, installCmd(m.root, mod))
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(k)
			return m, cmd
		}
	case scrRemove:
		switch key {
		case "q", "esc":
			m.screen = scrMenu
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.installed)-1 {
				m.cursor++
			}
		case "enter":
			if len(m.installed) == 0 {
				m.screen = scrMenu
				return m, nil
			}
			mod := m.installed[m.cursor]
			m.working = "removing " + mod + " — rebuild…"
			m.screen = scrWorking
			return m, tea.Batch(m.spin.Tick, removeCmd(m.root, mod))
		}
	case scrResult:
		if key == "enter" || key == "esc" || key == "q" {
			m.cursor = 0
			m.screen = scrMenu
		}
	}
	return m, nil
}

func (m loopsModel) View() string {
	var b strings.Builder
	b.WriteString(stAmber.Render("KARMAX · loops") + stMuted.Render("   (compile-time plugins)") + "\n\n")

	switch m.screen {
	case scrMenu:
		b.WriteString(stMuted.Render(fmt.Sprintf("%d active · %d installed module(s)\n\n", len(m.loops), len(m.installed))))
		for i, item := range m.menu {
			cursor := "  "
			line := item
			if i == m.cursor {
				cursor = stAmber.Render("› ")
				line = stBold.Render(stAmber.Render(item))
			}
			b.WriteString(cursor + line + "\n")
		}
		b.WriteString("\n" + stMuted.Render("↑/↓ move · enter select · q quit"))

	case scrActive:
		b.WriteString(stBold.Render("Active loops") + "\n\n")
		if len(m.loops) == 0 {
			b.WriteString(stMuted.Render("none registered yet.\n"))
		}
		for _, l := range m.loops {
			b.WriteString(stAmber.Render("• "+l.Name) + stMuted.Render("  ["+l.Schedule.CronExpr()+"]") + "\n")
			if l.Description != "" {
				b.WriteString(stMuted.Render("    "+l.Description) + "\n")
			}
		}
		b.WriteString("\n" + stMuted.Render("enter/esc back"))

	case scrInstall:
		b.WriteString(stBold.Render("Install a loop") + "\n\n")
		b.WriteString(stMuted.Render("Enter a Go module path (the loop author publishes it):") + "\n\n")
		b.WriteString(m.input.View() + "\n\n")
		b.WriteString(stMuted.Render("enter install · esc cancel"))

	case scrRemove:
		b.WriteString(stBold.Render("Remove a loop") + "\n\n")
		if len(m.installed) == 0 {
			b.WriteString(stMuted.Render("no installed loop modules.\n\n") + stMuted.Render("esc back"))
			break
		}
		for i, mod := range m.installed {
			cursor := "  "
			line := mod
			if i == m.cursor {
				cursor = stRed.Render("› ")
				line = stBold.Render(mod)
			}
			b.WriteString(cursor + line + "\n")
		}
		b.WriteString("\n" + stMuted.Render("↑/↓ move · enter remove · esc back"))

	case scrWorking:
		b.WriteString(m.spin.View() + " " + m.working + "\n")

	case scrResult:
		b.WriteString(m.result + "\n\n")
		b.WriteString(stMuted.Render("enter/esc back to menu"))
	}
	b.WriteString("\n")
	return b.String()
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
