package fleetapi

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// The demo's desk: four sessions on a made-up machine, seeded on first use
// (NewFake leaves it nil; ending every session leaves it empty).
const (
	fakeDeskHost = "devbox"
	fakeHome     = "/home/op"
)

// fakeDirs is the demo machine's directories, for the launch form.
var fakeDirs = []string{
	"/home", fakeHome, fakeHome + "/code", fakeHome + "/scratch",
	fakeHome + "/code/api", fakeHome + "/code/api/cmd", fakeHome + "/code/api/internal",
	fakeHome + "/code/infra", fakeHome + "/code/infra/modules",
	fakeHome + "/code/notes",
	fakeHome + "/code/web", fakeHome + "/code/web/public", fakeHome + "/code/web/src",
}

func (f *Fake) seedDesk() {
	if f.desk != nil {
		return
	}
	t := f.Now()
	f.desk = []DeskSession{
		{Name: "api", Dir: fakeHome + "/code/api", Command: "claude --model opus", Order: 0, Created: t.Add(-2*time.Hour - 10*time.Minute),
			Running: true, Busy: true, Clients: 1, Claude: "4f1c2a9e-7b3d-4e8a-9c61-2d5f0a8b7e13"},
		{Name: "web", Dir: fakeHome + "/code/web", Command: "claude", Order: 1, Created: t.Add(-26 * time.Hour),
			Running: true, Restored: t.Add(-3 * time.Minute), Claude: "9a2e6c41-0d8f-4b17-a3e5-6c9b1f2d4a70"},
		{Name: "infra", Dir: fakeHome + "/code/infra", Command: "shell", Order: 2, Created: t.Add(-45 * time.Minute), Running: true},
		{Name: "notes", Dir: fakeHome + "/code/notes", Command: "claude --model sonnet", Order: 3, Created: t.Add(-4 * time.Hour),
			Running: true, Claude: "c37d0b85-5e2a-4f96-8b1c-e04a7d63f9b2"},
	}
}

func (f *Fake) Desk(ctx context.Context, lines int) (*DeskList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return nil, fmt.Errorf("403: the desk is for admins")
	}
	f.seedDesk()
	out := &DeskList{Host: fakeDeskHost, Sessions: []DeskSession{}}
	for _, s := range f.desk {
		if s.Running {
			body := strings.Split(fakeDeskScreen(s, f.step), "\n")
			if lines > 0 && len(body) > lines {
				body = body[len(body)-lines:]
			}
			s.Text, s.Cols, s.At = strings.Join(body, "\n"), 80, f.Now()
		}
		out.Sessions = append(out.Sessions, s)
	}
	return out, nil
}

func (f *Fake) DeskCreate(ctx context.Context, c DeskCreate) (*DeskSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return nil, fmt.Errorf("403: the desk is for admins")
	}
	f.seedDesk()
	dir := fakeDir(c.Dir)
	if len(dir) > 1 {
		dir = strings.TrimSuffix(dir, "/")
	}
	if !slices.Contains(fakeDirs, dir) {
		return nil, fmt.Errorf("400: no directory %s on %s", c.Dir, fakeDeskHost)
	}
	taken := func(n string) bool {
		return slices.ContainsFunc(f.desk, func(s DeskSession) bool { return s.Name == n })
	}
	name := c.Name
	switch {
	case name != "" && taken(name):
		return nil, fmt.Errorf("409: a session named %s is already on the desk", name)
	case name == "":
		name = path.Base(dir)
		for i := 2; taken(name); i++ {
			name = fmt.Sprintf("%s-%d", path.Base(dir), i)
		}
	}
	cmd := c.Command
	if cmd == "" {
		cmd = "claude"
	}
	order := 0
	for _, s := range f.desk {
		order = max(order, s.Order+1)
	}
	s := DeskSession{Name: name, Dir: dir, Command: cmd, Order: order, Created: f.Now(), Running: true}
	f.desk = append(f.desk, s)
	f.record("desk new name=%q dir=%q command=%q", c.Name, c.Dir, c.Command)
	return &s, nil
}

func (f *Fake) DeskEnd(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return fmt.Errorf("403: the desk is for admins")
	}
	f.seedDesk()
	for i, s := range f.desk {
		if s.Name == name {
			f.desk = append(f.desk[:i], f.desk[i+1:]...)
			f.record("desk end %s", name)
			return nil
		}
	}
	return fmt.Errorf("404: no desk session %q", name)
}

