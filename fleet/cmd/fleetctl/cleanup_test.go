package main

import (
	"strings"
	"testing"
)

const cleanupReply = `{"at":"2026-10-31T10:00:00Z","dry_run":true,"stale":"7d","freed_bytes":3221225472,` +
	`"items":[{"where":"agent-03","path":"/work/wt/old","action":"would_delete","bytes":2147483648,"why":"unused"},` +
	`{"where":"agent-03","path":"/work/wt/mine","action":"kept","bytes":1048576,"why":"2 commits no remote has in /work/wt/mine"},` +
	`{"where":"code","path":"/home/op/code/.worktrees/agent-03/jdk","action":"would_delete","bytes":1073741824,"why":"unused"}],` +
	`"skipped":[{"where":"agent-05","why":"agent-05:2 has task T9"}]}`

func TestCleanupSaysWhatWentWhatStayedAndWhereItDidNotLook(t *testing.T) {
	f := &fakeFleetd{reply: map[string]string{"/v1/cleanup": cleanupReply}}
	out, err := fleetctl(t, f, "cleanup", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if f.reqs[0] != `POST /v1/cleanup {"dry_run":true}` {
		t.Fatalf("request = %q", f.reqs[0])
	}
	lines := strings.Split(out, "\n")
	if strings.Join(strings.Fields(lines[0]), " ") != "WHERE ACTION SIZE PATH WHY" {
		t.Fatalf("output:\n%s", out)
	}
	for _, want := range []string{"would delete", "2.0 GiB", "/work/wt/old", "kept", "1.0 MiB", "2 commits no remote has",
		"code", "/home/op/code/.worktrees/agent-03/jdk", "Not looked at:", "agent-05", "agent-05:2 has task T9",
		"Would free 3.0 GiB (dry run: nothing deleted). Unused for 7d, an item goes."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := fleetctl(t, f, "cleanup", "extra"); err == nil {
		t.Error("cleanup took an argument")
	}
}
