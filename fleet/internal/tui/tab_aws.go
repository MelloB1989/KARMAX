package tui

import (
	"cmp"
	"context"
	"fmt"
	"image/color"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

const (
	awsEvery   = 5 * time.Minute // read again while shown, this old
	awsRetry   = time.Minute     // after a failed read
	awsTimeout = 2*time.Minute + 15*time.Second
)

// awsTab is the AWS tab (admins): the account's cost on cards, the last two
// weeks' spend and the services it went on, a chip per service, and every
// resource in a table to filter. fleetd reads AWS (GET /v1/aws) and caches
// it; r reads again past the cache.
type awsTab struct {
	rep       *fleetapi.AWSReport
	err       string
	flying    bool
	sentAt    time.Time
	gotAt     time.Time
	box       chan awsMsg // answers, kept for when the tab is shown (awsGotMsg)
	svc       string      // the service shown; "" is all
	filtering bool
	filter    textinput.Model
	sel, off  int
	showErrs  bool
}

type (
	awsMsg struct {
		rep     *fleetapi.AWSReport
		err     error
		refresh bool
	}
	// awsGotMsg says an answer is in the box. The app hands messages to the
	// shown tab only: one that lands elsewhere is still in the box when the
	// AWS tab is shown again.
	awsGotMsg struct{}
)

func newAWS() Tab {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "prod  t3  us-east-1  stopped…"
	return &awsTab{filter: in, box: make(chan awsMsg, 4)}
}

func (a *awsTab) Title() string               { return "AWS" }
func (a *awsTab) Visible(me fleetapi.Me) bool { return me.Role == fleetapi.RoleAdmin }
func (a *awsTab) Capturing() bool             { return a.filtering }

var awsKeys = struct{ Move, Filter, Service, Refresh, Errors, Keep, Clear key.Binding }{
	Move:    key.NewBinding(key.WithKeys("up", "down", "k", "j"), key.WithHelp("↑↓", "select")),
	Filter:  keyIs("/", "filter"),
	Service: key.NewBinding(key.WithKeys("s", "S"), key.WithHelp("s", "service")),
	Refresh: keyIs("r", "refresh"),
	Errors:  keyIs("e", "errors"),
	Keep:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "keep")),
	Clear:   keyIs("esc", "clear"),
}

func (a *awsTab) Help() []key.Binding {
	k := awsKeys
	if a.filtering {
		return []key.Binding{k.Keep, k.Clear}
	}
	out := []key.Binding{k.Move, k.Filter, k.Service, k.Refresh}
	if a.rep != nil && len(a.rep.Errors) > 0 {
		out = append(out, k.Errors)
	}
	if a.svc != "" || a.filter.Value() != "" {
		out = append(out, k.Clear)
	}
	return out
}

func (a *awsTab) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	cmds := []tea.Cmd{a.collect(c)}
	if m, ok := msg.(tea.KeyPressMsg); ok {
		cmds = append(cmds, a.key(m, c))
	}
	return tea.Batch(append(cmds, a.fetch(c, false))...)
}

// collect takes the answers in the box.
func (a *awsTab) collect(c *Ctx) tea.Cmd {
	var cmds []tea.Cmd
	for {
		select {
		case m := <-a.box:
			a.flying = false
			if m.err != nil {
				a.err = m.err.Error()
				if m.refresh {
					cmds = append(cmds, toastCmd("AWS: "+a.err, true))
				}
				continue
			}
			a.rep, a.err, a.gotAt = m.rep, "", c.Now
			if m.refresh {
				cmds = append(cmds, toastCmd(fmt.Sprintf("read AWS: %d resources", len(m.rep.Items)), false))
			}
		default:
			return tea.Batch(cmds...)
		}
	}
}

