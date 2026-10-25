package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// lendCaveat is said after every lend (spec section 2, Caveat).
const lendCaveat = "Whoever types into an agent can read its Claude login. Lend agents whose subscription you are happy to share, or the friend's own."

const (
	accessEvery   = 30 * time.Second // the list is fetched again when shown and this old
	accessTimeout = 10 * time.Second
)

// access is the Access tab (admins): tagged devices with their roles and
// lends, then the untagged devices that knocked. Every change is
// Client.ChangeAccess, the route `fleetctl access` uses.
type access struct {
	list     *fleetapi.AccessList
	err      string
	flying   bool
	sentAt   time.Time
	gotAt    time.Time
	knocks   int // the snapshot's knock count the list was fetched for
	sel      int
	off      int
	note     string // the caveat, after a lend
	pick     *accessPick
	confirm  *fleetapi.AccessChange
	confirmQ string
}

// accessPick is the lend and unlend picker: one agent for one device.
type accessPick struct {
	op      string
	device  string // the StableID
	name    string
	choices []string
	i       int
}

// accessRow is one line of the list: a tagged device or a knock.
type accessRow struct {
	id, name, login string
	dev             *fleetapi.Device
	knock           *fleetapi.Knock
}

type accessMsg struct {
	list *fleetapi.AccessList
	err  error
	done string // the change made, for the toast; "" for a plain fetch
	lent bool
}

func newAccess() Tab { return &access{} }

func (a *access) Title() string               { return "Access" }
func (a *access) Visible(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }
func (a *access) Capturing() bool             { return a.pick != nil || a.confirm != nil }

var accessKeys = struct{ Move, Admin, Lend, Unlend, Revoke, Refresh key.Binding }{
	Move:    key.NewBinding(key.WithKeys("up", "down", "k", "j"), key.WithHelp("↑↓", "select")),
	Admin:   key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "admin")),
	Lend:    key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "lend")),
	Unlend:  key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unlend")),
	Revoke:  key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "revoke")),
	Refresh: key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
}

func (a *access) Help() []key.Binding {
	k := accessKeys
	if a.Capturing() {
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter", "y"), key.WithHelp("⏎/y", "yes")),
			key.NewBinding(key.WithKeys("esc", "n"), key.WithHelp("esc/n", "no")),
		}
	}
	return []key.Binding{k.Move, k.Admin, k.Lend, k.Unlend, k.Revoke, k.Refresh}
}

func (a *access) rows() []accessRow {
	if a.list == nil {
		return nil
	}
	var out []accessRow
	for i := range a.list.Devices {
		d := &a.list.Devices[i]
		out = append(out, accessRow{id: d.StableID, name: d.Name, login: d.Login, dev: d})
	}
	for i := range a.list.Knocks {
		k := &a.list.Knocks[i]
		out = append(out, accessRow{id: k.StableID, name: k.Name, login: k.Login, knock: k})
	}
	return out
}

// short is a tailnet name's machine part: friend-laptop.
func short(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}

func (a *access) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	var cmds []tea.Cmd
	switch m := msg.(type) {
	case accessMsg:
		a.flying = false
		if m.err != nil {
			a.err = m.err.Error()
			if m.done != "" {
				cmds = append(cmds, toastCmd(m.done+": "+a.err, true))
			}
			break
		}
		a.err, a.list, a.gotAt = "", m.list, c.Now
		if m.done != "" {
			cmds = append(cmds, toastCmd(m.done, false))
			a.note = ""
			if m.lent {
				a.note = lendCaveat
			}
		}
	case tea.KeyPressMsg:
		cmds = append(cmds, a.key(m.String(), c))
	}
	return tea.Batch(append(cmds, a.fetch(c))...)
}