// DeskDirs completes a directory as fleetd's desk does: nothing typed is
// home, the code and the sessions' directories; a directory is what is in
// it; anything else is the directories beside it that start so. ~ and a
// relative prefix are under the home directory.
func (f *Fake) DeskDirs(ctx context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Role != RoleAdmin {
		return nil, fmt.Errorf("403: the desk is for admins")
	}
	if strings.TrimSpace(prefix) == "" {
		f.seedDesk()
		out := []string{fakeHome, fakeHome + "/code"}
		for i := len(f.desk) - 1; i >= 0; i-- {
			if !slices.Contains(out, f.desk[i].Dir) {
				out = append(out, f.desk[i].Dir)
			}
		}
		return out, nil
	}
	p := path.Clean(fakeDir(prefix))
	parent, base := path.Dir(p), path.Base(p)
	if strings.HasSuffix(prefix, "/") || slices.Contains(fakeDirs, p) {
		parent, base = p, ""
	}
	out := []string{}
	for _, d := range fakeDirs {
		if path.Dir(d) == parent && strings.HasPrefix(strings.ToLower(path.Base(d)), strings.ToLower(base)) {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out, nil
}

// fakeDir is dir with ~ (and a relative dir) under the demo's home.
func fakeDir(dir string) string {
	switch {
	case dir == "" || dir == "~":
		return fakeHome + "/"
	case strings.HasPrefix(dir, "~/"):
		return fakeHome + dir[1:]
	case !strings.HasPrefix(dir, "/"):
		return fakeHome + "/" + dir
	}
	return dir
}

// ANSI for the demo's screens.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiOrange = "\x1b[38;5;209m"
	ansiGreen  = "\x1b[38;5;114m"
	ansiRed    = "\x1b[38;5;203m"
	ansiBlue   = "\x1b[38;5;111m"
	ansiViolet = "\x1b[38;5;141m"
)

// fakeDeskScreen is what a demo session's terminal shows: Claude Code at
// work or at rest, or a shell. The busy one's turn ticks on with step.
func fakeDeskScreen(s DeskSession, step int) string {
	tool := func(name, arg string) string {
		return ansiGreen + "⏺" + ansiReset + " " + ansiBold + name + ansiReset + "(" + arg + ")"
	}
	said := func(text string) string { return "⏺ " + text }
	out := func(text string) string { return ansiDim + "  ⎿  " + ansiReset + text }
	prompt := func(hint string) []string {
		return []string{
			ansiDim + "╭" + strings.Repeat("─", 76) + "╮" + ansiReset,
			ansiDim + "│" + ansiReset + " > " + strings.Repeat(" ", 73) + ansiDim + "│" + ansiReset,
			ansiDim + "╰" + strings.Repeat("─", 76) + "╯" + ansiReset,
			"  " + ansiDim + hint + ansiReset,
		}
	}
	welcome := []string{
		ansiOrange + "✻" + ansiReset + " " + ansiBold + "Welcome to Claude Code!" + ansiReset,
		"",
		"  " + ansiDim + "/help for help, /status for your current setup" + ansiReset,
		"",
		"  " + ansiDim + "cwd: " + s.Dir + ansiReset,
		"",
	}
	var lines []string
	switch s.Name {
	case "api":
		spin := []string{"✶", "✻", "✽", "✻"}[step%4]
		verb := []string{"Percolating", "Percolating", "Simmering", "Simmering"}[step%4]
		lines = append(welcome,
			"> add rate limiting to POST /v1/orders: 100 requests a minute per API key",
			"",
			said("I'll add a token-bucket limiter as middleware, keyed on the API key."),
			"",
			tool("Read", "internal/http/router.go"),
			out("Read 142 lines"),
			"",
			tool("Write", "internal/http/ratelimit.go"),
			out("Wrote 58 lines to internal/http/ratelimit.go"),
			"",
			tool("Bash", "go test ./internal/http/..."),
			out("Running…"),
			"",
			ansiOrange+spin+" "+verb+"…"+ansiReset+" "+ansiDim+fmt.Sprintf("(%ds · ↑ 2.1k tokens · esc to interrupt)", 47+2*step)+ansiReset,
			"",
		)
		lines = append(lines, prompt(ansiViolet+"⏵⏵ accept edits on"+ansiReset+ansiDim+" (shift+tab to cycle)")...)
	case "web":
		lines = append(welcome,
			"> /resume",
			out("Resumed: checkout page shows tax and shipping before pay"),
			"",
			said("Picking up where we left off: the checkout summary now shows tax"),
			"  and shipping before the pay button, and the tests pass.",
			"",
			"  Left: the empty-cart state. Shall I do that next?",
			"",
		)
		lines = append(lines, prompt("? for shortcuts")...)
	case "infra":
		ps1 := ansiGreen + "op@" + fakeDeskHost + ansiReset + ":" + ansiBlue + "~/code/infra" + ansiReset + "$ "
		lines = []string{
			ps1 + "terraform plan -out tf.plan",
			"aws_s3_bucket.assets: Refreshing state... [id=assets-example]",
			"aws_iam_role.deploy: Refreshing state... [id=deploy-example]",
			"",
			"Terraform will perform the following actions:",
			"",
			"  " + ansiBold + "# aws_s3_bucket_lifecycle_configuration.assets" + ansiReset + " will be created",
			"  " + ansiGreen + "+" + ansiReset + " resource \"aws_s3_bucket_lifecycle_configuration\" \"assets\" {",
			"      " + ansiGreen + "+" + ansiReset + " bucket = \"assets-example\"",
			"      " + ansiGreen + "+" + ansiReset + " id     = (known after apply)",
			"    }",
			"",
			ansiBold + "Plan:" + ansiReset + " 1 to add, 0 to change, 0 to destroy.",
			"",
			"Saved the plan to: tf.plan",
			"",
			ps1,
		}
	case "notes":
		lines = append(welcome,
			"> turn this week's incident notes into a short postmortem",
			"",
			tool("Read", "incidents/2026-10-*.md"),
			out("Read 4 files"),
			"",
			said("Wrote postmortem-2026-10.md (41 lines):"),
			"  • three incidents, all the queue worker's retry storm",
			"  • fixed: jittered backoff, at most 5 retries",
			"  • "+ansiRed+"open"+ansiReset+": alert when the queue is deeper than 10k",
			"",
		)
		lines = append(lines, prompt("? for shortcuts")...)
	default:
		ps1 := ansiGreen + "op@" + fakeDeskHost + ansiReset + ":" + ansiBlue + strings.Replace(s.Dir, fakeHome, "~", 1) + ansiReset + "$ "
		switch f := strings.Fields(s.Command); {
		case s.Command == "shell":
			return ps1
		case len(f) > 0 && f[0] != "claude":
			return ps1 + s.Command
		}
		lines = append(welcome, prompt("? for shortcuts")...)
	}
	return strings.Join(lines, "\n")
}
