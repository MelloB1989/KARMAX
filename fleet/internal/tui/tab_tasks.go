package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// tasksTab is the open and recent tasks; ⏎ opens one's thread. r runs a
// new task, f follows up, a answers a question, c cancels, d closes.
type tasksTab struct {
	sel, off int
	detail   *taskView
	form     *runForm
	input    *lineInput
	confirm  *confirm
}

func newTasks() Tab { return &tasksTab{} }

func (t *tasksTab) Title() string            { return "Tasks" }
func (t *tasksTab) Visible(fleetapi.Me) bool { return true }
func (t *tasksTab) Capturing() bool {
	return t.form != nil || t.input != nil || t.confirm != nil
}

func (t *tasksTab) Help() []key.Binding {
	open := key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "open"))
	if t.detail != nil {
		open = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back"))
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "select")), open,
		keyIs("r", "run"), keyIs("f", "follow up"), keyIs("a", "answer"), keyIs("c", "cancel"), keyIs("d", "done"),
	}
}

// taskView is one task's thread, as fleetd last told it.
type taskView struct {
	id   string
	data *fleetapi.TaskDetail
	err  error
	vp   viewport.Model
	w    int // the width the thread was laid out for
}

// taskMsg is a task's thread, fetched.
type taskMsg struct {
	id   string
	data *fleetapi.TaskDetail
	err  error
}

// list is the tasks in the order shown: open, then closed, each newest first.
func taskList(s *fleetapi.Snapshot) (open, closed []fleetapi.Task) {
	if s == nil {
		return nil, nil
	}
	for _, t := range s.Tasks {
		if t.Closed.IsZero() {
			open = append(open, t)
		} else {
			closed = append(closed, t)
		}
	}
	return open, closed
}

func (t *tasksTab) selected(c *Ctx) (fleetapi.Task, bool) {
	if t.detail != nil {
		if t.detail.data != nil {
			// The snapshot's copy is fresher about its last message.
			for _, x := range snapTasks(c) {
				if x.ID == t.detail.id {
					return x, true
				}
			}
			return t.detail.data.Task, true
		}
		return fleetapi.Task{ID: t.detail.id}, true
	}
	all := snapTasks(c)
	if len(all) == 0 {
		return fleetapi.Task{}, false
	}
	t.sel = max(0, min(t.sel, len(all)-1))
	return all[t.sel], true
}

func snapTasks(c *Ctx) []fleetapi.Task {
	open, closed := taskList(c.Snap)
	return append(open, closed...)
}

func fetchTask(c *Ctx, id string) tea.Cmd {
	cl := c.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		d, err := cl.Task(ctx, id)
		return taskMsg{id: id, data: d, err: err}
	}
}

func (t *tasksTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	switch m := msg.(type) {
	case taskMsg:
		if t.detail != nil && t.detail.id == m.id {
			t.detail.data, t.detail.err, t.detail.w = m.data, m.err, 0
		}
		return nil
	case ToastMsg:
		// An action landed: the open thread has news.
		if t.detail != nil && !m.Err {
			return fetchTask(c, t.detail.id)
		}
		return nil
	case tea.KeyPressMsg:
		return t.key(m, c)
	}
	return nil
}