func (a *access) key(k string, c *Ctx) tea.Cmd {
	switch {
	case a.confirm != nil:
		ch := *a.confirm
		switch k {
		case "y", "enter":
			a.confirm = nil
			return a.change(c, ch, a.confirmQ)
		case "n", "esc", "q":
			a.confirm = nil
		}
		return nil
	case a.pick != nil:
		p := a.pick
		switch k {
		case "up", "k":
			p.i = max(0, p.i-1)
		case "down", "j":
			p.i = min(len(p.choices)-1, p.i+1)
		case "enter":
			a.pick = nil
			ag := p.choices[p.i]
			verb := map[string]string{fleetapi.AccessLend: "lent %s to %s", fleetapi.AccessUnlend: "took %s back from %s"}[p.op]
			return a.change(c, fleetapi.AccessChange{Op: p.op, Device: p.device, Agents: []string{ag}}, fmt.Sprintf(verb, ag, p.name))
		case "esc", "q":
			a.pick = nil
		}
		return nil
	}
	rows := a.rows()
	switch k {
	case "up", "k":
		a.sel = max(0, a.sel-1)
		return nil
	case "down", "j":
		a.sel = min(max(0, len(rows)-1), a.sel+1)
		return nil
	case "R":
		a.gotAt = time.Time{}
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	a.sel = min(a.sel, len(rows)-1)
	r := rows[a.sel]
	name := short(r.name)
	switch k {
	case "a":
		if r.dev != nil && r.dev.Role == fleetapi.RoleAdmin {
			return toastCmd(name+" is already an admin", false)
		}
		a.confirm = &fleetapi.AccessChange{Op: fleetapi.AccessAdmin, Device: r.id}
		a.confirmQ = "made " + name + " an admin"
	case "x":
		if r.dev == nil {
			return toastCmd(name+" is not tagged; there is nothing to revoke", false)
		}
		a.confirm = &fleetapi.AccessChange{Op: fleetapi.AccessRevoke, Device: r.id}
		a.confirmQ = "revoked " + name
	case "l":
		if r.dev != nil && r.dev.Role == fleetapi.RoleAdmin {
			return toastCmd(name+" is an admin; agents are lent to guests", true)
		}
		var lent []string
		if r.dev != nil {
			lent = r.dev.Agents
		}
		choices := sandboxes(c.Snap, lent)
		if len(choices) == 0 {
			return toastCmd("no sandbox agent is left to lend", true)
		}
		a.pick = &accessPick{op: fleetapi.AccessLend, device: r.id, name: name, choices: choices}
	case "u":
		if r.dev == nil || len(r.dev.Agents) == 0 {
			return toastCmd(name+" has no agents lent", false)
		}
		a.pick = &accessPick{op: fleetapi.AccessUnlend, device: r.id, name: name, choices: slices.Clone(r.dev.Agents)}
	}
	return nil
}

// sandboxes is the sandbox agents in the snapshot, but those in except:
// the only agents that can be lent.
func sandboxes(s *fleetapi.Snapshot, except []string) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, w := range s.Workers {
		ag := w.SlotOf
		if ag == "" {
			ag = w.Agent
		}
		if w.Sandbox && !slices.Contains(out, ag) && !slices.Contains(except, ag) {
			out = append(out, ag)
		}
	}
	return out
}

func (a *access) change(c *Ctx, ch fleetapi.AccessChange, done string) tea.Cmd {
	if c.Client == nil {
		return nil
	}
	cl := c.Client
	a.flying, a.sentAt = true, c.Now
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), accessTimeout)
		defer cancel()
		l, err := cl.ChangeAccess(ctx, ch)
		return accessMsg{list: l, err: err, done: done, lent: ch.Op == fleetapi.AccessLend}
	}
}

// fetch gets the list when there is none, when it is old, or when the
// snapshot counts a different number of knocks.
func (a *access) fetch(c *Ctx) tea.Cmd {
	if c.Client == nil || a.flying && c.Now.Sub(a.sentAt) < accessTimeout {
		return nil
	}
	knocks := -1
	if c.Snap != nil {
		knocks = c.Snap.Knocks
	}
	if a.list != nil && knocks == a.knocks && c.Now.Sub(a.gotAt) < accessEvery && !a.gotAt.IsZero() {
		return nil
	}
	if a.err != "" && c.Now.Sub(a.sentAt) < accessEvery {
		return nil // a failed fetch waits as long as a good one
	}
	a.flying, a.sentAt, a.knocks = true, c.Now, knocks
	cl := c.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), accessTimeout)
		defer cancel()
		l, err := cl.Access(ctx)
		return accessMsg{list: l, err: err}
	}
}

