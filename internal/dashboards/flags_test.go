package dashboards

import (
	"errors"
	"os"
	"testing"
)

func TestOldMetaFileWithoutFlagsDefaultsToFalse(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "T", HTMLSet: true, HTML: "<p></p>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulates a dashboard.json written before pinned/archived existed —
	// the file KARMAX has to keep loading correctly after this upgrade.
	old := `{"id":"` + meta.ID + `","title":"T","createdAt":"x","updatedAt":"y","htmlVersion":1,"dataVersion":0,"data":[]}`
	if err := os.WriteFile(metaPath(meta.ID), []byte(old), 0o644); err != nil {
		t.Fatalf("writing old-style meta: %v", err)
	}

	got, _, err := Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pinned || got.Archived || got.ArchivedAt != "" {
		t.Fatalf("old meta file without the fields should load as false/empty, got %+v", got)
	}
}

func TestSetFlagsRoundTrips(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "T", HTMLSet: true, HTML: "<p></p>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	yes := true
	updated, err := SetFlags(meta.ID, &yes, nil)
	if err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if !updated.Pinned {
		t.Fatalf("Pinned = false, want true after SetFlags(pinned=true)")
	}
	if updated.Archived {
		t.Fatalf("archived changed by a call that left it nil: %+v", updated)
	}

	no := false
	updated, err = SetFlags(meta.ID, &no, nil)
	if err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if updated.Pinned {
		t.Fatalf("Pinned = true, want false after SetFlags(pinned=false)")
	}
}

func TestSetFlagsUnknownIDIsNotFound(t *testing.T) {
	isolate(t)
	yes := true
	if _, err := SetFlags("does-not-exist", &yes, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetFlags on an unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestSetFlagsArchiveRemovesRecipeAndKeepsRefreshThenUnarchiveRestoresIt(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Refresh: &Refresh{Every: "1h", Brief: "b"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(recipePath(meta.ID)); err != nil {
		t.Fatalf("recipe should exist before archiving: %v", err)
	}

	yes := true
	updated, err := SetFlags(meta.ID, nil, &yes)
	if err != nil {
		t.Fatalf("SetFlags(archive): %v", err)
	}
	if !updated.Archived {
		t.Fatalf("Archived = false, want true")
	}
	if updated.ArchivedAt == "" {
		t.Fatalf("ArchivedAt not set after archiving")
	}
	if updated.Refresh == nil || updated.Refresh.Every != "1h" {
		t.Fatalf("Meta.Refresh should be kept while archived, got %+v", updated.Refresh)
	}
	if _, err := os.Stat(recipePath(meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("recipe should have been removed by archiving, stat err = %v", err)
	}

	no := false
	updated, err = SetFlags(meta.ID, nil, &no)
	if err != nil {
		t.Fatalf("SetFlags(unarchive): %v", err)
	}
	if updated.Archived {
		t.Fatalf("Archived = true, want false after un-archiving")
	}
	if updated.ArchivedAt != "" {
		t.Fatalf("ArchivedAt = %q, want empty after un-archiving", updated.ArchivedAt)
	}
	if updated.Refresh == nil || updated.Refresh.Every != "1h" {
		t.Fatalf("Meta.Refresh should still be recorded after un-archiving, got %+v", updated.Refresh)
	}
	if _, err := os.Stat(recipePath(meta.ID)); err != nil {
		t.Fatalf("recipe should have been restored by un-archiving: %v", err)
	}
}

func TestSaveWithRefreshWhileArchivedWritesNoRecipe(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "T", HTMLSet: true, HTML: "<p></p>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	yes := true
	if _, err := SetFlags(meta.ID, nil, &yes); err != nil {
		t.Fatalf("SetFlags(archive): %v", err)
	}

	updated, err := Save(SaveInput{
		ID: meta.ID, Title: "T",
		Refresh: &Refresh{Every: "1h", Brief: "b"}, RefreshSet: true,
	})
	if err != nil {
		t.Fatalf("Save with refresh on an archived dashboard: %v", err)
	}
	if updated.Refresh == nil || updated.Refresh.Every != "1h" {
		t.Fatalf("Meta.Refresh should record the refresh even while archived, got %+v", updated.Refresh)
	}
	if !updated.Archived {
		t.Fatalf("Save must not un-archive a dashboard: %+v", updated)
	}
	if _, err := os.Stat(recipePath(meta.ID)); !os.IsNotExist(err) {
		t.Fatalf("an archived dashboard must not get a refresh recipe, stat err = %v", err)
	}
}

func TestSaveAndSetDataPreserveFlags(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "T", HTMLSet: true, HTML: "<p></p>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	yes := true
	if _, err := SetFlags(meta.ID, &yes, &yes); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}

	saved, err := Save(SaveInput{ID: meta.ID, Title: "T renamed"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !saved.Pinned || !saved.Archived {
		t.Fatalf("Save unpinned or unarchived a dashboard an agent merely updated: %+v", saved)
	}

	updated, err := SetData(meta.ID, "x", 1, "")
	if err != nil {
		t.Fatalf("SetData: %v", err)
	}
	if !updated.Pinned || !updated.Archived {
		t.Fatalf("SetData unpinned or unarchived a dashboard: %+v", updated)
	}
}

func TestSetFlagsLeavesVersionsAndUpdatedAtAlone(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Data: map[string]any{"a": 1},
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	yes := true
	updated, err := SetFlags(meta.ID, &yes, &yes)
	if err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	if updated.UpdatedAt != meta.UpdatedAt {
		t.Fatalf("SetFlags changed UpdatedAt: before=%q after=%q", meta.UpdatedAt, updated.UpdatedAt)
	}
	if updated.HTMLVersion != meta.HTMLVersion || updated.DataVersion != meta.DataVersion {
		t.Fatalf("SetFlags changed versions: before=%+v after=%+v", meta, updated)
	}
}
