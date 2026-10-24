package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func localProj(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(d, 0o755)
	if err := os.WriteFile(filepath.Join(d, "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRunAttachPlacesTheTaskThenSendsTheFilesThenTheBrief(t *testing.T) {
	s := &serve{out: map[string]string{
		"/v1/run":                   `{"task":"T-att","agent":"agent-03","why":"named"}`,
		"/v1/agents/agent-03/files": `{"ok":true,"bytes":10}`,
		"/v1/tasks/T-att/brief":     `{"ok":true}`,
		"/v1/done":                  `{"ok":true}`,
	}}
	if _, err := ctl(t, s, "", "run", "--attach", localProj(t), "--task", "T-att", "--agent", "agent-03", "read it"); err != nil {
		t.Fatal(err)
	}
	if len(s.reqs) != 3 || s.reqs[0] != "POST /v1/run" || !strings.HasPrefix(s.reqs[1], "PUT /v1/agents/agent-03/files?path=%2Fwork%2Finbox%2FT-att") || s.reqs[2] != "POST /v1/tasks/T-att/brief" {
		t.Fatalf("requests = %q", s.reqs)
	}
	if !strings.Contains(s.body[0], `"attach":true`) || strings.Contains(s.body[2], "attach") || !strings.Contains(s.body[2], `"brief":"read it"`) {
		t.Errorf("bodies = %q", s.body)
	}
	tr := tar.NewReader(strings.NewReader(s.body[1]))
	if h, err := tr.Next(); err != nil || h.Name != "proj/" {
		t.Errorf("the body is not the tar of proj: %+v, %v", h, err)
	}
}

func TestRunAttachThatFailsToUploadClosesTheTaskAndSendsNoBrief(t *testing.T) {
	s := &serve{
		out: map[string]string{
			"/v1/run":                   `{"task":"T-att","agent":"agent-03","why":"named"}`,
			"/v1/agents/agent-03/files": `{"error":"transfer is over the size limit (2 GiB)"}`,
			"/v1/done":                  `{"ok":true}`,
		},
		code: map[string]int{"/v1/agents/agent-03/files": 413},
	}
	_, err := ctl(t, s, "", "run", "--attach", localProj(t), "--task", "T-att", "--agent", "agent-03", "read it")
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("err = %v", err)
	}
	if last := s.reqs[len(s.reqs)-1]; last != "POST /v1/done" || !strings.Contains(s.body[len(s.body)-1], "undelivered") {
		t.Errorf("requests = %q", s.reqs)
	}
	for _, r := range s.reqs {
		if strings.Contains(r, "/brief") {
			t.Error("a brief was sent for files that did not arrive")
		}
	}
}

func TestRunAttachOfAMissingFileStopsBeforeATaskIsPlaced(t *testing.T) {
	s := &serve{}
	if _, err := ctl(t, s, "", "run", "--attach", "/nonexistent/nothing", "x"); err == nil {
		t.Fatal("no error")
	}
	if len(s.reqs) != 0 {
		t.Errorf("requests = %q", s.reqs)
	}
}

func TestCpUploadsAPackedTreeToTheDefaultInbox(t *testing.T) {
	s := &serve{out: map[string]string{"/v1/agents/agent-03/files": `{"ok":true,"bytes":2048}`}}
	out, err := ctl(t, s, "", "cp", localProj(t), "agent-03")
	if err != nil || !strings.Contains(out, "agent-03:/work/inbox") {
		t.Fatalf("%q, %v", out, err)
	}
	if s.reqs[0] != "PUT /v1/agents/agent-03/files?path=%2Fwork%2Finbox" {
		t.Errorf("request = %q", s.reqs[0])
	}
	if _, err := ctl(t, s, "", "cp", localProj(t), "agent-03:/etc"); err == nil || !strings.Contains(err.Error(), "/work or /tmp") {
		t.Errorf("a destination outside: %v", err)
	}
	if len(s.reqs) != 1 {
		t.Errorf("a refused destination reached fleetd: %q", s.reqs)
	}
}

func TestCpDownloadUnpacksIntoTheLocalDirectory(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "out/r.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: 2})
	_, _ = io.WriteString(tw, "ok")
	_ = tw.Close()
	s := &serve{out: map[string]string{"/v1/agents/agent-03/files": b.String()}}
	dir := t.TempDir()
	if _, err := ctl(t, s, "", "cp", "agent-03:/work/out", dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "out/r.txt")); string(got) != "ok" {
		t.Errorf("got %q", got)
	}
}

func TestCpDownloadRefusesAnArchiveThatEscapes(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1})
	_, _ = io.WriteString(tw, "x")
	_ = tw.Close()
	s := &serve{out: map[string]string{"/v1/agents/agent-03/files": b.String()}}
	dir := filepath.Join(t.TempDir(), "dest")
	if _, err := ctl(t, s, "", "cp", "agent-03:/work/out", dir); err == nil {
		t.Fatal("an escaping archive was unpacked")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "evil.txt")); err == nil {
		t.Error("a file landed outside the directory")
	}
}

func TestSyncBuildsAnRsyncThroughRsh(t *testing.T) {
	got, err := rsyncArgv("/usr/bin/fleetctl", "./site/", "agent-03:/work/site", true, false, []string{".git"})
	want := `rsync -a -e /usr/bin/fleetctl rsh --delete --exclude .git ./site/ agent-03:/work/site`
	if err != nil || strings.Join(got, " ") != want {
		t.Fatalf("%q, %v", got, err)
	}
	back, err := rsyncArgv("/opt/my tools/fleetctl", "agent-03:/tmp/out/", "./out", false, true, nil)
	if err != nil || back[3] != `"/opt/my tools/fleetctl" rsh` || back[len(back)-2] != "agent-03:/tmp/out/" {
		t.Fatalf("%q, %v", back, err)
	}
	for _, c := range [][2]string{{"./a", "agent-03:/etc"}, {"agent-03:/home/op", "./a"}, {"./a", "./b"}, {"agent-01:/work/a", "agent-03:/work/b"}} {
		if got, err := rsyncArgv("fleetctl", c[0], c[1], false, false, nil); err == nil {
			t.Errorf("%v allowed: %q", c, got)
		}
	}
}

func TestAWorkerIsNamedAgentColonSlot(t *testing.T) {
	for in, want := range map[string][3]string{
		"agent-03":           {"", "", ""},
		"agent-03:/work/x":   {"agent-03", "/work/x", "y"},
		"agent-03:2":         {"agent-03:2", "", "y"},
		"agent-03:2:/work/x": {"agent-03:2", "/work/x", "y"},
		"agent-03:2025.txt":  {"agent-03", "2025.txt", "y"},
		"C:\\dir":            {"", "", ""},
	} {
		w, p, ok := parseRemote(in)
		if (want[2] == "y") != ok || w != want[0] || p != want[1] {
			t.Errorf("%q = %q %q %v", in, w, p, ok)
		}
	}
	got, err := rsyncArgv("fleetctl", "./a/", "agent-03:2:/work/a/", false, false, nil)
	if err != nil || got[len(got)-1] != "agent-03+2:/work/a/" {
		t.Fatalf("%q, %v", got, err)
	}
	if rshWorker("agent-03+2") != "agent-03:2" || rshWorker("agent-03") != "agent-03" {
		t.Error("rsh does not turn the host back into a worker")
	}
}
