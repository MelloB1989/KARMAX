package store

import "testing"

func TestSandboxRunTerminalStatusSetsFinishedAt(t *testing.T) {
	s := newTestStore(t)
	if err := s.StartSandboxRun(SandboxRun{ID: "r1", CaseID: "c1", Driver: "docker", Status: "starting"}); err != nil {
		t.Fatalf("start: %v", err)
	}

	got, ok, err := s.SandboxRun("r1")
	if err != nil || !ok || got.FinishedAt != nil {
		t.Fatalf("fresh run: %+v, %v, %v", got, ok, err)
	}

	// A non-terminal transition leaves finished_at null.
	if err := s.UpdateSandboxRun("r1", "running", 0, "", "building..."); err != nil {
		t.Fatalf("update to running: %v", err)
	}
	got, _, err = s.SandboxRun("r1")
	if err != nil || got.FinishedAt != nil || got.Status != "running" {
		t.Fatalf("running run: %+v, %v", got, err)
	}

	// A terminal transition sets it.
	if err := s.UpdateSandboxRun("r1", "exited", 0, "", "done"); err != nil {
		t.Fatalf("update to exited: %v", err)
	}
	got, _, err = s.SandboxRun("r1")
	if err != nil || got.FinishedAt == nil || got.Status != "exited" || got.LogTail != "done" {
		t.Fatalf("exited run: %+v, %v", got, err)
	}
}

func TestSandboxRunFailedAndGoneAreTerminal(t *testing.T) {
	s := newTestStore(t)
	for _, status := range []string{"failed", "gone"} {
		id := "r-" + status
		if err := s.StartSandboxRun(SandboxRun{ID: id, Status: "starting"}); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateSandboxRun(id, status, 1, "boom", "log"); err != nil {
			t.Fatalf("update %s: %v", status, err)
		}
		got, _, err := s.SandboxRun(id)
		if err != nil || got.FinishedAt == nil {
			t.Errorf("%s should be terminal: %+v, %v", status, got, err)
		}
		if got.ExitCode != 1 || got.Error != "boom" {
			t.Errorf("%s did not record exit code/error: %+v", status, got)
		}
	}
}

func TestLiveSandboxRuns(t *testing.T) {
	s := newTestStore(t)
	if err := s.StartSandboxRun(SandboxRun{ID: "starting", Status: "starting"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartSandboxRun(SandboxRun{ID: "running", Status: "starting"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSandboxRun("running", "running", 0, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StartSandboxRun(SandboxRun{ID: "done", Status: "starting"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSandboxRun("done", "exited", 0, "", ""); err != nil {
		t.Fatal(err)
	}

	live, err := s.LiveSandboxRuns()
	if err != nil || len(live) != 2 {
		t.Fatalf("live runs: %+v, %v", live, err)
	}
	ids := map[string]bool{}
	for _, r := range live {
		ids[r.ID] = true
	}
	if !ids["starting"] || !ids["running"] || ids["done"] {
		t.Errorf("wrong set of live runs: %+v", live)
	}
}

func TestListSandboxRunsByCase(t *testing.T) {
	s := newTestStore(t)
	if err := s.StartSandboxRun(SandboxRun{ID: "r1", CaseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartSandboxRun(SandboxRun{ID: "r2", CaseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.StartSandboxRun(SandboxRun{ID: "r3", CaseID: "c2"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListSandboxRuns("c1", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("ListSandboxRuns: %+v, %v", got, err)
	}
	for _, r := range got {
		if r.CaseID != "c1" {
			t.Errorf("a run from another case leaked in: %+v", r)
		}
	}
}
