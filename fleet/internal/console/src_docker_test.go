package console

import (
	"context"
	"io"
	"strings"
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