// fetch reads the account when the tab has none, or it is awsEvery old (a
// failed read waits awsRetry); refresh reads at once, past fleetd's cache.
func (a *awsTab) fetch(c *Ctx, refresh bool) tea.Cmd {
	if c.Client == nil || a.flying && c.Now.Sub(a.sentAt) < awsTimeout {
		return nil
	}
	if !refresh {
		switch {
		case a.err != "" && c.Now.Sub(a.sentAt) < awsRetry:
			return nil
		case a.err == "" && a.rep != nil && c.Now.Sub(a.gotAt) < awsEvery:
			return nil
		}
	}
	a.flying, a.sentAt = true, c.Now
	cl, box := c.Client, a.box
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), awsTimeout)
		defer cancel()
		rep, err := cl.AWS(ctx, refresh)
		select {
		case box <- awsMsg{rep: rep, err: err, refresh: refresh}:
		default:
		}
		return awsGotMsg{}
	}
}

func (a *awsTab) key(m tea.KeyPressMsg, c *Ctx) tea.Cmd {
	k := m.String()
	if a.filtering {
		switch k {
		case "enter":
			a.filtering = false
			a.filter.Blur()
		case "esc":
			a.filtering = false
			a.filter.Blur()
			a.filter.SetValue("")
		default:
			var cmd tea.Cmd
			a.filter, cmd = a.filter.Update(m)
			a.sel, a.off = 0, 0
			return cmd
		}
		return nil
	}
	n := len(a.rows())
	switch k {
	case "/":
		a.filtering = true
		return a.filter.Focus()
	case "esc":
		a.filter.SetValue("")
		a.svc, a.sel, a.off = "", 0, 0
	case "s", "S":
		a.svc = a.nextService(k == "S")
		a.sel, a.off = 0, 0
	case "r":
		if a.flying {
			return toastCmd("already reading AWS…", false)
		}
		return a.fetch(c, true)
	case "e":
		a.showErrs = !a.showErrs
	case "up", "k":
		a.sel = max(0, a.sel-1)
	case "down", "j":
		a.sel = max(0, min(n-1, a.sel+1))
	case "pgup":
		a.sel = max(0, a.sel-10)
	case "pgdown":
		a.sel = max(0, min(n-1, a.sel+10))
	case "home", "g":
		a.sel = 0
	case "end", "G":
		a.sel = max(0, n-1)
	}
	return nil
}

// services is the report's services, in its order.
func (a *awsTab) services() []string {
	var out []string
	if a.rep != nil {
		for _, it := range a.rep.Items {
			if !slices.Contains(out, it.Service) {
				out = append(out, it.Service)
			}
		}
	}
	return out
}

// nextService is the service after the shown one ("" is all), or before.
func (a *awsTab) nextService(back bool) string {
	cycle := append([]string{""}, a.services()...)
	i := slices.Index(cycle, a.svc)
	if back {
		return cycle[(i+len(cycle)-1)%len(cycle)]
	}
	return cycle[(i+1)%len(cycle)]
}

// rows is the resources shown: of the service, matching every filter term
// in any field.
func (a *awsTab) rows() []fleetapi.AWSResource {
	if a.rep == nil {
		return nil
	}
	terms := strings.Fields(strings.ToLower(a.filter.Value()))
	var out []fleetapi.AWSResource
	for _, it := range a.rep.Items {
		if a.svc != "" && it.Service != a.svc {
			continue
		}
		all := strings.ToLower(strings.Join([]string{it.Service, it.Region, it.ID, it.Name, it.Type, it.State, it.Detail}, " "))
		if !slices.ContainsFunc(terms, func(q string) bool { return !strings.Contains(all, q) }) {
			out = append(out, it)
		}
	}
	return out
}

