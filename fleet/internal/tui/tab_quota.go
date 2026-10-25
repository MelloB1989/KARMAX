package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// quota is the Quota tab: a row per Claude account with its 5-hour and
// 7-day windows, burn rate and predicted run-out, the orchestrator's last,
// and the day's totals below.
type quota struct{ off int }

func newQuota() Tab { return &quota{} }

func (q *quota) Title() string               { return "Quota" }
func (q *quota) Visible(me fleetapi.Me) bool { return true }
func (q *quota) Capturing() bool             { return false }

var quotaScroll = key.NewBinding(key.WithKeys("up", "down", "k", "j"), key.WithHelp("↑↓", "scroll"))

func (q *quota) Help() []key.Binding { return []key.Binding{quotaScroll} }

func (q *quota) Update(msg tea.Msg, c *Ctx) tea.Cmd {
	switch keyOf(msg) {
	case "up", "k":
		q.off = max(0, q.off-1)
	case "down", "j":
		q.off++
	}
	return nil
}

// tableCol is one column of a table: its heading and width, and how soon it
// is dropped when the table does not fit (a higher drop goes first).
type tableCol[T any] struct {
	head string
	w    int
	drop int
	grow bool // takes what width is left over
	most int  // a growing column's widest; 0 is no limit
	cell func(T) string
}

// tableLayout draws rows as a table w cells wide, dropping columns until the rest
// fit, and returns the heading and the rows.
func tableLayout[T any](t *Theme, cols []tableCol[T], rows []T, w int) (string, []string) {
	keep := slices.Clone(cols)
	for {
		used := 0
		for _, c := range keep {
			used += c.w + 1
		}
		if used <= w+1 || len(keep) <= 1 {
			for i := range keep {
				if keep[i].grow {
					extra := max(0, w+1-used)
					if keep[i].most > 0 {
						extra = min(extra, max(0, keep[i].most-keep[i].w))
					}
					keep[i].w += extra
					break
				}
			}
			break
		}
		worst := 0
		for i, c := range keep {
			if c.drop > keep[worst].drop {
				worst = i
			}
		}
		if keep[worst].drop == 0 {
			break
		}
		keep = slices.Delete(keep, worst, worst+1)
	}
	var head []string
	for _, c := range keep {
		head = append(head, Fit(c.head, c.w))
	}
	var out []string
	for _, r := range rows {
		var cells []string
		for _, c := range keep {
			cells = append(cells, Fit(c.cell(r), c.w))
		}
		out = append(out, strings.Join(cells, " "))
	}
	return t.Muted.Render(strings.Join(head, " ")), out
}

// quotaOrder is the snapshot's accounts with the orchestrator's last.
func quotaOrder(s *fleetapi.Snapshot) []fleetapi.Account {
	as := slices.Clone(s.Quota)
	slices.SortStableFunc(as, func(a, b fleetapi.Account) int {
		switch {
		case a.Orchestrator == b.Orchestrator:
			return 0
		case a.Orchestrator:
			return 1
		}
		return -1
	})
	return as
}

// runOutCell is the account's prediction: when it runs out, in amber, only if
// that is before its window resets.
func runOutCell(t *Theme, a fleetapi.Account, now time.Time) string {
	amber := lipgloss.NewStyle().Foreground(t.Amber)
	switch {
	case a.FiveHour >= 100:
		return t.Danger.Render("out till " + hhmm(a.FiveResets, now))
	case !a.RunOut.IsZero() && (a.FiveResets.IsZero() || a.RunOut.Before(a.FiveResets)):
		return amber.Render("runs out " + hhmm(a.RunOut, now))
	}
	return t.Muted.Render("ok till reset")
}

// hhmm is a time as the TUI's user reads it: 20:40, in now's zone.
func hhmm(at, now time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return at.In(now.Location()).Format("15:04")
}

// untilAge is the time left until at: "1h32m", or "—".
func untilAge(now, at time.Time) string {
	if at.IsZero() {
		return "—"
	}
	return Age(at.Sub(now))
}

func pctCell(t *Theme, p float64) string {
	s := fmt.Sprintf("%3.0f%%", p)
	switch {
	case p >= 90:
		return t.Danger.Render(s)
	case p >= 70:
		return lipgloss.NewStyle().Foreground(t.Amber).Render(s)
	}
	return s
}

func (q *quota) View(c *Ctx, w, h int) string {
	t := c.Theme
	if c.Snap == nil {
		return centerNote(t, "waiting for fleetd…", w, h)
	}
	now := c.Now
	cols := []tableCol[fleetapi.Account]{
		{head: "ACCOUNT", w: 12, cell: func(a fleetapi.Account) string {
			if a.Orchestrator {
				return lipgloss.NewStyle().Foreground(t.Violet).Bold(true).Render(a.Name)
			}
			return t.Strong.Render(a.Name)
		}},
		{head: "AGENTS", w: 14, drop: 3, cell: func(a fleetapi.Account) string {
			if a.Orchestrator {
				return t.Muted.Render("orchestrator")
			}
			return t.Muted.Render(strings.Join(a.Agents, ","))
		}},
		{head: "5H", w: 15, cell: func(a fleetapi.Account) string { return Bar(t, a.FiveHour, 10) + " " + pctCell(t, a.FiveHour) }},
		{head: "RESETS", w: 6, drop: 1, cell: func(a fleetapi.Account) string { return untilAge(now, a.FiveResets) }},
		{head: "LAST 5H", w: 12, drop: 4, grow: true, most: 30, cell: func(a fleetapi.Account) string {
			return Spark(t, a.History, min(len(a.History), 30), 100, t.Cyan)
		}},
		{head: "BURN", w: 7, drop: 2, cell: func(a fleetapi.Account) string {
			if a.BurnPerHour <= 0 {
				return t.Muted.Render("—")
			}
			return fmt.Sprintf("%.1f%%/h", a.BurnPerHour)
		}},
		{head: "RUN-OUT", w: 14, cell: func(a fleetapi.Account) string { return runOutCell(t, a, now) }},
		{head: "7D", w: 12, drop: 5, cell: func(a fleetapi.Account) string { return Bar(t, a.SevenDay, 7) + " " + pctCell(t, a.SevenDay) }},
	}
	as := quotaOrder(c.Snap)
	head, rows := tableLayout(t, cols, as, w)
	for i, a := range as {
		if a.Orchestrator && i > 0 {
			rows = slices.Insert(rows, i, t.Border.Render(strings.Repeat("┄", w)))
			break
		}
	}
	if len(as) == 0 {
		rows = []string{t.Muted.Render("no quota yet")}
	}
	td := c.Snap.Today
	today := []string{
		"",
		t.Title.Render("Today") + t.Muted.Render(" since "+hhmm(td.Since, now)),
		fmt.Sprintf("%s tasks closed  ·  %s spent  ·  %s output tokens",
			t.Strong.Render(fmt.Sprint(td.TasksClosed)), t.Strong.Render(fmt.Sprintf("$%.2f", td.SpendUSD)),
			t.Strong.Render(tokenCount(td.OutputTokens))),
	}
	// The heading and Today stay put; the accounts scroll between them.
	room := max(0, h-1-len(today))
	q.off = max(0, min(q.off, len(rows)-room))
	body := rows[q.off:min(len(rows), q.off+room)]
	return fillBody(append(append([]string{head}, body...), today...), w, h)
}

// tokenCount is a count in thousands or millions: 1.28M.
func tokenCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
