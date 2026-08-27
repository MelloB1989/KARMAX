package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteEnvFilePermissionsAndContent(t *testing.T) {
	path, err := writeEnvFile(map[string]string{"GITHUB_TOKEN": "ghs_secret", "TASK": "do the thing"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	defer os.Remove(path)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file perm = %o, want 0600", perm)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, "GITHUB_TOKEN=ghs_secret") {
		t.Errorf("env file missing GITHUB_TOKEN line, got: %q", content)
	}
	if !strings.Contains(content, "TASK=do the thing") {
		t.Errorf("env file missing TASK line, got: %q", content)
	}
}

func TestWriteEnvFileRejectsNewlines(t *testing.T) {
	if _, err := writeEnvFile(map[string]string{"X": "line1\nFAKE_VAR=injected"}); err == nil {
		t.Fatal("expected an error for a newline-carrying value, got nil")
	}
}

func TestRunArgsCarriesOnlyTheEnvFilePath(t *testing.T) {
	args := runArgs("acme/img:latest", "/tmp/karmax-sandbox-env-xyz", "run-1", "2g", "2")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--env-file /tmp/karmax-sandbox-env-xyz") {
		t.Fatalf("expected --env-file pointing at the env file, got: %v", args)
	}
	if !strings.Contains(joined, "karmax.sandbox=1") || !strings.Contains(joined, "karmax.run-id=run-1") {
		t.Fatalf("expected findability labels, got: %v", args)
	}
	if !strings.Contains(joined, "--rm=false") {
		t.Fatalf("expected --rm=false so the exit code survives, got: %v", args)
	}
}

// fakeDocker installs a shell script named `docker` on PATH for the duration
// of the test and points it at t's temp dir, so Launch/Poll/Logs/Kill run
// their real exec.Command calls against something other than a live daemon.
func fakeDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestLaunchArgvExcludesSecretsButEnvFileCarriesThem(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv")
	envCopyFile := filepath.Join(t.TempDir(), "envcopy")
	t.Setenv("KARMAX_TEST_ARGV_FILE", argvFile)
	t.Setenv("KARMAX_TEST_ENVFILE_COPY", envCopyFile)

	// Records its own argv, and — before Launch's defer deletes it — copies
	// out the --env-file's content so the test can check what actually went
	// into the file versus what went into argv.
	fakeDocker(t, `
printf '%s\n' "$@" > "$KARMAX_TEST_ARGV_FILE"
prev=""
for a in "$@"; do
  if [ "$prev" = "--env-file" ]; then
    cp "$a" "$KARMAX_TEST_ENVFILE_COPY"
  fi
  prev="$a"
done
echo deadbeefcontainerid
exit 0
`)

	d := &DockerDriver{MemoryLimit: "1g", CPULimit: "1"}
	id, err := d.Launch(context.Background(), Spec{
		Image:  "acme/img:latest",
		Repo:   "acme/api",
		Branch: "main",
		Task:   "implement the thing",
		Env: map[string]string{
			"GITHUB_TOKEN":            "ghs_supersecrettoken",
			"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-supersecret",
		},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if id != "deadbeefcontainerid" {
		t.Errorf("Launch id = %q, want deadbeefcontainerid", id)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	for _, secret := range []string{"ghs_supersecrettoken", "sk-ant-oat-supersecret"} {
		if strings.Contains(string(argv), secret) {
			t.Errorf("secret %q leaked into docker argv: %q", secret, argv)
		}
	}

	envCopy, err := os.ReadFile(envCopyFile)
	if err != nil {
		t.Fatalf("read env file copy: %v", err)
	}
	for _, want := range []string{"GITHUB_TOKEN=ghs_supersecrettoken", "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-supersecret", "REPO=acme/api", "BASE_BRANCH=main", "TASK=implement the thing"} {
		if !strings.Contains(string(envCopy), want) {
			t.Errorf("env file missing %q, got: %q", want, envCopy)
		}
	}

	// Launch's own defer should have removed the env file it built.
	if entries, _ := os.ReadDir(os.TempDir()); entries != nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "karmax-sandbox-env-") {
				t.Errorf("Launch left an env file behind: %s", e.Name())
			}
		}
	}
}

func TestPollMapsGoneWhenDockerHasNeverHeardOfIt(t *testing.T) {
	fakeDocker(t, `echo "Error: No such object: nope" >&2; exit 1`)
	d := &DockerDriver{MemoryLimit: "1g", CPULimit: "1"}
	st, err := d.Poll(context.Background(), "nope")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if st.State != StateGone {
		t.Errorf("State = %q, want %q", st.State, StateGone)
	}
}

