package dashboards

import (
	"os"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/recipes"
)

func TestSaveWithRefreshEveryWritesARecipeThatLoads(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "Sales", HTMLSet: true, HTML: "<p></p>",
		Data:    map[string]any{"revenue": 1},
		Refresh: &Refresh{Every: "1h", Brief: "Pull yesterday's revenue from the ledger."}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if meta.Refresh == nil || meta.Refresh.Every != "1h" {
		t.Fatalf("meta.Refresh = %+v, want every=1h", meta.Refresh)
	}

	data, err := os.ReadFile(recipePath(meta.ID))
	if err != nil {
		t.Fatalf("recipe file was not written: %v", err)
	}
	r, err := recipes.Parse(recipePath(meta.ID), data)
	if err != nil {
		t.Fatalf("generated recipe does not load through recipes.Parse: %v\n---\n%s", err, data)
	}
	if r.On.Schedule != "@every 1h" {
		t.Fatalf("On.Schedule = %q, want %q", r.On.Schedule, "@every 1h")
	}
	if len(r.Steps) != 1 || r.Steps[0].Verb != recipes.VerbAsk {
		t.Fatalf("Steps = %+v, want exactly one ask step", r.Steps)
	}
	prompt := r.Steps[0].Text
	for _, want := range []string{"Sales", meta.ID, "Pull yesterday's revenue from the ledger.", "set_data", "revenue", "Do not change its HTML"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestSaveWithRefreshCronPassesTheCronThrough(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "Digest", HTMLSet: true, HTML: "<p></p>",
		Refresh: &Refresh{Cron: "0 0 9 * * *", Brief: "Summarize overnight activity."}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(recipePath(meta.ID))
	if err != nil {
		t.Fatalf("recipe file was not written: %v", err)
	}
	r, err := recipes.Parse(recipePath(meta.ID), data)
	if err != nil {
		t.Fatalf("generated recipe does not load: %v\n---\n%s", err, data)
	}
	if r.On.Schedule != "0 0 9 * * *" {
		t.Fatalf("On.Schedule = %q, want the cron passed through unchanged", r.On.Schedule)
	}
}

func TestRefreshOmittedKeepsTheExistingScheduleAndFile(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Refresh: &Refresh{Every: "30m", Brief: "b"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.ReadFile(recipePath(meta.ID))
	if err != nil {
		t.Fatalf("recipe should exist: %v", err)
	}

	// A later save that never mentions refresh must not touch it.
	meta2, err := Save(SaveInput{ID: meta.ID, Title: "T renamed"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if meta2.Refresh == nil || meta2.Refresh.Every != "30m" {
		t.Fatalf("refresh was dropped by a save that omitted it: %+v", meta2.Refresh)
	}
	after, err := os.ReadFile(recipePath(meta.ID))
	if err != nil {
		t.Fatalf("recipe should still exist: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("recipe file was rewritten by a save that omitted refresh")
	}
}

func TestRefreshNullRemovesScheduleAndFile(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Refresh: &Refresh{Every: "1h", Brief: "b"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(recipePath(meta.ID)); err != nil {
		t.Fatalf("recipe should exist before removal: %v", err)
	}

	meta2, err := Save(SaveInput{ID: meta.ID, Title: "T", Refresh: nil, RefreshSet: true})
	if err != nil {
		t.Fatalf("Save with refresh:null: %v", err)
	}
	if meta2.Refresh != nil {
		t.Fatalf("meta.Refresh = %+v, want nil after removal", meta2.Refresh)
	}
	if _, err := os.Stat(recipePath(meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("recipe file should have been removed, stat err = %v", err)
	}
}

func TestSaveWithRefreshReplacesAPreviousRecipe(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Refresh: &Refresh{Every: "1h", Brief: "first brief"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	meta, err = Save(SaveInput{
		ID: meta.ID, Title: "T",
		Refresh: &Refresh{Every: "6h", Brief: "second brief"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save (replace refresh): %v", err)
	}
	data, err := os.ReadFile(recipePath(meta.ID))
	if err != nil {
		t.Fatalf("recipe should exist: %v", err)
	}
	r, err := recipes.Parse(recipePath(meta.ID), data)
	if err != nil {
		t.Fatalf("replaced recipe does not load: %v", err)
	}
	if r.On.Schedule != "@every 6h" {
		t.Fatalf("On.Schedule = %q, want the replaced schedule", r.On.Schedule)
	}
	if !strings.Contains(r.Steps[0].Text, "second brief") || strings.Contains(r.Steps[0].Text, "first brief") {
		t.Fatalf("recipe still carries the old brief: %s", r.Steps[0].Text)
	}
}

func TestDeleteDashboardRemovesItsRefreshRecipe(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Refresh: &Refresh{Every: "1h", Brief: "b"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Delete(meta.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(recipePath(meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("recipe file should have been removed on delete, stat err = %v", err)
	}
}

func TestValidateRefreshRejectsBothOrNeitherAndMissingBrief(t *testing.T) {
	cases := []*Refresh{
		{Brief: "b"}, // neither every nor cron
		{Every: "1h", Cron: "0 0 9 * * *", Brief: "b"}, // both
		{Every: "1h"},                         // no brief
		{Every: "not-a-duration", Brief: "b"}, // bad duration
	}
	for _, r := range cases {
		if err := validateRefresh(r); err == nil {
			t.Errorf("validateRefresh(%+v) should have failed", r)
		}
	}
}
