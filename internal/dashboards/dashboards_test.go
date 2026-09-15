package dashboards

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// isolate points Root() and recipes.Dir() at fresh temp directories, so a
// test can never read or write the operator's real ~/.karmax.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	t.Setenv("KARMAX_RECIPES_DIR", t.TempDir())
}

func TestSaveCreatesADashboardAndDefaultsIDFromTitle(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "Sales Overview!", HTMLSet: true, HTML: "<html></html>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if meta.ID != "sales-overview" {
		t.Fatalf("ID = %q, want %q", meta.ID, "sales-overview")
	}
	if meta.HTMLVersion != 1 {
		t.Fatalf("HTMLVersion = %d, want 1", meta.HTMLVersion)
	}
	if meta.DataVersion != 0 {
		t.Fatalf("DataVersion = %d, want 0 (no data given)", meta.DataVersion)
	}
	if meta.CreatedAt == "" || meta.UpdatedAt == "" {
		t.Fatalf("CreatedAt/UpdatedAt not set: %+v", meta)
	}
}

func TestSaveWithoutHTMLFailsWhenDashboardDoesNotExist(t *testing.T) {
	isolate(t)
	if _, err := Save(SaveInput{Title: "New One"}); err == nil {
		t.Fatal("Save with no html on a new dashboard should fail")
	}
}

func TestSaveThenUpdateKeepsUntouchedDataAndBumpsOnlyWhatChanged(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{
		Title: "Metrics", HTMLSet: true, HTML: "<p>v1</p>",
		Data: map[string]any{"revenue": 100, "orders": 5},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if meta.HTMLVersion != 1 || meta.DataVersion != 1 {
		t.Fatalf("after create: html=%d data=%d, want 1,1", meta.HTMLVersion, meta.DataVersion)
	}

	// Update only "revenue" and the title — html and "orders" untouched.
	meta, err = Save(SaveInput{
		ID: meta.ID, Title: "Metrics v2",
		Data: map[string]any{"revenue": 200},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if meta.HTMLVersion != 1 {
		t.Fatalf("HTMLVersion changed on an update that did not supply html: %d", meta.HTMLVersion)
	}
	if meta.DataVersion != 2 {
		t.Fatalf("DataVersion = %d, want 2", meta.DataVersion)
	}
	if len(meta.Data) != 2 {
		t.Fatalf("Data = %v, want both revenue and orders kept", meta.Data)
	}
	_, html, err := Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if html != "<p>v1</p>" {
		t.Fatalf("html changed without being supplied: %q", html)
	}
	ordersRaw, err := GetData(meta.ID, "orders")
	if err != nil || string(ordersRaw) != "5" {
		t.Fatalf("orders data changed or missing: %q, err=%v", ordersRaw, err)
	}
}

func TestSaveRejectsReservedAndMalformedIDs(t *testing.T) {
	isolate(t)
	cases := []string{"_kit", "_anything", "Has-Caps", "has space", "-leading-hyphen"}
	for _, id := range cases {
		if _, err := Save(SaveInput{ID: id, Title: "T", HTMLSet: true, HTML: "<p></p>"}); err == nil {
			t.Errorf("id %q should have been rejected", id)
		}
	}
}

func TestSaveRejectsATitleThatHasNoUsableSlug(t *testing.T) {
	isolate(t)
	if _, err := Save(SaveInput{Title: "😀😀😀", HTMLSet: true, HTML: "<p></p>"}); err == nil {
		t.Fatal("a title with no letters or digits should fail to derive an id")
	}
}

func TestSaveRejectsMalformedDataNames(t *testing.T) {
	isolate(t)
	_, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Data: map[string]any{"Bad Name!": 1},
	})
	if err == nil {
		t.Fatal("malformed data name should have been rejected")
	}
}

func TestSaveEnforcesSizeAndCountLimits(t *testing.T) {
	isolate(t)

	// html over 512 KB.
	big := strings.Repeat("x", MaxHTMLBytes+1)
	if _, err := Save(SaveInput{Title: "T", HTMLSet: true, HTML: big}); err == nil {
		t.Error("oversized html should have been rejected")
	}

	// a data value over 2 MB serialized.
	bigVal := strings.Repeat("y", MaxDataValueBytes)
	if _, err := Save(SaveInput{
		Title: "T", HTMLSet: true, HTML: "<p></p>",
		Data: map[string]any{"blob": bigVal},
	}); err == nil {
		t.Error("oversized data value should have been rejected")
	}

	// more than 32 distinct data files.
	data := map[string]any{}
	for i := 0; i < MaxDataFiles+1; i++ {
		data[strings.Repeat("a", 1)+itoa(i)] = i
	}
	if _, err := Save(SaveInput{Title: "T2", HTMLSet: true, HTML: "<p></p>", Data: data}); err == nil {
		t.Error("more than 32 data files should have been rejected")
	}

	// title over 120 chars.
	if _, err := Save(SaveInput{Title: strings.Repeat("t", MaxTitleChars+1), HTMLSet: true, HTML: "<p></p>"}); err == nil {
		t.Error("oversized title should have been rejected")
	}

	// description over 500 chars.
	if _, err := Save(SaveInput{
		Title: "T3", HTMLSet: true, HTML: "<p></p>",
		Description: strings.Repeat("d", MaxDescChars+1), DescriptionSet: true,
	}); err == nil {
		t.Error("oversized description should have been rejected")
	}

	// live: too many names, and a malformed one.
	live := make([]string, MaxLiveNames+1)
	for i := range live {
		live[i] = "n" + itoa(i)
	}
	if _, err := Save(SaveInput{Title: "T4", HTMLSet: true, HTML: "<p></p>", Live: live, LiveSet: true}); err == nil {
		t.Error("more than 16 live names should have been rejected")
	}
	if _, err := Save(SaveInput{
		Title: "T5", HTMLSet: true, HTML: "<p></p>",
		Live: []string{"Bad Live!"}, LiveSet: true,
	}); err == nil {
		t.Error("malformed live name should have been rejected")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "Clean", HTMLSet: true, HTML: "<p></p>", Data: map[string]any{"a": 1}})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := SetData(meta.ID, "b", 2, ""); err != nil {
		t.Fatalf("SetData: %v", err)
	}

	var tmpFiles []string
	_ = filepath.Walk(dashboardDir(meta.ID), func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(path, ".tmp") {
			tmpFiles = append(tmpFiles, path)
		}
		return nil
	})
	if len(tmpFiles) != 0 {
		t.Fatalf("left temp files behind: %v", tmpFiles)
	}
}

