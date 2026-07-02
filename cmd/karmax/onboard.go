package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

// newOnboardCmd is the cool, guided first-run wizard.
func newOnboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "onboard",
		Short:         "Guided first-run setup wizard",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			_, err := tea.NewProgram(newOnboardModel()).Run()
			return err
		},
	}
}

var karmaxLogo = []string{
	"██╗  ██╗ █████╗ ██████╗ ███╗   ███╗ █████╗ ██╗  ██╗",
	"██║ ██╔╝██╔══██╗██╔══██╗████╗ ████║██╔══██╗╚██╗██╔╝",
	"█████╔╝ ███████║██████╔╝██╔████╔██║███████║ ╚███╔╝ ",
	"██╔═██╗ ██╔══██║██╔══██╗██║╚██╔╝██║██╔══██║ ██╔██╗ ",
	"██║  ██╗██║  ██║██║  ██║██║ ╚═╝ ██║██║  ██║██╔╝ ██╗",
	"╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝     ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝",
}

type obStep int

const (
	obWelcome obStep = iota
	obWorkspace
	obEnv
	obToken
	obDone
)

var obTitles = map[obStep]string{
	obWorkspace: "Workspace",
	obEnv:       "Environment",
	obToken:     "Access token",
}

type envCheck struct {
	label string
	hint  string
	ok    bool
}

type wsDoneMsg struct{ lines []string }
type envDoneMsg struct{ checks []envCheck }
type tokenMsg struct {
	token string
	err   error
}

type obModel struct {
	step       obStep
	spin       spinner.Model
	busy       bool
	wsLines    []string
	checks     []envCheck
	token      string
	tokenSaved bool
	tokenErr   error
}

func newOnboardModel() obModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = stAmber
	return obModel{spin: sp, token: existingToken()}
}

func (m obModel) Init() tea.Cmd { return m.spin.Tick }

func (m obModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)
	case wsDoneMsg:
		m.busy = false
		m.wsLines = msg.lines
		return m, nil
	case envDoneMsg:
		m.busy = false
		m.checks = msg.checks
		return m, nil
	case tokenMsg:
		m.busy = false
		m.token, m.tokenErr = msg.token, msg.err
		m.tokenSaved = msg.err == nil
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m obModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "g":
		if m.step == obToken && !m.busy {
			m.busy = true
			return m, tea.Batch(m.spin.Tick, genTokenCmd())
		}
	case "enter":
		switch m.step {
		case obWelcome:
			m.step = obWorkspace
			m.busy = true
			return m, tea.Batch(m.spin.Tick, workspaceCmd())
		case obWorkspace:
			if m.busy {
				return m, nil
			}
			m.step = obEnv
			m.busy = true
			return m, tea.Batch(m.spin.Tick, envCmd())
		case obEnv:
			if m.busy {
				return m, nil
			}
			m.step = obToken
			return m, nil
		case obToken:
			if m.busy {
				return m, nil
			}
			m.step = obDone
			return m, nil
		case obDone:
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m obModel) View() string {
	var b strings.Builder
	if m.step == obWelcome {
		b.WriteString("\n")
		for _, l := range karmaxLogo {
			b.WriteString("  " + stAmber.Render(l) + "\n")
		}
		b.WriteString("\n  " + stBold.Render("Welcome to KARMAX") + stMuted.Render(" — your always-on personal AI agent.\n"))
		b.WriteString(stMuted.Render("  This wizard sets up your workspace, checks your environment, and gets you running.\n\n"))
		b.WriteString("  " + stAmber.Render("press enter to begin") + stMuted.Render("   ·   q to quit") + "\n")
		return b.String()
	}

	// header + progress dots for the working steps
	b.WriteString("\n  " + stAmber.Render("KARMAX setup") + stMuted.Render("  ·  "+obTitles[m.step]) + "\n  ")
	for _, s := range []obStep{obWorkspace, obEnv, obToken} {
		if s == m.step {
			b.WriteString(stAmber.Render("● "))
		} else if s < m.step {
			b.WriteString(stGreen.Render("● "))
		} else {
			b.WriteString(stMuted.Render("○ "))
		}
	}
	b.WriteString("\n\n")

	switch m.step {
	case obWorkspace:
		if m.busy {
			b.WriteString("  " + m.spin.View() + " preparing ~/.karmax …\n")
			break
		}
		for _, l := range m.wsLines {
			b.WriteString("  " + stGreen.Render("✓ ") + l + "\n")
		}
		b.WriteString("\n  " + stMuted.Render("enter to continue"))

	case obEnv:
		if m.busy {
			b.WriteString("  " + m.spin.View() + " checking your environment …\n")
			break
		}
		for _, c := range m.checks {
			if c.ok {
				b.WriteString("  " + stGreen.Render("✓ ") + c.label + "\n")
			} else {
				b.WriteString("  " + stRed.Render("✗ ") + c.label + stMuted.Render("  — "+c.hint) + "\n")
			}
		}
		b.WriteString("\n  " + stMuted.Render("missing items are optional — fix them anytime. enter to continue"))

	case obToken:
		b.WriteString("  " + stMuted.Render("The phone app authenticates to KARMAX with an access token.\n\n"))
		if m.busy {
			b.WriteString("  " + m.spin.View() + " generating …\n")
			break
		}
		if m.token != "" {
			label := "current token"
			if m.tokenSaved {
				label = stGreen.Render("✓ ") + "saved to ~/.karmax/.env"
			}
			b.WriteString("  " + label + ":\n  " + stAmber.Render(m.token) + "\n")
		} else {
			b.WriteString("  " + stMuted.Render("no token set (API auth is open).") + "\n")
		}
		if m.tokenErr != nil {
			b.WriteString("  " + stRed.Render("! "+m.tokenErr.Error()) + "\n")
		}
		b.WriteString("\n  " + stAmber.Render("g") + stMuted.Render(" generate & save a new token   ·   enter to continue"))

	case obDone:
		b.WriteString("  " + stGreen.Render("✓ You're all set.") + "\n\n")
		b.WriteString(stMuted.Render("  Next:\n"))
		b.WriteString("    " + stAmber.Render("karmax start") + stMuted.Render("   run the agent + phone API (or it's already running via systemd)") + "\n")
		b.WriteString("    " + stAmber.Render("karmax loops") + stMuted.Render("   install/manage scheduled loops") + "\n")
		b.WriteString("    " + stAmber.Render("karmax doctor") + stMuted.Render("  re-check your environment") + "\n")
		b.WriteString("\n  Connect the phone app to " + stAmber.Render("http://<this-host>:9091") + ".\n")
		b.WriteString("\n  " + stMuted.Render("enter to finish"))
	}
	return b.String()
}