func (a *access) View(c *Ctx, w, h int) string {
	t := c.Theme
	if a.list == nil {
		if a.err != "" {
			return centerNote(t, a.err, w, h)
		}
		return centerNote(t, "asking fleetd who may use the fleet…", w, h)
	}
	now := c.Now
	rows := a.rows()
	a.sel = min(a.sel, max(0, len(rows)-1))
	mark := func(i int, s string) string {
		if i == a.sel && a.pick == nil && a.confirm == nil {
			return t.Selected.Render("▸ ") + s
		}
		if i == a.sel {
			return t.Selected.Render("▹ ") + s
		}
		return "  " + s
	}
	devCols := []tableCol[fleetapi.Device]{
		{head: "DEVICE", w: 18, grow: true, most: 34, cell: func(d fleetapi.Device) string { return t.Strong.Render(d.Name) }},
		{head: "LOGIN", w: 20, drop: 2, cell: func(d fleetapi.Device) string { return d.Login }},
		{head: "ROLE", w: 5, cell: func(d fleetapi.Device) string {
			if d.Role == fleetapi.RoleAdmin {
				return lipgloss.NewStyle().Foreground(t.Violet).Bold(true).Render("admin")
			}
			return lipgloss.NewStyle().Foreground(t.Cyan).Render(string(d.Role))
		}},
		{head: "AGENTS", w: 17, cell: func(d fleetapi.Device) string {
			if d.Role == fleetapi.RoleAdmin {
				return t.Muted.Render("all")
			}
			if len(d.Agents) == 0 {
				return t.Muted.Render("none")
			}
			return strings.Join(d.Agents, ",")
		}},
		{head: "SEEN", w: 6, drop: 1, cell: func(d fleetapi.Device) string { return Since(now, d.LastSeen) }},
	}
	knockCols := []tableCol[fleetapi.Knock]{
		{head: "DEVICE", w: 18, grow: true, most: 34, cell: func(k fleetapi.Knock) string { return k.Name }},
		{head: "LOGIN", w: 20, drop: 2, cell: func(k fleetapi.Knock) string { return k.Login }},
		{head: "ASKED", w: 23, cell: func(k fleetapi.Knock) string {
			return fmt.Sprintf("%d× · last %s ago", k.Count, Since(now, k.Last))
		}},
	}
	in := max(0, w-2)
	dh, dl := tableLayout(t, devCols, a.list.Devices, in)
	var lines []string
	lines = append(lines, t.Title.Render("Devices")+t.Muted.Render(fmt.Sprintf("  %d tagged", len(a.list.Devices))), "  "+dh)
	if len(dl) == 0 {
		lines = append(lines, t.Muted.Render("  none yet: tag one with a or l on a knock"))
	}
	for i, l := range dl {
		lines = append(lines, mark(i, l))
	}
	lines = append(lines, "")
	kt := t.Title.Render("Knocks")
	if n := len(a.list.Knocks); n > 0 {
		kt += " " + t.Badge.Render(fmt.Sprintf("%d new", n))
	} else {
		kt += t.Muted.Render("  none")
	}
	lines = append(lines, kt)
	if len(a.list.Knocks) > 0 {
		kh, kl := tableLayout(t, knockCols, a.list.Knocks, in)
		lines = append(lines, "  "+kh)
		for i, l := range kl {
			lines = append(lines, mark(len(dl)+i, l))
		}
	}
	// What sits below the list: an open question, or the last word.
	var foot []string
	switch {
	case a.confirm != nil:
		q := map[string]string{
			fleetapi.AccessAdmin:  "Make %s an admin? It may do everything the operator can.",
			fleetapi.AccessRevoke: "Revoke %s? It loses its tag and every lend at once.",
		}[a.confirm.Op]
		foot = []string{"", t.Danger.Render(fmt.Sprintf(q, short(rows[a.sel].name))) + t.Muted.Render("  y / n")}
	case a.pick != nil:
		verb := map[string]string{fleetapi.AccessLend: "Lend to %s:", fleetapi.AccessUnlend: "Take back from %s:"}[a.pick.op]
		foot = []string{"", t.Title.Render(fmt.Sprintf(verb, a.pick.name)) + t.Muted.Render("  ⏎ choose · esc cancel")}
		for i, ch := range a.pick.choices {
			if i == a.pick.i {
				foot = append(foot, t.Selected.Render("  ▸ "+ch))
			} else {
				foot = append(foot, "    "+ch)
			}
		}
	case a.err != "":
		foot = []string{"", t.Danger.Render(a.err)}
	case a.note != "":
		foot = []string{"", lipgloss.NewStyle().Foreground(t.Amber).Render("! " + a.note)}
	}
	room := max(0, h-len(foot))
	selLine := a.sel + 2 // the heading and the column names
	if a.sel >= len(dl) {
		selLine = a.sel + 5 + max(0, 1-len(dl))
	}
	a.off = keepInView(a.off, selLine, len(lines), room)
	lines = lines[min(a.off, len(lines)):]
	if len(lines) > room {
		lines = lines[:room]
	}
	return fillBody(append(lines, foot...), w, h)
}
