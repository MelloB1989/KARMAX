package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// awsOpen is the AWS tab shown on the Fake, its first read in.
func awsOpen(t *testing.T) (*Ctx, *fleetapi.Fake, *awsTab) {
	t.Helper()
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	tab := newAWS().(*awsTab)
	tabSettle(tab, c, 120, 40)
	if tab.rep == nil {
		t.Fatalf("no report after the first show: calls %q", f.Calls)
	}
	return c, f, tab
}

// awsRows is the table's rows as shown: the name/ID column of each.
func awsRows(c *Ctx, tab *awsTab) []string {
	var out []string
	for _, it := range tab.rows() {
		out = append(out, it.Service+"/"+awsLabel(it))
	}
	return out
}

func TestAWSGoldenViews(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {200, 60}} {
		c, _ := tabCtx(t, fleetapi.RoleAdmin)
		tab := newAWS()
		tabSettle(tab, c, sz[0], sz[1])
		view := tab.View(c, sz[0], sz[1])
		tabSize(t, "AWS", view, sz[0], sz[1])
		tabGolden(t, fmt.Sprintf("aws_admin_%dx%d.golden", sz[0], sz[1]), ansi.Strip(view)+"\n")
	}
}

// The errors fold out with e.
func TestAWSErrorsFoldOut(t *testing.T) {
	c, _, tab := awsOpen(t)
	v := ansi.Strip(tab.View(c, 120, 40))
	if !strings.Contains(v, "2 parts could not be read") || strings.Contains(v, "eks ap-south-1: AccessDenied") {
		t.Fatalf("folded:\n%s", v)
	}
	tab.Update(tabKey("e"), c)
	v = ansi.Strip(tab.View(c, 120, 40))
	if !strings.Contains(v, "eks ap-south-1: AccessDenied") || !strings.Contains(v, "cognito-idp us-east-1: no answer in time") {
		t.Errorf("unfolded:\n%s", v)
	}
}

func TestAWSFilterByText(t *testing.T) {
	c, _, tab := awsOpen(t)
	all := len(tab.rows())
	tab.Update(tabKey("/"), c)
	if !tab.Capturing() {
		t.Fatal("/ did not open the filter")
	}
	for _, r := range "prod" {
		tab.Update(tabKey(string(r)), c)
	}
	tab.Update(tabKey("enter"), c)
	if tab.Capturing() {
		t.Error("enter left the filter open")
	}
	got := awsRows(c, tab)
	want := []string{"ec2/api-prod-1", "ec2/api-prod-2", "ecs/prod-cluster", "elb/api-prod-alb", "s3/example-assets-prod"}
	if !slices.Equal(got, want) {
		t.Errorf("prod: %q, want %q", got, want)
	}
	if v := ansi.Strip(tab.View(c, 120, 40)); !strings.Contains(v, "filter prod") {
		t.Errorf("the filter is not shown:\n%s", v)
	}
	// Terms narrow each other, in any field: a type, a region.
	tab.Update(tabKey("esc"), c)
	tab.Update(tabKey("/"), c)
	for _, r := range "python us-east" {
		tab.Update(tabKey(string(r)), c)
	}
	if got := awsRows(c, tab); !slices.Equal(got, []string{"lambda/cert-renewer", "lambda/cost-alerts"}) {
		t.Errorf("python us-east: %q", got)
	}
	tab.Update(tabKey("esc"), c)
	if tab.Capturing() || len(tab.rows()) != all {
		t.Errorf("esc did not clear the filter: %d of %d rows", len(tab.rows()), all)
	}
}

func TestAWSServiceFilterCycles(t *testing.T) {
	c, _, tab := awsOpen(t)
	var seen []string
	for range 3 {
		tab.Update(tabKey("s"), c)
		seen = append(seen, tab.svc)
		for _, it := range tab.rows() {
			if it.Service != tab.svc {
				t.Fatalf("%s shows a %s", tab.svc, it.Service)
			}
		}
	}
	if !slices.Equal(seen, []string{"apigateway", "cloudfront", "cognito"}) {
		t.Errorf("s went %q", seen)
	}
	tab.Update(tabKey("S"), c)
	if tab.svc != "cloudfront" {
		t.Errorf("S went back to %q", tab.svc)
	}
	if v := ansi.Strip(tab.View(c, 120, 40)); !strings.Contains(v, "Resources 2 of 47") {
		t.Errorf("the count does not say the filter:\n%s", v)
	}
	tab.Update(tabKey("esc"), c)
	if tab.svc != "" {
		t.Errorf("esc kept the service %q", tab.svc)
	}
}

// r reads again at once, past fleetd's cache; while a read is on its way the
// tab says so.
func TestAWSRefresh(t *testing.T) {
	c, f, tab := awsOpen(t)
	cmd := tab.Update(tabKey("r"), c)
	if !tab.flying {
		t.Fatal("r sent nothing")
	}
	if v := ansi.Strip(tab.View(c, 120, 40)); !strings.Contains(v, "reading") {
		t.Errorf("no sign of the read in flight:\n%s", v)
	}
	tabFeed(tab, c, tabRun(cmd))
	if got := f.Calls; !slices.Equal(got, []string{"aws refresh=false", "aws refresh=true"}) {
		t.Errorf("calls %q", got)
	}
	if tab.flying {
		t.Error("still reading after the answer")
	}
}