func TestSetDataRequiresAnExistingDashboard(t *testing.T) {
	isolate(t)
	if _, err := SetData("does-not-exist", "a", 1, ""); err == nil {
		t.Fatal("SetData on an unknown dashboard should fail")
	}
}

func TestSetDataBumpsVersionAndLeavesHTMLAlone(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "T", HTMLSet: true, HTML: "<p>orig</p>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	meta, err = SetData(meta.ID, "x", map[string]any{"n": 1}, "")
	if err != nil {
		t.Fatalf("SetData: %v", err)
	}
	if meta.DataVersion != 1 {
		t.Fatalf("DataVersion = %d, want 1", meta.DataVersion)
	}
	if meta.HTMLVersion != 1 {
		t.Fatalf("HTMLVersion changed by set_data: %d", meta.HTMLVersion)
	}
}

func TestGetListDeleteRoundTrip(t *testing.T) {
	isolate(t)
	if _, _, err := Get("nope"); err == nil {
		t.Fatal("Get of an unknown id should fail")
	}
	if err := Delete("nope"); err == nil {
		t.Fatal("Delete of an unknown id should fail")
	}

	a, err := Save(SaveInput{Title: "A", HTMLSet: true, HTML: "<p>a</p>"})
	if err != nil {
		t.Fatalf("Save a: %v", err)
	}
	b, err := Save(SaveInput{Title: "B", HTMLSet: true, HTML: "<p>b</p>"})
	if err != nil {
		t.Fatalf("Save b: %v", err)
	}
	// b was saved after a, so it should sort first.
	list, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("List() = %+v, want [b, a] newest-updated first", list)
	}

	if err := Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := Get(a.ID); err == nil {
		t.Fatal("Get after Delete should fail")
	}
	list, err = List()
	if err != nil || len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("List() after delete = %+v, err=%v", list, err)
	}
}

func TestConcurrentSetDataDoesNotLoseUpdates(t *testing.T) {
	isolate(t)
	meta, err := Save(SaveInput{Title: "Race", HTMLSet: true, HTML: "<p></p>"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := SetData(meta.ID, "counter", i, ""); err != nil {
				t.Errorf("SetData: %v", err)
			}
		}(i)
	}
	wg.Wait()

	final, _, err := Get(meta.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Every one of the n writes must have been counted exactly once — a
	// mutex that failed to serialize these would drop some read-modify-write
	// increments to dashboard.json under concurrent writers.
	if final.DataVersion != n {
		t.Fatalf("DataVersion = %d, want %d — a concurrent write was lost", final.DataVersion, n)
	}
}

func TestComponentReferenceFallsBackThenReflectsWhatWasInstalled(t *testing.T) {
	isolate(t)
	if got := ComponentReference(); got != referenceFallback {
		t.Fatalf("ComponentReference() with nothing installed = %q, want the fallback", got)
	}
	if err := WriteComponentReference("# My Kit\n\nUse <kit-card>."); err != nil {
		t.Fatalf("WriteComponentReference: %v", err)
	}
	if got := ComponentReference(); got != "# My Kit\n\nUse <kit-card>." {
		t.Fatalf("ComponentReference() = %q, want the installed text", got)
	}
}