func (t *tasksTab) key(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	k := m.String()
	switch {
	case t.confirm != nil:
		cf := t.confirm
		t.confirm = nil
		return cf.update(k)
	case t.input != nil:
		cmd, done := t.input.update(m)
		if done {
			t.input = nil
		}
		return cmd
	case t.form != nil:
		cmd, done := t.form.update(m, c)
		if done {
			t.form = nil
		}
		return cmd
	}
	if k == "r" {
		t.form = newRunForm(c)
		return t.form.brief.Focus()
	}
	task, ok := t.selected(c)
	if !ok {
		return nil
	}
	cl := c.Client
	switch k {
	case "up", "k", "down", "j", "pgup", "pgdown", "home", "end":
		if t.detail != nil {
			var cmd tea.Cmd
			t.detail.vp, cmd = t.detail.vp.Update(m)
			return cmd
		}
		n := len(snapTasks(c))
		switch k {
		case "up", "k":
			t.sel = max(0, t.sel-1)
		case "down", "j":
			t.sel = min(n-1, t.sel+1)
		case "pgup":
			t.sel = max(0, t.sel-10)
		case "pgdown":
			t.sel = min(n-1, t.sel+10)
		case "home":
			t.sel = 0
		case "end":
			t.sel = n - 1
		}
	case "enter":
		if t.detail == nil {
			t.detail = &taskView{id: task.ID}
			return fetchTask(c, task.ID)
		}
	case "esc":
		t.detail = nil
	case "f":
		if !task.Closed.IsZero() {
			return toast(task.ID+" is closed", true)
		}
		id := task.ID
		t.input = newLineInput("Follow up "+id, func(text string) tea.Cmd {
			return call("sent to "+task.Agent, func(ctx context.Context) (string, error) { return "", cl.Followup(ctx, id, text) })
		})
		return t.input.in.Focus()
	case "a":
		if !task.Closed.IsZero() || task.LastKind != "question" {
			return toast(task.Agent+" is not asking anything on "+task.ID, true)
		}
		id := task.ID
		t.input = newLineInput("Answer "+task.Agent, func(text string) tea.Cmd {
			return call("answered "+task.Agent, func(ctx context.Context) (string, error) { return "", cl.Answer(ctx, id, text) })
		})
		return t.input.in.Focus()
	case "c", "d":
		if !task.Closed.IsZero() {
			return toast(task.ID+" is already closed", true)
		}
		t.confirm = taskConfirm(c, k, task.ID, task.Agent)
	}
	return nil
}

func (t *tasksTab) View(c *Ctx, w, h int) string {
	th := c.Theme
	if c.Snap == nil || w <= 0 || h <= 0 {
		return ""
	}
	if t.form != nil {
		return t.form.view(c, w, h)
	}
	var foot string
	switch {
	case t.confirm != nil:
		foot = t.confirm.view(th, w)
	case t.input != nil:
		foot = t.input.view(th, w)
	}
	bh := h
	if foot != "" {
		bh--
	}
	var body string
	if t.detail != nil {
		body = t.detailView(c, w, bh)
	} else {
		body = t.listView(c, w, bh)
	}
	if foot == "" {
		return body
	}
	if bh <= 0 {
		return foot
	}
	return block(body, w, bh) + "\n" + foot
}

func (t *tasksTab) listView(c *Ctx, w, h int) string {
	th := c.Theme
	open, closed := taskList(c.Snap)
	if len(open)+len(closed) == 0 {
		return center(th.Muted.Render("no tasks in the last 24 hours")+"\n\n"+hint(th, keyIs("r", "runs one")), w, h)
	}
	all := append(slices.Clone(open), closed...)
	t.sel = max(0, min(t.sel, len(all)-1))

	idW, agentW, whoW := 26, 11, 0
	if w >= 100 {
		whoW = 20
	}
	if w < 70 {
		idW = 20
	}
	section := func(title string, n int) string {
		label := th.Title.Render(title) + th.Muted.Render(fmt.Sprintf(" %d ", n))
		return label + lipgloss.NewStyle().Foreground(th.Line).Render(strings.Repeat("─", max(0, w-lipgloss.Width(label))))
	}
	type line struct {
		s    string
		task int // the task's index in all; -1 for a heading
	}
	var lines []line
	add := func(title string, ts []fleetapi.Task, base int) {
		if len(ts) == 0 {
			return
		}
		lines = append(lines, line{section(title, len(ts)), -1})
		for i, x := range ts {
			lines = append(lines, line{taskRow(c, x, base+i == t.sel, w, idW, agentW, whoW), base + i})
		}
	}
	add("Open", open, 0)
	if len(open) > 0 && len(closed) > 0 {
		lines = append(lines, line{"", -1})
	}
	add("Recent", closed, len(open))

	selAt := slices.IndexFunc(lines, func(l line) bool { return l.task == t.sel })
	t.off = scrollTo(t.off, selAt, len(lines), h)
	if t.off == 1 { // keep the first heading rather than scroll it off alone
		t.off = 0
	}
	var out []string
	for _, l := range lines[t.off:min(len(lines), t.off+h)] {
		out = append(out, l.s)
	}
	return block(strings.Join(out, "\n"), w, h)
}

