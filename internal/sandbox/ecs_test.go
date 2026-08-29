package sandbox

import (
	"os"
	"strings"
	"testing"
)

// ECS has six words for "not running yet" and one for "stopped". Collapsing
// them here means the rest of KARMAX never learns what PROVISIONING means.
func TestECSStatesFoldOntoTheFourWeReport(t *testing.T) {
	for last, want := range map[string]string{
		"PROVISIONING":   StateStarting,
		"PENDING":        StateStarting,
		"ACTIVATING":     StateStarting,
		"RUNNING":        StateRunning,
		"DEACTIVATING":   StateExited,
		"STOPPING":       StateExited,
		"DEPROVISIONING": StateExited,
		"STOPPED":        StateExited,
		"running":        StateRunning,
		"":               StateStarting,
	} {
		if got := mapECSState(last); got != want {
			t.Errorf("%q -> %q, want %q", last, got, want)
		}
	}
}

// A missing driver config should say which variable is missing, not fail later
// inside Launch where it looks like the sandbox itself is broken.
func TestTheECSDriverNamesWhatItIsMissing(t *testing.T) {
	t.Setenv("KARMAX_SANDBOX_ECS_CLUSTER", "")
	t.Setenv("KARMAX_SANDBOX_ECS_TASKDEF", "")
	t.Setenv("KARMAX_SANDBOX_ECS_SUBNETS", "")

	_, err := NewECSDriver()
	if err == nil {
		t.Fatal("a driver with no configuration was returned")
	}
	for _, want := range []string{"CLUSTER", "TASKDEF", "SUBNETS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

func TestSubnetListsAreParsedForgivingly(t *testing.T) {
	for in, want := range map[string]int{
		"subnet-a":              1,
		"subnet-a,subnet-b":     2,
		" subnet-a , subnet-b ": 2,
		"subnet-a,,subnet-b,":   2,
		"":                      0,
		"   ":                   0,
	} {
		if got := len(splitList(in)); got != want {
			t.Errorf("%q -> %d entries, want %d", in, got, want)
		}
	}
}

// Tags reject most punctuation, and a repo name is full of it. A rejected tag
// fails the whole RunTask call, which would read as "the sandbox will not
// start" rather than "your branch name has a slash in it".
func TestTagValuesSurviveRepoAndBranchNames(t *testing.T) {
	for in, want := range map[string]string{
		"acme/routes-web":           "acme/routes-web",
		"feature/RTE-17-first-area": "feature/RTE-17-first-area",
		"":                          "none",
		"weird\"quote":              "weird-quote",
	} {
		if got := tagSafe(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if len(tagSafe(strings.Repeat("x", 400))) > 256 {
		t.Error("an over-long tag value was not truncated, and AWS would refuse the run")
	}
}

// The registry used to refuse ecs outright; a caller asking for it must now get
// a driver or a configuration error, never "not implemented".
func TestTheRegistryOffersECS(t *testing.T) {
	t.Setenv("KARMAX_SANDBOX_ECS_CLUSTER", "")
	_, err := Open("ecs")
	if err != nil && strings.Contains(err.Error(), "not implemented") {
		t.Errorf("ecs is still reported as unimplemented: %v", err)
	}
	if _, err := Open("k8s"); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("k8s should still say it is not implemented, got %v", err)
	}
}

// Both drivers hand the same entrypoint.sh its environment, and it exits on
// `REPO is required` before doing anything. A driver inventing its own
// spelling produces a container that starts, fails instantly, and reports
// nothing useful.
func TestBothDriversSpeakTheEntrypointsEnvNames(t *testing.T) {
	src, err := os.ReadFile("ecs.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`add("TASK"`, `add("REPO"`, `add("BASE_BRANCH"`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("the ECS driver does not set %s — entrypoint.sh would refuse the run", want)
		}
	}
	if strings.Contains(string(src), "SANDBOX_REPO") {
		t.Error("the ECS driver still uses its own env spelling")
	}
}
