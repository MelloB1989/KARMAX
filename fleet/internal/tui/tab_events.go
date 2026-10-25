package tui

import (
	"encoding/json"
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// eventsTab is the fleet's event stream, newest first, colored by kind.
// / filters it (by agent, kind or task), space pauses it.
type eventsTab struct {
	paused    []fleetapi.Event // the stream as it was when paused; nil: live
	isPaused  bool
	filtering bool
	filter    textinput.Model
	sel, off  int
}

func newEvents() Tab {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "agent-04  kind:fleet.alert  task:20261025…"
	return &eventsTab{filter: in}
}

func (e *eventsTab) Title() string            { return "Events" }
func (e *eventsTab) Visible(fleetapi.Me) bool { return true }
func (e *eventsTab) Capturing() bool          { return e.filtering }

func (e *eventsTab) Help() []key.Binding {
	pause := keyIs("space", "pause")
	if e.isPaused {
		pause = keyIs("space", "resume")
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "scroll")),
		keyIs("/", "filter"), pause,
	}
}

// events is the stream shown: paused or live, filtered.
func (e *eventsTab) events(c *Ctx) []fleetapi.Event {
	src := e.paused
	if !e.isPaused && c.Snap != nil {
		src = c.Snap.Events
	}
	q := strings.Fields(e.filter.Value())
	if len(q) == 0 {
		return src
	}
	var out []fleetapi.Event
	for _, ev := range src {
		if matches(ev, q) {
			out = append(out, ev)
		}
	}
	return out
}

// matches says whether every term matches the event: agent:, kind: and
// task: name a field (kind by prefix); a bare term matches any of them.
func matches(ev fleetapi.Event, terms []string) bool {
	task := eventTask(ev)
	for _, q := range terms {
		k, v, named := strings.Cut(q, ":")
		if !named || (k != "agent" && k != "kind" && k != "task") {
			k, v = "", q
		}
		v = strings.ToLower(v)
		agent, kind := strings.ToLower(ev.Agent), strings.ToLower(ev.Kind)
		ok := false
		switch k {
		case "agent":
			ok = agent == v || strings.HasPrefix(agent, v+":")
		case "kind":
			ok = strings.HasPrefix(kind, v)
		case "task":
			ok = strings.Contains(strings.ToLower(task), v)
		default:
			ok = agent == v || strings.HasPrefix(agent, v+":") || strings.HasPrefix(kind, v) || (task != "" && strings.Contains(strings.ToLower(task), v))
		}
		if !ok {
			return false
		}
	}
	return true
}

func (e *eventsTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	m, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	k := m.String()
	if e.filtering {
		switch k {
		case "enter":
			e.filtering = false
			e.filter.Blur()
		case "esc":
			e.filtering = false
			e.filter.Blur()
			e.filter.SetValue("")
		default:
			var cmd tea.Cmd
			e.filter, cmd = e.filter.Update(m)
			e.sel, e.off = 0, 0
			return cmd
		}
		return nil
	}
	n := len(e.events(c))
	switch k {
	case "/":
		e.filtering = true
		return e.filter.Focus()
	case "esc":
		e.filter.SetValue("")
	case "space":
		e.isPaused = !e.isPaused
		if e.isPaused && c.Snap != nil {
			e.paused = slices.Clone(c.Snap.Events)
		} else {
			e.paused = nil
		}
	case "up", "k":
		e.sel = max(0, e.sel-1)
	case "down", "j":
		e.sel = max(0, min(n-1, e.sel+1))
	case "pgup":
		e.sel = max(0, e.sel-10)
	case "pgdown":
		e.sel = max(0, min(n-1, e.sel+10))
	case "home", "g":
		e.sel = 0
	case "end", "G":
		e.sel = max(0, n-1)
	}
	return nil
}

func (e *eventsTab) View(c *Ctx, w, h int) string {
	t := c.Theme
	if c.Snap == nil || w <= 0 || h <= 0 {
		return ""
	}
	evs := e.events(c)
	e.sel = max(0, min(e.sel, len(evs)-1))

	// The status line: live or paused, how many, the filter.
	state := lipgloss.NewStyle().Foreground(t.Green).Render("● live")
	if e.isPaused {
		state = lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render("❚❚ paused") + t.Muted.Render(" · space resumes")
	}
	status := state + t.Muted.Render(fmt.Sprintf(" · %d events", len(evs)))
	switch {
	case e.filtering:
		e.filter.SetWidth(max(1, w/2))
		status += t.Muted.Render("   filter ▸ ") + e.filter.View()
	case e.filter.Value() != "":
		status += t.Muted.Render("   filter ") + lipgloss.NewStyle().Foreground(t.Cyan).Render(e.filter.Value()) + t.Muted.Render("  (esc clears)")
	}
	lines := []string{Fit(status, w)}
	rows := h - 1
	if rows <= 0 {
		return block(strings.Join(lines, "\n"), w, h)
	}
	if len(evs) == 0 {
		lines = append(lines, "", t.Muted.Render("  no events match"))
		return block(strings.Join(lines, "\n"), w, h)
	}
	e.off = scrollTo(e.off, e.sel, len(evs), rows)
	agentW, kindW := 11, 20
	if w < 70 {
		agentW, kindW = 10, 14
	}
	for i := e.off; i < min(len(evs), e.off+rows); i++ {
		ev := evs[i]
		mark := " "
		if i == e.sel {
			mark = lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render("▸")
		}
		agent := ev.Agent
		if agent == "" {
			agent = "fleetd"
		}
		row := mark + t.Muted.Render(ev.At.Format("15:04:05")) + " " +
			lipgloss.NewStyle().Foreground(t.Text).Render(Fit(agent, agentW)) + " " +
			paint(kindColor(t, ev.Kind), Fit(ev.Kind, kindW)) + " "
		if w >= 110 {
			row += t.Muted.Render(Fit(eventTask(ev), 26)) + " "
		}
		row += lipgloss.NewStyle().Foreground(t.Text).Render(eventText(ev))
		lines = append(lines, Fit(row, w))
	}
	return block(strings.Join(lines, "\n"), w, h)
}

// kindColor is an event kind's color, by its prefix.
func kindColor(t *Theme, kind string) color.Color {
	switch {
	case strings.HasPrefix(kind, "fleet.alert"):
		return t.Alert
	case strings.HasPrefix(kind, "fleet.question"):
		return t.Cyan
	case strings.HasPrefix(kind, "fleet.report"):
		return t.Amber
	case strings.HasPrefix(kind, "fleet.access"):
		return t.Violet
	case strings.HasPrefix(kind, "fleet.task"):
		return t.Green
	case strings.HasPrefix(kind, "otel"):
		return t.Grey
	}
	return t.Dim
}

// eventText is an event's detail in a line: its text, else its fields.
func eventText(e fleetapi.Event) string {
	var d map[string]any
	if json.Unmarshal(e.Detail, &d) != nil || len(d) == 0 {
		return oneLine(string(e.Detail))
	}
	for _, k := range []string{"text", "error", "reason"} {
		if s, ok := d[k].(string); ok && s != "" {
			return oneLine(s)
		}
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		if k != "task" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		switch v := d[k].(type) {
		case string:
			parts = append(parts, k+"="+oneLine(v))
		case float64, bool:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " ")
}