// ---- step work ----

func workspaceCmd() tea.Cmd {
	return func() tea.Msg {
		home, _ := os.UserHomeDir()
		dataDir := filepath.Join(home, ".karmax")
		var lines []string
		for _, d := range []string{dataDir, filepath.Join(dataDir, "memory"), filepath.Join(dataDir, "db")} {
			os.MkdirAll(d, 0755)
		}
		lines = append(lines, "workspace ready: "+dataDir)

		cfgFile := filepath.Join(dataDir, "karmax.yaml")
		if _, err := os.Stat(cfgFile); err != nil {
			if err := config.SaveDefault(cfgFile); err == nil {
				lines = append(lines, "created default config: "+cfgFile)
			}
		} else {
			lines = append(lines, "config already present: "+cfgFile)
		}
		return wsDoneMsg{lines: lines}
	}
}

func envCmd() tea.Cmd {
	return func() tea.Msg {
		home, _ := os.UserHomeDir()
		checks := []envCheck{
			{"Go compiler", "run `karmax setup`", loopinstall.HaveGo()},
			{"git", "install git", loopinstall.HaveGit()},
			{"C compiler (cgo / SQLite)", "apt install build-essential", commandOnPath("gcc") || commandOnPath("cc")},
			{"Codex auth (~/.codex/auth.json)", "run the codex login", fileExists(filepath.Join(home, ".codex", "auth.json"))},
			{"Claude Code CLI", "install Claude Code", commandOnPath("claude")},
			{"WhatsApp (wacli)", "run `wacli login`", wacliConnected()},
		}
		return envDoneMsg{checks: checks}
	}
}

func genTokenCmd() tea.Cmd {
	return func() tea.Msg {
		buf := make([]byte, 24)
		if _, err := rand.Read(buf); err != nil {
			return tokenMsg{err: err}
		}
		tok := "kmx_" + hex.EncodeToString(buf)
		if err := saveEnvVar("KARMAX_API_TOKEN", tok); err != nil {
			return tokenMsg{token: tok, err: err}
		}
		return tokenMsg{token: tok}
	}
}

func existingToken() string { return strings.TrimSpace(os.Getenv("KARMAX_API_TOKEN")) }

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func wacliConnected() bool {
	bin := hostpaths.Wacli()
	if !commandOnPath(bin) && !fileExists(bin) {
		return false
	}
	out, _ := exec.Command(bin, "status").Output()
	return strings.Contains(string(out), "connected: true")
}

// saveEnvVar upserts KEY=value in ~/.karmax/.env.
func saveEnvVar(key, value string) error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".karmax", ".env")
	os.MkdirAll(filepath.Dir(path), 0755)

	var kept []string
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
				continue
			}
			kept = append(kept, line)
		}
	}
	// drop a trailing empty line to avoid blank-line growth
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	kept = append(kept, key+"="+value, "")
	os.Setenv(key, value)
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0600)
}