// The tab reads when first shown, then every 5 minutes while shown; not on
// every message.
func TestAWSReadsWhenShownAndEveryFiveMinutes(t *testing.T) {
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	tab := newAWS()
	tabSettle(tab, c, 120, 40)
	tabFeed(tab, c, tabRun(tab.Update(SnapshotMsg{Snap: c.Snap}, c)))
	c.Now = c.Now.Add(4 * time.Minute)
	tabFeed(tab, c, tabRun(tab.Update(FrameMsg{}, c)))
	if len(f.Calls) != 1 {
		t.Fatalf("calls %q, want one read", f.Calls)
	}
	c.Now = c.Now.Add(time.Minute)
	tabFeed(tab, c, tabRun(tab.Update(SnapshotMsg{Snap: c.Snap}, c)))
	if !slices.Equal(f.Calls, []string{"aws refresh=false", "aws refresh=false"}) {
		t.Errorf("calls %q, want a second read after 5m", f.Calls)
	}
}

// An answer that comes while another tab is shown is not lost: the AWS tab
// has it when it is shown again.
func TestAWSKeepsAnAnswerThatCameWhileHidden(t *testing.T) {
	a, f := testApp(t, fleetapi.RoleAdmin, 120, 40, false)
	snap(t, a, f)
	cmd := showTab(t, a, "AWS")
	showTab(t, a, "Fleet")
	do(a, cmd) // the answer lands on the Fleet tab
	showTab(t, a, "AWS")
	if tab := a.current().(*awsTab); tab.rep == nil || tab.flying {
		t.Errorf("the answer was lost: rep %v, flying %v", tab.rep != nil, tab.flying)
	}
}

// fleetd without aws: in fleet.yaml says so, and the tab shows it.
type noAWS struct{ *fleetapi.Fake }

func (noAWS) AWS(context.Context, bool) (*fleetapi.AWSReport, error) {
	return nil, errors.New("404: aws is not set up in fleet.yaml (aws: keys_csv: …)")
}

func TestAWSNotSetUpIsShown(t *testing.T) {
	c, f := tabCtx(t, fleetapi.RoleAdmin)
	c.Client = noAWS{f}
	tab := newAWS()
	tabSettle(tab, c, 120, 40)
	if v := ansi.Strip(tab.View(c, 120, 40)); !strings.Contains(v, "aws is not set up in fleet.yaml") {
		t.Errorf("view:\n%s", v)
	}
}

func TestAWSIsForAdmins(t *testing.T) {
	for _, role := range []fleetapi.Role{fleetapi.RoleAdmin, fleetapi.RoleGuest} {
		a, f := testApp(t, role, 120, 40, false)
		snap(t, a, f)
		var titles []string
		for _, tb := range a.visible() {
			titles = append(titles, tb.Title())
		}
		i := slices.Index(titles, "AWS")
		switch {
		case role == fleetapi.RoleGuest && i >= 0:
			t.Errorf("a guest has the AWS tab: %v", titles)
		case role == fleetapi.RoleAdmin && (i < 1 || titles[i-1] != "Quota"):
			t.Errorf("an admin's AWS tab is not after Quota: %v", titles)
		}
		if role == fleetapi.RoleGuest {
			for k := 1; k <= 9; k++ {
				a.Update(keyMsg(fmt.Sprint(k)))
				if a.current().Title() == "AWS" {
					t.Errorf("%d took a guest to AWS", k)
				}
			}
			if slices.Contains(f.Calls, "aws refresh=false") {
				t.Error("a guest's TUI read AWS")
			}
		}
	}
}

func TestAWSKeepsItsSize(t *testing.T) {
	sizes := [][2]int{{0, 0}, {1, 1}, {20, 5}, {40, 12}, {61, 9}, {80, 24}, {100, 30}, {120, 40}, {200, 60}}
	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		c, _ := tabCtx(t, fleetapi.RoleAdmin)
		tab := newAWS()
		tabSize(t, "AWS before a read", tab.View(c, w, h), w, h)
		tabSettle(tab, c, w, h)
		for _, k := range []string{"e", "s", "down", "down", "/", "x", "enter", "up", "S"} {
			tab.Update(tabKey(k), c)
		}
		tabSize(t, "AWS", tab.View(c, w, h), w, h)
	}
}

// showTab shows the visible tab with this title, by its number.
func showTab(t *testing.T, a *App, title string) tea.Cmd {
	t.Helper()
	for i, tb := range a.visible() {
		if tb.Title() == title {
			_, cmd := a.Update(keyMsg(fmt.Sprint(i + 1)))
			return cmd
		}
	}
	t.Fatalf("no %s tab", title)
	return nil
}
