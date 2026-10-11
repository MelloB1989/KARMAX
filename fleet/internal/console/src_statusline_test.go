package console

import (
	"os"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/MelloB1989/karmax/fleet/internal/target"
)

func TestStatuslineUsesTheFilesMtime(t *testing.T) {
	fixture, err := os.ReadFile("testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}
	const mtime = 1791700000
	r := &fakeRunner{fn: func(argv []string) ([]byte, error) {
		return append([]byte("1791700000\n"), fixture...), nil
	}}
	cs := map[string]target.Container{"agent-01": {Exec: []string{"docker"}, Name: "agent-01", Runner: r}}
	s := newStatuslineSrc([]string{"agent-01"})
	fetched := time.Unix(mtime+3600, 0) // an hour after the file was written
	s.poll(context.Background(), cs, fetched)

	sl, ok := s.get("agent-01")
	if !ok {
		t.Fatal("no status line")
	}
	if !sl.At.Equal(time.Unix(mtime, 0)) {
		t.Fatalf("At = %v, want the file's mtime, not the fetch time", sl.At)
	}
	if sl.Model != "Sonnet 5.5" || sl.ContextPct != 8 || sl.FiveHourPct != 3 || sl.SevenDayPct != 11 || !sl.FiveHourResets.Equal(time.Unix(1791744000, 0)) {
		t.Fatalf("status line = %+v", sl)
	}
	if got := strings.Join(r.seen[0], " "); !strings.HasPrefix(got, "docker exec agent-01 sh -c") || !strings.Contains(got, "stat -c %Y") {
		t.Fatalf("argv = %q", got)
	}
	if src := s.source(fetched); !src.OK {
		t.Fatalf("source = %+v", src)
	}
}

func TestStatuslineFailureMarksOnlyThatAgent(t *testing.T) {
	r := &fakeRunner{fn: func(argv []string) ([]byte, error) {
		if argv[2] == "agent-02" {
			return nil, os.ErrNotExist
		}
		b, _ := os.ReadFile("testdata/status.json")
		return append([]byte("1791700000\n"), b...), nil
	}}
	cs := map[string]target.Container{}
	for _, n := range []string{"agent-01", "agent-02"} {
		cs[n] = target.Container{Exec: []string{"docker"}, Name: n, Runner: r}
	}
	s := newStatuslineSrc([]string{"agent-01", "agent-02"})
	now := time.Unix(1791700005, 0)
	s.poll(context.Background(), cs, now)
	if _, ok := s.get("agent-01"); !ok {
		t.Fatal("agent-01 lost its status line to agent-02's failure")
	}
	src := s.source(now)
	if src.OK || !strings.Contains(src.Error, "agent-02") {
		t.Fatalf("source = %+v", src)
	}
}
