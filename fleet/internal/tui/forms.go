package tui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Small pieces the Fleet and Tasks tabs share: a yes/no question, a one-line
// input, and a client call that ends in a toast.

// callTimeout bounds one action's call to fleetd.
const callTimeout = 30 * time.Second

// call runs fn against fleetd off the UI loop and toasts its result: ok on
// success, the error otherwise.
func call(ok string, fn func(ctx context.Context) (string, error)) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		said, err := fn(ctx)
		if err != nil {
			return ToastMsg{Text: err.Error(), Err: true}
		}
		if said != "" {
			ok = said
		}
		return ToastMsg{Text: ok}
	}
}

// confirm is a yes/no question shown at the foot of a tab: y runs act, any
// other key drops it.
type confirm struct {
	ask string
	act tea.Cmd
}

// update answers the question with k; the confirm is done either way.
func (c *confirm) update(k string) tea.Cmd {
	if k == "y" || k == "Y" {
		return c.act
	}
	return nil
}

func (c *confirm) view(t *Theme, w int) string {
	return Fit(lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render("▌ ")+lipgloss.NewStyle().Foreground(t.Text).Render(c.ask)+"  "+
		hint(t, key.NewBinding(key.WithHelp("y", "yes")))+t.Muted.Render(" · ")+hint(t, key.NewBinding(key.WithHelp("n", "no"))), w)
}

// lineInput is a one-line field at the foot of a tab, sent with enter.
type lineInput struct {
	label string
	in    textinput.Model
	send  func(text string) tea.Cmd
}

func newLineInput(label string, send func(string) tea.Cmd) *lineInput {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "type, then enter · esc drops it"
	in.Focus()
	return &lineInput{label: label, in: in, send: send}
}

// update takes a key; done says the input is finished (sent or dropped).
func (l *lineInput) update(msg tea.KeyPressMsg) (cmd tea.Cmd, done bool) {
	switch msg.String() {
	case "esc":
		return nil, true
	case "enter":
		if l.in.Value() == "" {
			return nil, true
		}
		return l.send(l.in.Value()), true
	}
	l.in, cmd = l.in.Update(msg)
	return cmd, false
}

func (l *lineInput) view(t *Theme, w int) string {
	label := lipgloss.NewStyle().Foreground(t.Amber).Bold(true).Render("▌ "+l.label) + t.Muted.Render(" ▸ ")
	l.in.SetWidth(max(1, w-lipgloss.Width(label)-1))
	return Fit(label+l.in.View(), w)
}

// keyIs builds a help binding.
func keyIs(k, desc string) key.Binding {
	return key.NewBinding(key.WithKeys(k), key.WithHelp(k, desc))
}

// scrollTo is the first line to show of n so that line sel is in a window
// of h lines, moving off as little as possible.
func scrollTo(off, sel, n, h int) int {
	if h <= 0 {
		return 0
	}
	if sel < off {
		off = sel
	}
	if sel >= off+h {
		off = sel - h + 1
	}
	return max(0, min(off, n-h))
}