// taskRow is one task: its state, id, agent, who gave it, age and last word.
func taskRow(c *Ctx, x fleetapi.Task, sel bool, w, idW, agentW, whoW int) string {
	th := c.Theme
	text := lipgloss.NewStyle().Foreground(th.Text)
	mark, idStyle := " ", text
	if sel {
		mark, idStyle = lipgloss.NewStyle().Foreground(th.Amber).Bold(true).Render("▸"), idStyle.Foreground(th.Amber).Bold(true)
	}
	var icon string
	switch {
	case x.Closed.IsZero() && x.LastKind == "question":
		icon = lipgloss.NewStyle().Foreground(th.Ground).Background(th.Cyan).Bold(true).Render("?")
	case x.Closed.IsZero():
		icon = paint(th.Amber, "●")
	case x.Outcome == "done":
		icon = paint(th.Green, "✓")
	case x.Outcome == "cancelled":
		icon = paint(th.Grey, "✕")
	default:
		icon = paint(th.Dim, "·")
	}
	row := mark + icon + " " + idStyle.Render(Fit(x.ID, idW)) + " " + text.Render(Fit(x.Agent, agentW)) + " "
	if whoW > 0 {
		row += th.Muted.Render(Fit(x.Assigner, whoW)) + " "
	}
	at := x.LastAt
	if at.IsZero() {
		at = x.Assigned
	}
	row += th.Muted.Render(Fit(Since(c.Now, at), 6)) + " "
	last := oneLine(x.Last)
	switch {
	case !x.Closed.IsZero() && x.Branch != "":
		last = x.Outcome + " · " + x.Branch
	case !x.Closed.IsZero() && last == "":
		last = x.Outcome
	}
	switch x.LastKind {
	case "question":
		row += paint(th.Cyan, last)
	case "progress":
		row += th.Muted.Render(last)
	default:
		row += text.Render(last)
	}
	return Fit(row, w)
}

func (t *tasksTab) detailView(c *Ctx, w, h int) string {
	th := c.Theme
	d := t.detail
	text := lipgloss.NewStyle().Foreground(th.Text)
	if d.err != nil {
		return center(th.Danger.Render("could not read "+d.id)+"\n"+th.Muted.Render(oneLine(d.err.Error()))+"\n\n"+hint(th, keyIs("esc", "back")), w, h)
	}
	if d.data == nil {
		return center(th.Muted.Render("reading "+d.id+"…"), w, h)
	}
	x := d.data.Task
	if s, ok := t.selected(c); ok {
		x = s
	}
	state := paint(th.Amber, "open")
	if !x.Closed.IsZero() {
		state = paint(th.Green, x.Outcome) + th.Muted.Render(" "+Since(c.Now, x.Closed)+" ago")
	}
	kv := func(k, v string) string { return th.Muted.Render(Fit(k, 9)) + v }
	head := []string{
		th.Strong.Render(x.ID) + "  " + state,
		kv("agent", text.Render(x.Agent)) + th.Muted.Render("   from ") + text.Render(x.Assigner) + th.Muted.Render("   "+Since(c.Now, x.Assigned)+" ago"),
	}
	var more []string
	if x.Repo != "" {
		more = append(more, th.Muted.Render("repo ")+text.Render(x.Repo))
	}
	if x.Model != "" {
		more = append(more, th.Muted.Render("model ")+text.Render(x.Model))
	}
	if x.Branch != "" {
		more = append(more, th.Muted.Render("branch ")+text.Render(x.Branch))
	}
	if x.PR != "" {
		more = append(more, th.Muted.Render("pr ")+text.Render(x.PR))
	}
	if d.data.Ahead > 0 {
		more = append(more, th.Muted.Render(fmt.Sprintf("%d commits ahead", d.data.Ahead)))
	}
	if len(more) > 0 {
		head = append(head, strings.Join(more, th.Muted.Render(" · ")))
	}
	head = append(head, lipgloss.NewStyle().Foreground(th.Line).Render(strings.Repeat("─", w)))
	vh := h - len(head)
	if vh <= 0 {
		return block(strings.Join(head, "\n"), w, h)
	}
	if d.w != w {
		bottom := d.w == 0
		d.vp = viewport.New(viewport.WithWidth(w), viewport.WithHeight(vh))
		d.vp.SetContent(thread(c, d.data, w))
		if bottom {
			d.vp.GotoBottom()
		}
		d.w = w
	}
	d.vp.SetHeight(vh)
	return block(strings.Join(head, "\n")+"\n"+d.vp.View(), w, h)
}

