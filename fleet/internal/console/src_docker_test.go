package console

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDockerStreamUpdatesOnEachLine(t *testing.T) {
	pr, pw := io.Pipe()
	sp := func(ctx context.Context, argv []string) (io.ReadCloser, error) {
		if strings.Join(argv, " ") != "docker stats --format {{json .}}" {
			t.Errorf("argv = %q", argv)
		}
		return pr, nil
	}
	clock := time.Unix(1000, 0)
	var mu = make(chan struct{}, 8)
	d := newDockerSrc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx, sp, []string{"docker"}, func() time.Time { return clock }, func() { mu <- struct{}{} })
	<-mu // started

	pw.Write([]byte("\x1b[2J\x1b[H" + `{"Name":"agent-01","CPUPerc":"1.31%","MemUsage":"128.3MiB / 1.5GiB","NetIO":"2.38MB / 1.78MB"}` + "\n"))
	<-mu
	c := d.container("agent-01", clock)
	if c == nil || c.CPUPct != 1.31 || c.MemLimit != 1610612736 || !c.Running {
		t.Fatalf("first frame: %+v", c)
	}
	clock = clock.Add(time.Second)
	pw.Write([]byte(`{"Name":"agent-01","CPUPerc":"40.00%","MemUsage":"200MiB / 1.5GiB","NetIO":"3MB / 2MB"}` + "\n"))
	<-mu
	if c = d.container("agent-01", clock); c.CPUPct != 40 || c.NetRx != 3000000 {
		t.Fatalf("second frame: %+v", c)
	}
	if c := d.container("agent-01", clock.Add(time.Minute)); c.Running {
		t.Fatal("a container missing from the frames for a minute is not running")
	}
	if d.container("agent-09", clock) != nil {
		t.Fatal("unknown container must be nil")
	}
	if d.tr.stale(clock) {
		t.Fatal("running process is not stale")
	}
}

func TestDockerStatsLinesWithANSIParse(t *testing.T) {
	// Docker 29.9.0, exactly as streamed, and a Docker 28 plain line.
	d29 := "\x1b[H" + `{"BlockIO":"24.8MB / 1.18GB","CPUPerc":"0.30%","Container":"abc","ID":"abc","MemPerc":"1.2%","MemUsage":"128.3MiB / 1.5GiB","Name":"agent-01","NetIO":"2.38MB / 1.78MB","PIDs":"31"}` + "\x1b[K"
	d28 := `{"Name":"agent-02","CPUPerc":"1.31%","MemUsage":"64MiB / 1.5GiB","NetIO":"1MB / 2MB"}`
	d29next := `{"Name":"agent-03","CPUPerc":"5.00%","MemUsage":"10MiB / 1.5GiB","NetIO":"1MB / 2MB"}` + "\x1b[K" // later line of the frame: no ESC[H

	pr, pw := io.Pipe()
	sp := func(context.Context, []string) (io.ReadCloser, error) { return pr, nil }
	clock := time.Unix(1000, 0)
	ch := make(chan struct{}, 16)
	d := newDockerSrc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx, sp, []string{"docker"}, func() time.Time { return clock }, func() { ch <- struct{}{} })
	<-ch
	for _, l := range []string{d29, d28, d29next} {
		pw.Write([]byte(l + "\n"))
		<-ch
	}
	if c := d.container("agent-01", clock); c == nil || c.CPUPct != 0.3 || c.Mem == 0 || c.MemLimit != 1610612736 {
		t.Fatalf("docker 29 line: %+v", c)
	}
	if c := d.container("agent-02", clock); c == nil || c.CPUPct != 1.31 {
		t.Fatalf("docker 28 line: %+v", c)
	}
	if c := d.container("agent-03", clock); c == nil || c.CPUPct != 5 {
		t.Fatalf("trailing-ESC line: %+v", c)
	}
}

func TestDockerStreamThatParsesNothingIsStale(t *testing.T) {
	pr, pw := io.Pipe()
	sp := func(context.Context, []string) (io.ReadCloser, error) { return pr, nil }
	clock := time.Unix(1000, 0)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	ch := make(chan struct{}, 16)
	d := newDockerSrc()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx, sp, []string{"docker"}, now, func() { ch <- struct{}{} })
	<-ch // process is up, nothing parsed yet

	if src := d.tr.source(now()); !src.OK {
		t.Fatalf("a process that has just started is not stale: %+v", src)
	}
	// it keeps running and printing lines we cannot parse
	pw.Write([]byte("Error response from daemon: something unexpected\n"))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	clock = clock.Add(11 * time.Second) // more than 2 x CadenceDocker
	mu.Unlock()
	src := d.tr.source(now())
	if src.OK || !strings.Contains(src.Error, "no stats parsed for 11 s") {
		t.Fatalf("source = %+v, want not ok: no stats parsed for 11 s", src)
	}
	if !d.tr.stale(now()) {
		t.Fatal("stale() must agree")
	}
	// a parsed frame brings it back
	pw.Write([]byte(`{"Name":"agent-01","CPUPerc":"1.00%","MemUsage":"1MiB / 2MiB","NetIO":"1B / 1B"}` + "\n"))
	<-ch
	if src := d.tr.source(now()); !src.OK || src.Error != "" {
		t.Fatalf("after a parsed frame: %+v", src)
	}
}