func TestKillIsNotAnErrorWhenAlreadyGone(t *testing.T) {
	fakeDocker(t, `echo "Error response from daemon: No such container: nope" >&2; exit 1`)
	d := &DockerDriver{MemoryLimit: "1g", CPULimit: "1"}
	if err := d.Kill(context.Background(), "nope"); err != nil {
		t.Errorf("Kill on an already-gone container should not error, got: %v", err)
	}
}

func TestKillPropagatesRealErrors(t *testing.T) {
	fakeDocker(t, `echo "Error response from daemon: something is on fire" >&2; exit 1`)
	d := &DockerDriver{MemoryLimit: "1g", CPULimit: "1"}
	if err := d.Kill(context.Background(), "id"); err == nil {
		t.Error("expected a real docker failure to surface as an error")
	}
}

func TestStatusFromInspect(t *testing.T) {
	// Shaped to match `docker inspect`'s documented State object.
	cases := []struct {
		name string
		json string
		want string
		exit int
	}{
		{
			name: "starting",
			json: `[{"State":{"Status":"created","ExitCode":0,"OOMKilled":false}}]`,
			want: StateStarting,
		},
		{
			name: "running",
			json: `[{"State":{"Status":"running","ExitCode":0,"OOMKilled":false}}]`,
			want: StateRunning,
		},
		{
			name: "exited ok",
			json: `[{"State":{"Status":"exited","ExitCode":0,"OOMKilled":false}}]`,
			want: StateExited,
		},
		{
			name: "exited nonzero",
			json: `[{"State":{"Status":"exited","ExitCode":1,"OOMKilled":false}}]`,
			want: StateFailed,
			exit: 1,
		},
		{
			name: "oom killed",
			json: `[{"State":{"Status":"exited","ExitCode":137,"OOMKilled":true}}]`,
			want: StateFailed,
			exit: 137,
		},
		{
			name: "dead",
			json: `[{"State":{"Status":"dead","ExitCode":137,"OOMKilled":false}}]`,
			want: StateFailed,
			exit: 137,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeDocker(t, `cat <<'EOF'
`+c.json+`
EOF
`)
			d := &DockerDriver{MemoryLimit: "1g", CPULimit: "1"}
			st, err := d.Poll(context.Background(), "id")
			if err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if st.State != c.want {
				t.Errorf("State = %q, want %q", st.State, c.want)
			}
			if st.ExitCode != c.exit {
				t.Errorf("ExitCode = %d, want %d", st.ExitCode, c.exit)
			}
		})
	}
}

func TestOpenUnimplementedDrivers(t *testing.T) {
	for _, name := range []string{"ecs", "k8s"} {
		if _, err := Open(name); err == nil {
			t.Errorf("Open(%q) should error until it's built", name)
		}
	}
	if _, err := Open("carrier-pigeon"); err == nil {
		t.Error("Open of an unknown driver name should error")
	}
}

func TestNewDockerDriverErrorsClearlyWithoutDocker(t *testing.T) {
	if _, err := exec.LookPath("docker"); err == nil {
		t.Skip("docker is on PATH in this environment; nothing to assert")
	}
	_, err := NewDockerDriver()
	if err == nil {
		t.Fatal("expected an error when docker is not installed")
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("error should name docker as the problem, got: %v", err)
	}
}

// TestDockerDriverEndToEnd proves Launch/Poll/Logs/Kill against a real
// daemon. It needs Docker and network access to pull an image, so it's opt-in.
func TestDockerDriverEndToEnd(t *testing.T) {
	if os.Getenv("KARMAX_SANDBOX_DOCKER_TESTS") == "" {
		t.Skip("set KARMAX_SANDBOX_DOCKER_TESTS=1 to run this against a real docker daemon")
	}
	d, err := NewDockerDriver()
	if err != nil {
		t.Fatalf("NewDockerDriver: %v", err)
	}
	ctx := context.Background()
	id, err := d.Launch(ctx, Spec{
		Image:  "hello-world",
		Repo:   "n/a",
		Branch: "n/a",
		Task:   "n/a",
		Env:    map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = d.Kill(ctx, id) })

	deadline := 30
	var st Status
	for i := 0; i < deadline; i++ {
		st, err = d.Poll(ctx, id)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if st.State == StateExited || st.State == StateFailed {
			break
		}
	}
	if st.State != StateExited {
		t.Fatalf("container ended in state %q, want %q", st.State, StateExited)
	}
	if st.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", st.ExitCode)
	}

	logs, err := d.Logs(ctx, id, 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "Hello from Docker") {
		t.Errorf("logs missing the hello-world banner, got: %q", logs)
	}

	if err := d.Kill(ctx, id); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	st, err = d.Poll(ctx, id)
	if err != nil {
		t.Fatalf("Poll after Kill: %v", err)
	}
	if st.State != StateGone {
		t.Errorf("State after Kill = %q, want %q", st.State, StateGone)
	}
}