// thread is a task's messages, oldest first, each wrapped to w.
func thread(c *Ctx, d *fleetapi.TaskDetail, w int) string {
	th := c.Theme
	text := lipgloss.NewStyle().Foreground(th.Text)
	var b []string
	for _, m := range d.Reports {
		kc := th.Amber
		switch m.Kind {
		case "question":
			kc = th.Cyan
		case "progress":
			kc = th.Dim
		}
		kind := m.Kind
		if kind == "" {
			kind = "message"
		}
		head := th.Muted.Render(m.At.Format("15:04")+"  ") + th.Strong.Render(m.From) + th.Muted.Render(" → "+m.To)
		b = append(b, spread(head, paint(kc, kind), w))
		for _, l := range strings.Split(ansi.Wrap(strings.TrimRight(m.Text, "\n"), max(1, w-2), " -"), "\n") {
			b = append(b, "  "+text.Render(l))
		}
		if m.Error != "" {
			b = append(b, "  "+th.Danger.Render(m.Error))
		}
		b = append(b, "")
	}
	if len(d.Commits) > 0 {
		b = append(b, th.Title.Render("Commits"))
		for _, cm := range d.Commits {
			b = append(b, "  "+th.Muted.Render(Fit(cm, w-2)))
		}
	}
	if len(b) == 0 {
		return th.Muted.Render("no messages yet")
	}
	return strings.Join(b, "\n")
}

// runForm is `fleetctl run` as a form: the brief, then repo, agent, model.
type runForm struct {
	brief  textarea.Model
	repo   textinput.Model
	agents []string // "" is any free agent
	agent  int
	model  int
	field  int // 0 brief, 1 repo, 2 agent, 3 model
}

func newRunForm(c *Ctx) *runForm {
	ta := textarea.New()
	ta.Placeholder = "What should the agent do? Say what done looks like."
	ta.ShowLineNumbers = false
	ta.Prompt = ""
	ta.CharLimit = 0
	repo := textinput.New()
	repo.Prompt = ""
	repo.Placeholder = "a repository under the code directory, or none"
	f := &runForm{brief: ta, repo: repo, agents: []string{""}}
	lent := map[string]bool{}
	guest := c.Snap != nil && c.Snap.Me.Role == fleetapi.RoleGuest
	if guest {
		for _, a := range c.Snap.Me.Lent {
			lent[a] = true
		}
	}
	for _, g := range groups(c.Snap) {
		if guest && !lent[g.name] {
			continue
		}
		f.agents = append(f.agents, g.name)
	}
	return f
}

