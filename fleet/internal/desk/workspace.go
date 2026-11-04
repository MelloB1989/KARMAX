package desk

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// ErrNotEmpty is a workspace removed while sessions are in it.
var ErrNotEmpty = errors.New("workspace not empty")

// workspace is the index of the workspace with id, or -1.
func (m *Manager) workspace(id string) int {
	return slices.IndexFunc(m.st.Workspaces, func(w workspace) bool { return w.ID == id })
}

// endOf is where a session put at the end of workspace ws goes in the
// order: after its last one, or, when it has none, at the end.
func (m *Manager) endOf(ws string) int {
	for i := len(m.st.Sessions) - 1; i >= 0; i-- {
		if m.st.Sessions[i].Workspace == ws {
			return i + 1
		}
	}
	return len(m.st.Sessions)
}

func (m *Manager) workspaceViews() []fleetapi.DeskWorkspace {
	out := make([]fleetapi.DeskWorkspace, len(m.st.Workspaces))
	for i, w := range m.st.Workspaces {
		out[i] = fleetapi.DeskWorkspace{ID: w.ID, Name: w.Name, Layout: w.Layout, Order: i, Created: w.Created}
	}
	return out
}

// cleanLabel is a name the operator gave: one line, trimmed, at most 80
// runes.
func cleanLabel(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsControl(r) }), " ")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		s = strings.TrimSpace(string(r[:80]))
	}
	return s
}

// cleanTitle is a pane's title without what is not its name: the spinner
// or mark Claude Code puts in front, and tmux's default, the host's name.
func cleanTitle(t, host string) string {
	t = strings.TrimLeftFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	t = cleanLabel(t)
	short, _, _ := strings.Cut(host, ".")
	if t == host || t == short {
		return ""
	}
	return t
}

// claudeStatus is a Claude Code session file's status as the desk says it.
func claudeStatus(s string) string {
	switch s {
	case "busy":
		return fleetapi.StatusWorking
	case "waiting":
		return fleetapi.StatusWaiting
	}
	return fleetapi.StatusIdle
}

// Update changes a session's label or workspace, or moves it (DeskUpdate).
func (m *Manager) Update(ctx context.Context, name string, u fleetapi.DeskUpdate) (fleetapi.DeskSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadAll()
	i := m.find(name)
	if i < 0 {
		return fleetapi.DeskSession{}, fmt.Errorf("%w %q", ErrNoSession, name)
	}
	r := m.st.Sessions[i]
	if u.Label != nil {
		r.Label = cleanLabel(*u.Label)
	}
	moved := false
	if u.Workspace != nil && *u.Workspace != r.Workspace {
		if m.workspace(*u.Workspace) < 0 {
			return fleetapi.DeskSession{}, invalidf("no workspace %q", *u.Workspace)
		}
		r.Workspace, moved = *u.Workspace, true
	}
	if u.Before != nil || moved {
		before := ""
		if u.Before != nil {
			before = *u.Before
		}
		if before == name {
			return fleetapi.DeskSession{}, invalidf("a session cannot go in front of itself")
		}
		if before != "" {
			if j := m.find(before); j < 0 || m.st.Sessions[j].Workspace != r.Workspace {
				return fleetapi.DeskSession{}, invalidf("no session %q in its workspace to go in front of", before)
			}
		}
		m.st.Sessions = slices.Delete(m.st.Sessions, i, i+1)
		at := m.endOf(r.Workspace)
		if before != "" {
			at = m.find(before)
		}
		m.st.Sessions = slices.Insert(m.st.Sessions, at, r)
		m.renumber()
	} else {
		m.st.Sessions[i] = r
	}
	m.save()
	m.touch()
	return m.st.Sessions[m.find(name)].view(), nil
}

