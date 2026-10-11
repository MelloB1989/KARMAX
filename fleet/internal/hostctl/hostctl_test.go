package hostctl

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
)

func cfgFor(t *testing.T, extra string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte("home: /home/op\nimage: karmax-fleet:9\nuid: 1000\ngid: 1001\nuser: op\n" + extra +
		"hosts: {kali: {exec: [docker]}, pc2: {exec: [docker, --context, pc2]}}\norchestrator: {host: kali, token_env: O}\nagents: {agent-01: {host: pc2, token_env: T}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The identity of the image comes from fleet.yaml, never from typed args: an
// image whose home is not the fleet's strands every agent in the wizard.
func TestBuildArgvTakesTheImageIdentityFromFleetYAML(t *testing.T) {
	argv, err := BuildArgv(cfgFor(t, "claude_version: 2.1.300\n"), "/src", "")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	want := "docker build -f /src/packaging/fleet/Dockerfile -t karmax-fleet:9 --build-arg UID=1000 --build-arg GID=1001 " +
		"--build-arg USER_NAME=op --build-arg HOME_DIR=/home/op --build-arg CLAUDE_VERSION=2.1.300 /src"
	if got != want {
		t.Errorf("argv =\n%s\nwant\n%s", got, want)
	}
	argv, _ = BuildArgv(cfgFor(t, ""), "/src", "other:1")
	got = strings.Join(argv, " ")
	if !strings.Contains(got, "-t other:1") || strings.Contains(got, "CLAUDE_VERSION") {
		t.Errorf("--tag, and no version when fleet.yaml has none (the Dockerfile's pin): %s", got)
	}
}

func TestBuildRefusesAHomeItCannotUse(t *testing.T) {
	c := cfgFor(t, "")
	c.Home = "relative"
	if _, err := BuildArgv(c, "/src", ""); err == nil {
		t.Error("a relative home must be refused")
	}
	c.Home = "/"
	if _, err := BuildArgv(c, "/src", ""); err == nil {
		t.Error("/ must be refused")
	}
}

func TestImageBuildEchoesTheCommandBeforeRunningIt(t *testing.T) {
	var out bytes.Buffer
	var ran string
	err := ImageBuild(context.Background(), cfgFor(t, ""), "/src", "", &out, func(_ context.Context, argv []string, w io.Writer) error {
		ran = strings.Join(argv, " ")
		if !strings.HasPrefix(out.String(), "$ "+ran) {
			t.Errorf("command not printed before it ran: %q", out.String())
		}
		return nil
	})
	if err != nil || !strings.Contains(ran, "HOME_DIR=/home/op") {
		t.Fatalf("ran %q, %v", ran, err)
	}
}

func TestImageShipPipesSaveIntoTheHostsOwnEngine(t *testing.T) {
	var out bytes.Buffer
	var from, to []string
	err := ImageShip(context.Background(), cfgFor(t, ""), "pc2", "", &out, func(_ context.Context, f, t []string, _ io.Writer) error {
		from, to = f, t
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(from, " ") != "docker save karmax-fleet:9" || strings.Join(to, " ") != "docker --context pc2 load" {
		t.Errorf("save %v | load %v", from, to)
	}
	if out.String() != "$ docker save karmax-fleet:9 | docker --context pc2 load\n" {
		t.Errorf("printed %q", out.String())
	}
	if err := ImageShip(context.Background(), cfgFor(t, ""), "mars", "", &out, nil); err == nil {
		t.Error("an unknown host must be refused")
	}
}

func TestExecPiperConnectsTheTwoCommandsAndNamesTheOneThatFailed(t *testing.T) {
	var out bytes.Buffer
	if err := ExecPiper(context.Background(), []string{"echo", "hello"}, []string{"cat"}, &out); err != nil || out.String() != "hello\n" {
		t.Fatalf("out %q, %v", out.String(), err)
	}
	err := ExecPiper(context.Background(), []string{"false"}, []string{"cat"}, &out)
	if err == nil || !strings.Contains(err.Error(), "false") {
		t.Errorf("err = %v", err)
	}
}

func TestFindSourceWalksUpToTheDockerfile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "packaging/fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, Dockerfile), nil, 0o644)
	deep := filepath.Join(root, "fleet/cmd")
	_ = os.MkdirAll(deep, 0o755)
	if got, err := FindSource(deep); err != nil || got != root {
		t.Fatalf("FindSource = %q, %v", got, err)
	}
	if _, err := FindSource(t.TempDir()); err == nil {
		t.Fatal("no Dockerfile above must be an error")
	}
}

func TestExecRunnerDoesNotHangOnGrandchild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var b bytes.Buffer
		done <- ExecRunner(ctx, []string{"sh", "-c", "sleep 30 & wait"}, &b)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the killed command")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecRunner still blocked 10s after its context ended")
	}
}

// Both commands write their stderr to the one writer, concurrently.
func TestExecPiperBothCommandsMayWriteStderrAtOnce(t *testing.T) {
	var out bytes.Buffer
	sh := func(script string) []string { return []string{"sh", "-c", script} }
	loud := "i=0; while [ $i -lt 200 ]; do echo err >&2; i=$((i+1)); done"
	if err := ExecPiper(context.Background(), sh(loud+"; echo data"), sh(loud+"; cat"), &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "err\n"); got != 400 || !strings.Contains(out.String(), "data\n") {
		t.Errorf("err lines %d, out has data: %v", got, strings.Contains(out.String(), "data"))
	}
}