// awsLabel is what a resource is called: its name, else its ID.
func awsLabel(it fleetapi.AWSResource) string {
	if it.Name != "" {
		return it.Name
	}
	return it.ID
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (a *awsTab) View(c *Ctx, w, h int) string {
	t := c.Theme
	if w <= 0 || h <= 0 {
		return fillBody(nil, w, h)
	}
	spin := lipgloss.NewStyle().Foreground(t.Amber).Render(spinner[c.Frame%len(spinner)])
	if a.rep == nil {
		if a.err != "" && !a.flying {
			return centerNote(t, oneLine(a.err), w, h)
		}
		return centerNote(t, spinner[c.Frame%len(spinner)]+" reading the AWS account…", w, h)
	}
	r := a.rep
	rows := a.rows()
	a.sel = max(0, min(a.sel, len(rows)-1))

	var top []string
	top = append(top, awsCards(t, r, c.Now, w, h < 30)...)
	if h >= 30 && w >= 96 && r.Cost != nil {
		top = append(top, awsCharts(t, r, w)...)
	} else if r.Cost != nil && len(r.Cost.ByService) > 0 {
		top = append(top, awsTopLine(t, r.Cost, w))
	}
	top = append(top, awsChips(t, r.Items, a.svc, w)...)

	// The status line: what is shown, the filter, how old the read is.
	left := t.Title.Render("Resources") + t.Muted.Render(fmt.Sprintf(" %d", len(rows)))
	if len(rows) != len(r.Items) {
		left += t.Muted.Render(fmt.Sprintf(" of %d", len(r.Items)))
	}
	if a.svc != "" {
		left += t.Muted.Render(" · ") + lipgloss.NewStyle().Foreground(t.Cyan).Render(a.svc)
	}
	switch {
	case a.filtering:
		a.filter.SetWidth(max(1, w/3))
		left += t.Muted.Render("   filter ▸ ") + a.filter.View()
	case a.filter.Value() != "":
		left += t.Muted.Render("   filter ") + lipgloss.NewStyle().Foreground(t.Cyan).Render(a.filter.Value()) + t.Muted.Render("  (esc clears)")
	}
	right := t.Muted.Render("read " + Since(c.Now, r.At) + " ago")
	if r.Cost != nil && !r.Cost.At.IsZero() {
		right += t.Muted.Render(" · cost " + Since(c.Now, r.Cost.At) + " ago")
	}
	if a.flying {
		right = spin + t.Muted.Render(" reading…")
	} else if a.err != "" {
		right = t.Danger.Render("✕ ") + t.Muted.Render(oneLine(a.err))
	}

	foot := awsErrors(t, r.Errors, a.showErrs, w, h)
	room := max(0, h-len(top)-2-len(foot))
	if room < 3 { // a short terminal: the table first
		top = nil
		room = max(0, h-2-len(foot))
	}
	lines := append(top, spread(left, right, w))
	head, body := awsTable(t, rows, c.Now, w-2)
	lines = append(lines, "  "+head)
	if len(rows) == 0 {
		msg := "nothing matches"
		if len(r.Items) == 0 {
			msg = "nothing found in " + strings.Join(r.Regions, ", ")
		}
		body = []string{t.Muted.Render(msg)}
	}
	a.off = keepInView(a.off, a.sel, len(body), room)
	for i := a.off; i < min(len(body), a.off+room); i++ {
		mark := "  "
		if i == a.sel && len(rows) > 0 {
			mark = t.Selected.Render("▸ ")
		}
		lines = append(lines, mark+body[i])
	}
	for len(lines) < h-len(foot) {
		lines = append(lines, "")
	}
	return fillBody(append(lines, foot...), w, h)
}

// awsErrors is the parts that could not be read: a line, or the list.
func awsErrors(t *Theme, errs []string, open bool, w, h int) []string {
	if len(errs) == 0 || h < 8 {
		return nil
	}
	amber := lipgloss.NewStyle().Foreground(t.Amber)
	noun := "parts"
	if len(errs) == 1 {
		noun = "part"
	}
	if !open {
		return []string{amber.Render(fmt.Sprintf("▲ %d %s could not be read", len(errs), noun)) + t.Muted.Render(" — e to show")}
	}
	out := []string{amber.Render(fmt.Sprintf("▼ %d %s could not be read", len(errs), noun)) + t.Muted.Render(" — e to hide")}
	most := max(1, h/3-1)
	for i, e := range errs {
		if i == most-1 && len(errs) > most {
			out = append(out, t.Muted.Render(fmt.Sprintf("  … and %d more", len(errs)-i)))
			break
		}
		out = append(out, t.Muted.Render("  "+e))
	}
	return out
}

// awsCards is the top row: the account, the month so far, the forecast and
// last month, each in a box; spark puts the last 14 days in the month's.
// Each line says the most that fits its card.
func awsCards(t *Theme, r *fleetapi.AWSReport, now time.Time, w int, spark bool) []string {
	cols := 4
	switch {
	case w < 32:
		cols = 1
	case w < 64:
		cols = 2
	}
	big, dim := t.Strong, t.Muted
	type card struct {
		title string
		c     color.Color
		lines [][]string // each line's choices, longest first
	}
	who := r.User
	if who == "" {
		who = "?"
	}
	regions := fmt.Sprintf("%d regions", len(r.Regions))
	if len(r.Regions) == 1 {
		regions = r.Regions[0]
	}
	acct := big.Render(r.Account)
	if r.Account == "" {
		acct = dim.Render("—") // the keys could not say whose they are
	}
	cards := []card{{"Account", t.Violet, [][]string{{acct}, {dim.Render(who + " · " + regions), dim.Render(who)}}}}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	prev := month.AddDate(0, -1, 0)
	if cost := r.Cost; cost != nil {
		cur := cost.Currency
		span := fmt.Sprintf("%s 1–%d", now.Format("Jan"), now.Day())
		under := []string{dim.Render(span)}
		if cost.Credits >= 0.005 {
			credits := paint(t.Green, "−"+awsMoney(cur, cost.Credits)+" credits")
			under = []string{dim.Render(span+" · ") + credits, credits, dim.Render(span)}
		}
		if spark && len(cost.Daily) > 0 {
			bars := Spark(t, awsPoints(cost.Daily), len(cost.Daily), 0, t.Cyan)
			under = []string{bars + dim.Render(" 14d"), bars}
		}
		mtd := [][]string{{big.Render(awsMoney(cur, cost.MonthToDate))}, under}
		fc := [][]string{{dim.Render("—")}, {dim.Render("not enough data yet"), dim.Render("no data yet")}}
		if cost.Forecast > 0 {
			end := month.AddDate(0, 1, -1).Format("Jan 2")
			fc = [][]string{{big.Render(awsMoney(cur, cost.Forecast))}, {dim.Render("by " + end)}}
			if cost.LastMonth > 0 {
				d := (cost.Forecast - cost.LastMonth) / cost.LastMonth * 100
				arrow, col := "▲", t.Amber
				switch {
				case d >= 10:
					col = t.Alert
				case d < 0:
					arrow, col = "▼", t.Green
				}
				fc[0][0] += " " + lipgloss.NewStyle().Foreground(col).Bold(true).Render(fmt.Sprintf("%s %.1f%%", arrow, math.Abs(d)))
				fc[1] = []string{dim.Render("by " + end + " · vs " + prev.Format("January")), dim.Render(end + " · vs " + prev.Format("Jan"))}
			}
		}
		perDay := awsMoney(cur, cost.LastMonth/(month.Sub(prev).Hours()/24)) + "/day"
		last := [][]string{{big.Render(awsMoney(cur, cost.LastMonth))},
			{dim.Render(prev.Format("January") + " · " + perDay), dim.Render(prev.Format("Jan") + " · " + perDay), dim.Render(perDay)}}
		cards = append(cards, card{"Month to date", t.Amber, mtd}, card{"Forecast", t.Cyan, fc}, card{"Last month", t.Dim, last})
	} else {
		none := [][]string{{dim.Render("—")}, {dim.Render("no cost yet")}}
		cards = append(cards, card{"Month to date", t.Amber, none}, card{"Forecast", t.Cyan, none}, card{"Last month", t.Dim, none})
	}
	var out []string
	for i := 0; i < len(cards); i += cols {
		row := make([]string, 4)
		for j := range min(cols, len(cards)-i) {
			cw := w / cols
			if j == cols-1 {
				cw = w - (cols-1)*(w/cols)
			}
			cd := cards[i+j]
			var body []string
			for _, choices := range cd.lines {
				body = append(body, " "+fitting(cw-4, choices))
			}
			box := tileBox(t, lipgloss.NewStyle().Foreground(t.Text).Render(cd.title), cd.c, false, body, cw, 4)
			for k := range row {
				row[k] += box[k]
			}
		}
		out = append(out, row...)
	}
	return out
}

// fitting is the first of choices that fits in w cells, else the last.
func fitting(w int, choices []string) string {
	for _, c := range choices {
		if lipgloss.Width(c) <= w {
			return c
		}
	}
	return choices[len(choices)-1]
}

func awsPoints(days []fleetapi.AWSAmount) []fleetapi.Point {
	out := make([]fleetapi.Point, len(days))
	for i, d := range days {
		out[i] = fleetapi.Point{V: d.Amount}
	}
	return out
}

// awsCharts is the daily bars and the top services, side by side in boxes,
// and on a wide terminal where the resources are.
func awsCharts(t *Theme, r *fleetapi.AWSReport, w int) []string {
	const h = 7
	cost := r.Cost
	lw := min(w/2, max(46, len(cost.Daily)*4+4))
	hi, sum := 0.0, 0.0
	for _, d := range cost.Daily {
		hi, sum = math.Max(hi, d.Amount), sum+d.Amount
	}
	title := t.Title.Render("Daily") + t.Muted.Render(" · 14 days")
	if len(cost.Daily) > 0 {
		title += t.Muted.Render(" · avg " + awsMoney(cost.Currency, sum/float64(len(cost.Daily))) + " · max " + awsMoney(cost.Currency, hi))
	}
	boxes := [][]string{tileBox(t, title, t.Line, false, awsBars(t, cost.Daily, lw-4, h-2), lw, h)}
	mw := w - lw
	if w >= 160 {
		mw = (w - lw) * 11 / 20
	}
	boxes = append(boxes, tileBox(t, t.Title.Render("Top services")+t.Muted.Render(" · month to date"), t.Line, false,
		awsServiceBars(t, cost, mw-4, h-2), mw, h))
	if rw := w - lw - mw; rw > 0 {
		boxes = append(boxes, tileBox(t, t.Title.Render("By region")+t.Muted.Render(fmt.Sprintf(" · %d resources", len(r.Items))), t.Line, false,
			awsRegionBars(t, r.Items, rw-4, h-2), rw, h))
	}
	out := make([]string, h)
	for i := range out {
		for _, b := range boxes {
			out[i] += b[i]
		}
	}
	return out
}

// awsRegionBars is how many resources each region holds, most first, with
// its busiest services.
func awsRegionBars(t *Theme, items []fleetapi.AWSResource, w, rows int) []string {
	count := map[string]int{}
	svcs := map[string]map[string]int{}
	for _, it := range items {
		count[it.Region]++
		if svcs[it.Region] == nil {
			svcs[it.Region] = map[string]int{}
		}
		svcs[it.Region][it.Service]++
	}
	regions := slices.Collect(maps.Keys(count))
	slices.SortFunc(regions, func(a, b string) int {
		if c := cmp.Compare(count[b], count[a]); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	if len(regions) == 0 {
		return []string{t.Muted.Render(" nothing found")}
	}
	barW := min(20, max(4, w/4))
	var out []string
	for _, rg := range regions[:min(rows, len(regions))] {
		ss := slices.Collect(maps.Keys(svcs[rg]))
		slices.SortFunc(ss, func(a, b string) int {
			if c := cmp.Compare(svcs[rg][b], svcs[rg][a]); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		var top []string
		for _, sv := range ss[:min(3, len(ss))] {
			top = append(top, fmt.Sprintf("%s %d", sv, svcs[rg][sv]))
		}
		out = append(out, " "+lipgloss.NewStyle().Foreground(t.Text).Render(Fit(rg, 12))+" "+
			awsHBar(t, float64(count[rg])/float64(count[regions[0]]), barW, t.Violet)+" "+
			t.Strong.Render(fmt.Sprintf("%3d", count[rg]))+"  "+t.Muted.Render(strings.Join(top, " · ")))
	}
	return out
}

// awsBars is days as vertical bars rows high: rows-1 of bars, scaled to the
// highest, eighths at their tops, and the day of the month under each.
// Today's is lit.
func awsBars(t *Theme, days []fleetapi.AWSAmount, w, rows int) []string {
	n := len(days)
	if n == 0 || w < n || rows < 2 {
		return []string{t.Muted.Render(" no days yet")}
	}
	colW := min(5, w/n)
	barW := max(1, colW-1)
	if colW == 1 {
		barW = 1
	}
	hi := 0.0
	for _, d := range days {
		hi = math.Max(hi, d.Amount)
	}
	pad := strings.Repeat(" ", max(0, (w-colW*n)/2))
	levels := (rows - 1) * 8
	out := make([]string, rows)
	for r := 0; r < rows-1; r++ {
		var b strings.Builder
		b.WriteString(pad)
		for i, d := range days {
			lv := 0
			if hi > 0 {
				lv = int(math.Round(d.Amount / hi * float64(levels)))
				if d.Amount > 0 && lv == 0 {
					lv = 1
				}
			}
			fill := lv - (rows-2-r)*8
			ch := " "
			switch {
			case fill >= 8:
				ch = "█"
			case fill > 0:
				ch = string(sparks[fill-1])
			}
			c := t.Cyan
			if i == n-1 {
				c = t.Amber
			}
			b.WriteString(paint(c, strings.Repeat(ch, barW)) + strings.Repeat(" ", colW-barW))
		}
		out[r] = b.String()
	}
	var lb strings.Builder
	lb.WriteString(pad)
	for i, d := range days {
		day := d.Name[max(0, len(d.Name)-2):]
		switch {
		case colW >= 3 || colW == 2 && i%2 == 0:
			lb.WriteString(Fit(day, colW))
		default:
			lb.WriteString(strings.Repeat(" ", colW))
		}
	}
	out[rows-1] = t.Muted.Render(lb.String())
	return out
}

// awsServiceBars is the costliest services, rows of them, each with a bar
// against the costliest, its amount and its share.
func awsServiceBars(t *Theme, cost *fleetapi.AWSCost, w, rows int) []string {
	svcs := cost.ByService
	if len(svcs) == 0 {
		return []string{t.Muted.Render(" nothing spent yet")}
	}
	show := min(rows, len(svcs))
	if len(svcs) > rows {
		show = rows - 1
	}
	total := 0.0
	for _, s := range svcs {
		total += s.Amount
	}
	nameW := min(14, max(8, w/4))
	amtW := 10
	barW := min(48, max(4, w-nameW-amtW-4-4))
	cols := lipgloss.Blend1D(max(2, show), t.Amber, t.Violet, t.Cyan)
	var out []string
	for i, s := range svcs[:show] {
		frac := s.Amount / svcs[0].Amount
		out = append(out, " "+lipgloss.NewStyle().Foreground(t.Text).Render(Fit(awsService(s.Name), nameW))+" "+
			awsHBar(t, frac, barW, cols[i])+" "+t.Strong.Render(fmt.Sprintf("%*s", amtW, awsMoney(cost.Currency, s.Amount)))+
			t.Muted.Render(fmt.Sprintf(" %3.0f%%", s.Amount/total*100)))
	}
	if rest := len(svcs) - show; rest > 0 {
		sum := 0.0
		for _, s := range svcs[show:] {
			sum += s.Amount
		}
		out = append(out, t.Muted.Render(fmt.Sprintf(" +%d more · %s", rest, awsMoney(cost.Currency, sum))))
	}
	return out
}

// awsHBar is a w-cell bar for frac (0–1), eighths at its end, on a track.
func awsHBar(t *Theme, frac float64, w int, c color.Color) string {
	eighths := int(math.Round(math.Max(0, math.Min(frac, 1)) * float64(w*8)))
	full, part := eighths/8, eighths%8
	s := strings.Repeat("█", full)
	if part > 0 {
		s += string([]rune("▏▎▍▌▋▊▉")[part-1])
	}
	used := full + min(part, 1)
	return paint(c, s) + paint(t.Line, strings.Repeat("·", w-used))
}

// awsTopLine is the costliest services on one line, for a short terminal.
func awsTopLine(t *Theme, cost *fleetapi.AWSCost, w int) string {
	parts := []string{t.Title.Render("Top")}
	used := 3
	for _, s := range cost.ByService {
		p := lipgloss.NewStyle().Foreground(t.Text).Render(awsService(s.Name)) + " " + t.Strong.Render(awsMoney(cost.Currency, s.Amount))
		if used+lipgloss.Width(p)+3 > w {
			parts = append(parts, t.Muted.Render("…"))
			break
		}
		parts = append(parts, p)
		used += lipgloss.Width(p) + 3
	}
	return strings.Join(parts, t.Muted.Render(" · "))
}

// awsChips is a chip per service with its count; where some of a
// service's resources are not up, the count of each state instead: ec2
// 3 running/1 pending/1 stopped. The service shown is lit.
func awsChips(t *Theme, items []fleetapi.AWSResource, shown string, w int) []string {
	type tally struct {
		n      int
		states map[string]int
		down   bool // some are on their way or down
	}
	var order []string
	by := map[string]*tally{}
	for _, it := range items {
		x := by[it.Service]
		if x == nil {
			x = &tally{states: map[string]int{}}
			by[it.Service] = x
			order = append(order, it.Service)
		}
		x.n++
		if it.State != "" {
			x.states[it.State]++
		}
		if k := awsStateKind(it.State); k == stateWait || k == stateBad {
			x.down = true
		}
	}
	var chips []string
	for _, s := range order {
		x := by[s]
		name := t.Strong.Render(s)
		if s == shown {
			name = t.Badge.Render(s)
		}
		chip := name + " " + lipgloss.NewStyle().Foreground(t.Text).Render(strconv.Itoa(x.n))
		if x.down {
			states := slices.Collect(maps.Keys(x.states))
			// Up first, then on the way, then down; by name within each.
			slices.SortFunc(states, func(a, b string) int {
				if ka, kb := awsStateKind(a), awsStateKind(b); ka != kb {
					return cmp.Compare(ka, kb)
				}
				return strings.Compare(a, b)
			})
			var parts []string
			for _, st := range states {
				parts = append(parts, awsStateCell(t, fmt.Sprintf("%d %s", x.states[st], awsWords(st)), st))
			}
			chip = name + " " + strings.Join(parts, t.Muted.Render("/"))
		}
		chips = append(chips, chip)
	}
	if len(chips) == 0 {
		return nil
	}
	var lines []string
	line := ""
	for _, ch := range chips {
		switch {
		case line == "":
			line = ch
		case lipgloss.Width(line)+3+lipgloss.Width(ch) > w:
			lines = append(lines, line)
			line = ch
		default:
			line += "   " + ch
		}
	}
	lines = append(lines, line)
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return lines
}

type stateKind int

const (
	stateNone stateKind = iota
	stateGood
	stateWait
	stateBad
)

// awsWords is a state as words: InProgress is "in progress".
func awsWords(state string) string {
	var b strings.Builder
	for i, r := range state {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// awsStateKind is what a resource's state means: up, on its way, down, or
// nothing said.
func awsStateKind(s string) stateKind {
	switch strings.ToLower(s) {
	case "running", "available", "active", "deployed", "inservice", "in-use", "enabled":
		return stateGood
	case "pending", "creating", "starting", "stopping", "modifying", "provisioning", "inprogress", "in progress",
		"backing-up", "rebooting", "updating", "deleting", "shutting-down", "upgrading", "maintenance":
		return stateWait
	case "stopped", "failed", "terminated", "error", "disabled", "inactive", "incompatible-parameters",
		"storage-full", "inaccessible-encryption-credentials", "active_impaired":
		return stateBad
	}
	return stateNone
}

// awsStateCell is text in the color of state: green up, amber on its way,
// red down, dim otherwise.
func awsStateCell(t *Theme, text, state string) string {
	switch awsStateKind(state) {
	case stateGood:
		return paint(t.Green, text)
	case stateWait:
		return paint(t.Amber, text)
	case stateBad:
		return paint(t.Alert, text)
	}
	if text == "" {
		return t.Muted.Render("—")
	}
	return t.Muted.Render(text)
}

// awsTable is the resources as a table w cells wide: its heading and rows.
// The name takes a fair share; the detail takes the rest.
func awsTable(t *Theme, rows []fleetapi.AWSResource, now time.Time, w int) (string, []string) {
	nameW := min(40, max(22, w/4))
	cols := []tableCol[fleetapi.AWSResource]{
		{head: "SERVICE", w: 10, cell: func(it fleetapi.AWSResource) string { return lipgloss.NewStyle().Foreground(t.Cyan).Render(it.Service) }},
		{head: "REGION", w: 10, drop: 3, cell: func(it fleetapi.AWSResource) string { return t.Muted.Render(it.Region) }},
		{head: "NAME / ID", w: nameW, cell: func(it fleetapi.AWSResource) string {
			s := t.Strong.Render(awsLabel(it))
			if it.Name != "" && it.Name != it.ID {
				s += " " + t.Muted.Render(it.ID)
			}
			return s
		}},
		{head: "TYPE", w: 15, drop: 2, cell: func(it fleetapi.AWSResource) string { return it.Type }},
		{head: "STATE", w: 10, cell: func(it fleetapi.AWSResource) string { return awsStateCell(t, it.State, it.State) }},
		{head: "DETAIL", w: 12, drop: 4, grow: true, cell: func(it fleetapi.AWSResource) string { return t.Muted.Render(it.Detail) }},
		{head: "AGE", w: 5, drop: 1, cell: func(it fleetapi.AWSResource) string { return awsAge(now, it.Created) }},
	}
	return tableLayout(t, cols, rows, w)
}

// awsAge is how long ago at was: 40m, 6h, 41d, 1.9y; "—" when unknown.
func awsAge(now, at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	d := now.Sub(at)
	switch {
	case d < 24*time.Hour:
		return Age(d)
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%.1fy", d.Hours()/24/365)
}

// awsMoney is an amount in its currency: $1,234.56; −$5.00.
func awsMoney(cur string, v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "−", -v
	}
	whole, cents, _ := strings.Cut(strconv.FormatFloat(v, 'f', 2, 64), ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	sym := "$"
	if cur != "" && cur != "USD" {
		sym = cur + " "
	}
	return sign + sym + whole + "." + cents
}

// awsServiceNames are Cost Explorer's service names as people say them.
var awsServiceNames = map[string]string{
	"Amazon Elastic Compute Cloud - Compute":          "EC2",
	"EC2 - Other":                                     "EC2 other",
	"Amazon Relational Database Service":              "RDS",
	"Amazon Simple Storage Service":                   "S3",
	"Amazon Elastic Load Balancing":                   "ELB",
	"Amazon Virtual Private Cloud":                    "VPC",
	"AmazonCloudWatch":                                "CloudWatch",
	"Amazon EC2 Container Registry (ECR)":             "ECR",
	"Amazon Elastic Container Service":                "ECS",
	"Amazon Elastic Container Service for Kubernetes": "EKS",
	"Amazon Elastic Kubernetes Service":               "EKS",
	"Amazon Simple Queue Service":                     "SQS",
	"Amazon Simple Notification Service":              "SNS",
	"Amazon Simple Email Service":                     "SES",
	"AWS Key Management Service":                      "KMS",
	"Amazon Elastic File System":                      "EFS",
	"Amazon Elastic Block Store":                      "EBS",
	"Amazon Elastic Container Registry Public":        "ECR public",
}

// awsService is a service's short name: from the list, else without
// "Amazon " or "AWS " in front.
func awsService(name string) string {
	if s, ok := awsServiceNames[name]; ok {
		return s
	}
	for _, p := range []string{"Amazon ", "AWS "} {
		name = strings.TrimPrefix(name, p)
	}
	return name
}