// CreateWorkspace adds a workspace at the end.
func (m *Manager) CreateWorkspace(ctx context.Context, c fleetapi.DeskWorkspaceCreate) (fleetapi.DeskWorkspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadAll()
	layout := c.Layout
	if layout == "" {
		layout = fleetapi.Layouts[0]
	}
	if !slices.Contains(fleetapi.Layouts, layout) {
		return fleetapi.DeskWorkspace{}, invalidf("layout %q: want one of %s", layout, strings.Join(fleetapi.Layouts, ", "))
	}
	name := cleanLabel(c.Name)
	if name == "" {
		name = fmt.Sprintf("Workspace %d", len(m.st.Workspaces)+1)
	}
	id := "w" + randHex(4)
	for m.workspace(id) >= 0 {
		id = "w" + randHex(4)
	}
	m.st.Workspaces = append(m.st.Workspaces, workspace{ID: id, Name: name, Layout: layout, Created: m.Now()})
	m.save()
	m.touch()
	m.logf("workspace %s (%s) made", name, id)
	return m.workspaceViews()[len(m.st.Workspaces)-1], nil
}

// UpdateWorkspace renames a workspace, changes its layout, or moves it.
func (m *Manager) UpdateWorkspace(ctx context.Context, id string, u fleetapi.DeskWorkspaceUpdate) (fleetapi.DeskWorkspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadAll()
	i := m.workspace(id)
	if i < 0 {
		return fleetapi.DeskWorkspace{}, missingf("no workspace %q", id)
	}
	w := m.st.Workspaces[i]
	if u.Name != nil {
		if n := cleanLabel(*u.Name); n != "" {
			w.Name = n
		} else {
			return fleetapi.DeskWorkspace{}, invalidf("a workspace needs a name")
		}
	}
	if u.Layout != nil {
		if !slices.Contains(fleetapi.Layouts, *u.Layout) {
			return fleetapi.DeskWorkspace{}, invalidf("layout %q: want one of %s", *u.Layout, strings.Join(fleetapi.Layouts, ", "))
		}
		w.Layout = *u.Layout
	}
	m.st.Workspaces[i] = w
	if u.Before != nil {
		if *u.Before == id {
			return fleetapi.DeskWorkspace{}, invalidf("a workspace cannot go in front of itself")
		}
		rest := slices.Delete(slices.Clone(m.st.Workspaces), i, i+1)
		at := len(rest)
		if *u.Before != "" {
			at = slices.IndexFunc(rest, func(x workspace) bool { return x.ID == *u.Before })
			if at < 0 {
				return fleetapi.DeskWorkspace{}, invalidf("no workspace %q to go in front of", *u.Before)
			}
		}
		m.st.Workspaces = slices.Insert(rest, at, w)
	}
	m.save()
	m.touch()
	return m.workspaceViews()[m.workspace(id)], nil
}

// DeleteWorkspace removes a workspace. One with sessions is refused, unless
// end, which ends them first. The desk keeps one workspace at least.
func (m *Manager) DeleteWorkspace(ctx context.Context, id string, end bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadAll()
	i := m.workspace(id)
	if i < 0 {
		return missingf("no workspace %q", id)
	}
	if len(m.st.Workspaces) == 1 {
		return invalidf("the desk keeps one workspace at least")
	}
	var in []string
	for _, r := range m.st.Sessions {
		if r.Workspace == id {
			in = append(in, r.Name)
		}
	}
	if len(in) > 0 && !end {
		return fmt.Errorf("%w: %s has %d session(s): %s", ErrNotEmpty, m.st.Workspaces[i].Name, len(in), strings.Join(in, ", "))
	}
	for _, name := range in {
		if _, err := m.tmux(ctx, "kill-session", "-t", "="+name); err != nil && !gone(err) {
			m.save()
			return fmt.Errorf("end %s: %w", name, err)
		}
		m.st.Sessions = slices.DeleteFunc(m.st.Sessions, func(r record) bool { return r.Name == name })
		m.forgetNote(name)
		m.logf("%s: ended with its workspace", name)
	}
	m.renumber()
	m.st.Workspaces = slices.Delete(m.st.Workspaces, i, i+1)
	m.save()
	m.touch()
	return nil
}
