package console

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ev(t int64, event, tool string) []byte {
	if tool == "" {
		return []byte(fmt.Sprintf(`{"t":%d,"event":%q,"session":"s1"}`, t, event))
	}
	return []byte(fmt.Sprintf(`{"t":%d,"event":%q,"session":"s1","tool":%q}`, t, event, tool))
}

func TestPreThenPostSetsAndClearsCurrentTool(t *testing.T) {
	e := newEventsSrc([]string{"agent-01"})
	now := time.Unix(2000, 0)
	e.handle("agent-01", ev(1990, "PreToolUse", "Bash"), now)
	tool, last := e.tool("agent-01", "s1")
	if tool == nil || tool.Name != "Bash" || !tool.Since.Equal(time.Unix(1990, 0)) || !last.Equal(time.Unix(1990, 0)) {
		t.Fatalf("after Pre: %+v %v", tool, last)
	}
	e.handle("agent-01", ev(1995, "PostToolUse", "Read"), now) // a different tool finishing does not clear it
	if tool, _ := e.tool("agent-01", "s1"); tool == nil {
		t.Fatal("a Post for another tool cleared the current one")
	}
	e.handle("agent-01", ev(1996, "PostToolUse", "Bash"), now)
	if tool, _ := e.tool("agent-01", "s1"); tool != nil {
		t.Fatalf("after Post: %+v", tool)
	}
	e.handle("agent-01", ev(1997, "PreToolUse", "Edit"), now)
	e.handle("agent-01", ev(1998, "Stop", ""), now)
	if tool, _ := e.tool("agent-01", "s1"); tool != nil {
		t.Fatalf("Stop must clear the tool: %+v", tool)
	}
}

func TestToolEventsStayOutOfTheStream(t *testing.T) {
	e := newEventsSrc([]string{"agent-01"})
	now := time.Unix(2000, 0)
	for _, k := range []string{"PreToolUse", "PostToolUse", "tool_result"} {
		e.handle("agent-01", ev(1990, k, "Bash"), now)
	}
	e.handle("agent-01", ev(1991, "UserPromptSubmit", ""), now)
	e.handle("agent-01", ev(1992, "Stop", ""), now)
	live := e.liveEvents(nil)
	if len(live) != 2 || live[0].Kind != "UserPromptSubmit" || live[1].Kind != "Stop" || live[0].ID != 0 {
		t.Fatalf("live = %+v", live)
	}
	// ...but every event counts toward activity.
	var n int
	for _, b := range e.buckets("agent-01", now) {
		n += b.N
	}
	if n != 5 {
		t.Fatalf("activity counted %d events, want 5", n)
	}
	if !IsStreamKind("fleet.relay") || IsStreamKind("PreToolUse") || IsStreamKind("fleet") {
		t.Fatal("IsStreamKind")
	}
}

func TestActivityBucketsAre15MinutesAndWindowed(t *testing.T) {
	e := newEventsSrc([]string{"agent-01"})
	now := time.Unix(100*86400, 0)
	e.handle("agent-01", ev(now.Unix()-10, "Stop", ""), now)
	e.handle("agent-01", ev(now.Unix()-20, "Stop", ""), now)
	e.handle("agent-01", ev(now.Unix()-25*3600, "Stop", ""), now) // outside 24 h
	b := e.buckets("agent-01", now)
	if len(b) != 1 || b[0].N != 2 || b[0].T.Unix()%900 != 0 {
		t.Fatalf("buckets = %+v", b)
	}
}

// fakeDocker is a `docker` that runs the command after `exec <container>`,
// so a native tail stands in for one inside a container.
func fakeDocker(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n[ \"$1\" = exec ] || exit 2\nshift\nwhile [ \"${1#-}\" != \"$1\" ]; do shift; done\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTailSurvivesTheFileBeingMoved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "fleet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	e := newEventsSrc([]string{"agent-01"})
	changes := make(chan struct{}, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.run(ctx, execSpawn, []string{fakeDocker(t)}, "agent-01", "agent-01", "", time.Now, func() { changes <- struct{}{} })

	// wait until tail is up before the first line, so -n0 does not skip it
	for deadline := time.Now().Add(5 * time.Second); e.trs["agent-01"].stale(time.Now()); {
		if time.Now().After(deadline) {
			t.Fatal("tail never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	app := func(name string, lines ...string) {
		fh, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		fh.WriteString(strings.Join(lines, "\n") + "\n")
	}
	now := time.Now().Unix()
	app(f, string(ev(now, "UserPromptSubmit", "")), string(ev(now+1, "Stop", "")))
	waitLive(t, e, 2)

	// fleetd's drain: move the file aside, read it, delete it; the hook recreates the log.
	if err := os.Rename(f, filepath.Join(dir, "events.taking")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	app(f, string(ev(now+2, "SessionStart", "")), string(ev(now+3, "Notification", "")))
	os.Remove(filepath.Join(dir, "events.taking"))
	waitLive(t, e, 4)

	time.Sleep(300 * time.Millisecond) // and nothing arrives twice
	live := e.liveEvents(nil)
	var kinds []string
	for _, l := range live {
		kinds = append(kinds, l.Kind)
	}
	if got := strings.Join(kinds, ","); got != "UserPromptSubmit,Stop,SessionStart,Notification" {
		t.Fatalf("kinds = %s", got)
	}
}

func waitLive(t *testing.T, e *eventsSrc, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(e.liveEvents(nil)) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("want %d live events, have %d", n, len(e.liveEvents(nil)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