// models is the choice of models for the chosen agent: "" is its default.
func (f *runForm) models(c *Ctx) []string {
	out := []string{""}
	seen := map[string]bool{}
	for _, g := range groups(c.Snap) {
		if f.agents[f.agent] != "" && g.name != f.agents[f.agent] {
			continue
		}
		for _, m := range g.slots[0].Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func (f *runForm) focus() tea.Cmd {
	f.brief.Blur()
	f.repo.Blur()
	switch f.field {
	case 0:
		return f.brief.Focus()
	case 1:
		return f.repo.Focus()
	}
	return nil
}

// update takes a key; done says the form is finished (sent or dropped).
func (f *runForm) update(m tea.KeyPressMsg, c *Ctx) (tea.Cmd, bool) {
	k := m.String()
	switch k {
	case "esc":
		return nil, true
	case "ctrl+s":
		return f.send(c)
	case "tab":
		f.field = (f.field + 1) % 4
		return f.focus(), false
	case "shift+tab":
		f.field = (f.field + 3) % 4
		return f.focus(), false
	}
	var cmd tea.Cmd
	switch f.field {
	case 0:
		f.brief, cmd = f.brief.Update(m)
	case 1:
		if k == "enter" {
			return f.send(c)
		}
		f.repo, cmd = f.repo.Update(m)
	case 2, 3:
		n, at := len(f.agents), &f.agent
		if f.field == 3 {
			n, at = len(f.models(c)), &f.model
		}
		switch k {
		case "left", "h":
			*at = (*at + n - 1) % n
		case "right", "l", "space":
			*at = (*at + 1) % n
		case "enter":
			return f.send(c)
		}
		if f.field == 2 {
			f.model = 0
		}
	}
	return cmd, false
}

func (f *runForm) send(c *Ctx) (tea.Cmd, bool) {
	brief := strings.TrimSpace(f.brief.Value())
	if brief == "" {
		f.field = 0
		return tea.Batch(f.focus(), toast("the brief is empty", true)), false
	}
	models := f.models(c)
	req := fleetapi.RunRequest{Brief: brief, Repo: strings.TrimSpace(f.repo.Value()), Agent: f.agents[f.agent], Model: models[min(f.model, len(models)-1)]}
	cl := c.Client
	return call("", func(ctx context.Context) (string, error) {
		r, err := cl.Run(ctx, req)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("task %s → %s (%s)", r.Task, r.Agent, r.Why), nil
	}), true
}

func (f *runForm) view(c *Ctx, w, h int) string {
	th := c.Theme
	text := lipgloss.NewStyle().Foreground(th.Text)
	label := func(i int, s string) string {
		if f.field == i {
			return lipgloss.NewStyle().Foreground(th.Amber).Bold(true).Render("▸ " + Fit(s, 7))
		}
		return th.Muted.Render("  " + Fit(s, 7))
	}
	choice := func(i int, opts []string, at int, none string) string {
		v := opts[min(at, len(opts)-1)]
		if v == "" {
			v = none
		}
		s := text.Render(v)
		if f.field == i {
			s = th.Muted.Render("‹ ") + lipgloss.NewStyle().Foreground(th.Amber).Bold(true).Render(v) + th.Muted.Render(" ›")
		}
		return s + th.Muted.Render(fmt.Sprintf("   %d of %d", at+1, len(opts)))
	}
	fw := max(10, min(w-12, 100))
	bh := max(1, min(8, h-10))
	f.brief.SetWidth(fw - 2)
	f.brief.SetHeight(bh)
	f.repo.SetWidth(fw)
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(th.Line).Padding(0, 1)
	if f.field == 0 {
		box = box.BorderForeground(th.Amber)
	}
	briefBox := strings.Split(box.Render(f.brief.View()), "\n")
	lines := []string{th.Title.Render("New task") + th.Muted.Render("  as fleetctl run"), ""}
	for i, l := range briefBox {
		lead := "         "
		if i == 1 {
			lead = label(0, "brief")
		}
		lines = append(lines, Fit(lead, 9)+" "+l)
	}
	lines = append(lines,
		"",
		Fit(label(1, "repo"), 9)+" "+f.repo.View(),
		Fit(label(2, "agent"), 9)+" "+choice(2, f.agents, f.agent, "any free agent"),
		Fit(label(3, "model"), 9)+" "+choice(3, f.models(c), f.model, "the agent's default"),
		"",
		hint(th, keyIs("ctrl+s", "send"))+th.Muted.Render(" · ")+hint(th, keyIs("tab", "next field"))+th.Muted.Render(" · ")+
			hint(th, keyIs("←→", "choose"))+th.Muted.Render(" · ")+hint(th, keyIs("esc", "drop it")),
	)
	return block(strings.Join(lines, "\n"), w, h)
}
