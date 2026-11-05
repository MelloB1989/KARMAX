package hostfiles

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// testFiles is a Manager whose home and state are a test's own.
func testFiles(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	write(t, filepath.Join(home, "code", "app", "main.go"), "package main\n")
	write(t, filepath.Join(home, "code", "app", "README.md"), "# App\n")
	write(t, filepath.Join(home, ".hidden"), "x")
	if err := os.Symlink("code/app", filepath.Join(home, "app")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(home, "broken")); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Home: home, State: filepath.Join(root, "state"), Host: "testhost"}
	return m, home
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(d fleetapi.FileDir) map[string]fleetapi.FileEntry {
	out := map[string]fleetapi.FileEntry{}
	for _, e := range d.Entries {
		out[e.Name] = e
	}
	return out
}

func TestAFolderIsListedWithWhatItsLinksPointAt(t *testing.T) {
	m, home := testFiles(t)
	d, err := m.List("~")
	if err != nil {
		t.Fatal(err)
	}
	if d.Path != home || d.Home != home || d.Host != "testhost" {
		t.Fatalf("listed %+v", d)
	}
	es := names(d)
	if e := es["code"]; e.Kind != fleetapi.KindDir || !e.IsDir() {
		t.Errorf("code: %+v", e)
	}
	if e := es["app"]; e.Kind != fleetapi.KindLink || e.To != fleetapi.KindDir || !e.IsDir() || e.Link != "code/app" {
		t.Errorf("a link to a folder: %+v", e)
	}
	if e := es["broken"]; e.Kind != fleetapi.KindLink || e.To != "" || e.IsDir() {
		t.Errorf("a broken link: %+v", e)
	}
	if e := es[".hidden"]; e.Kind != fleetapi.KindFile || e.Size != 1 {
		t.Errorf("a hidden file is listed (the TUI hides it): %+v", e)
	}
	// A file's path is its folder, the file in focus; ~/ and a relative
	// path are from home.
	d, err = m.List("code/app/main.go")
	if err != nil || d.Path != filepath.Join(home, "code", "app") || d.Focus != "main.go" {
		t.Fatalf("a file's path: %+v %v", d, err)
	}
	if _, err := m.List("~/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a folder that is not there: %v", err)
	}
}

func TestAnUploadGoesOnWhereALinkBrokeOff(t *testing.T) {
	m, home := testFiles(t)
	dir := filepath.Join(home, "code")
	body := strings.Repeat("0123456789", 1000)
	id := NewUploadID()
	// The link breaks after 4000 bytes: fleetd holds them.
	p, err := m.Put(dir, "big.txt", id, 0, int64(len(body)), strings.NewReader(body[:4000]))
	if err == nil || p.Done || p.Have != 4000 {
		t.Fatalf("a cut upload: %+v %v", p, err)
	}
	if n, _ := m.Have(id); n != 4000 {
		t.Fatalf("held %d", n)
	}
	// From the wrong place: refused, saying where it is.
	if _, err := m.Put(dir, "big.txt", id, 3000, int64(len(body)), strings.NewReader(body[3000:])); !errors.Is(err, ErrMismatch) {
		t.Fatalf("from the wrong place: %v", err)
	}
	p, err = m.Put(dir, "big.txt", id, 4000, int64(len(body)), strings.NewReader(body[4000:]))
	if err != nil || !p.Done || p.Path != filepath.Join(dir, "big.txt") {
		t.Fatalf("the rest: %+v %v", p, err)
	}
	if b, _ := os.ReadFile(p.Path); string(b) != body {
		t.Fatal("the file is not what was sent")
	}
	// The same name again: beside it, not over it.
	p, err = m.Put(dir, "big.txt", NewUploadID(), 0, 3, strings.NewReader("new"))
	if err != nil || p.Path != filepath.Join(dir, "big 2.txt") {
		t.Fatalf("a second big.txt: %+v %v", p, err)
	}
	// An empty file is a file.
	if p, err = m.Put(dir, "empty", NewUploadID(), 0, 0, strings.NewReader("")); err != nil || !p.Done {
		t.Fatalf("an empty file: %+v %v", p, err)
	}
	for _, bad := range []struct{ name, id string }{{"../escape", NewUploadID()}, {"a/b", NewUploadID()}, {"ok", "../../x"}, {"..", NewUploadID()}} {
		if _, err := m.Put(dir, bad.name, bad.id, 0, 1, strings.NewReader("x")); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q id %q: %v", bad.name, bad.id, err)
		}
	}
}

func TestAFolderMovesAsATarEitherWay(t *testing.T) {
	m, home := testFiles(t)
	write(t, filepath.Join(home, "proj", "a", "b.txt"), "bee")
	if err := os.Symlink("a/b.txt", filepath.Join(home, "proj", "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(home, "proj", "outside")); err != nil {
		t.Fatal(err)
	}
	var tarball bytes.Buffer
	if err := m.Tar(context.Background(), "~/proj", true, &tarball); err != nil {
		t.Fatal(err)
	}
	// Unpacked into a folder that has a proj already: beside it.
	p, err := m.Untar("~/code", bytes.NewReader(tarball.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != filepath.Join(home, "code", "proj") {
		t.Fatalf("unpacked to %s", p.Path)
	}
	if b, _ := os.ReadFile(filepath.Join(p.Path, "a", "b.txt")); string(b) != "bee" {
		t.Fatal("b.txt did not come")
	}
	if to, _ := os.Readlink(filepath.Join(p.Path, "inside")); to != "a/b.txt" {
		t.Fatalf("a link inside: %q", to)
	}
	if _, err := os.Lstat(filepath.Join(p.Path, "outside")); err == nil {
		t.Fatal("a link out of the folder came")
	}
	p, err = m.Untar("~/code", bytes.NewReader(tarball.Bytes()))
	if err != nil || p.Path != filepath.Join(home, "code", "proj 2") {
		t.Fatalf("again: %+v %v", p, err)
	}
	if left, _ := filepath.Glob(filepath.Join(home, "code", ".fleet-untar-*")); len(left) > 0 {
		t.Fatalf("left behind: %v", left)
	}
	// A tar that breaks off leaves nothing half there.
	if _, err := m.Untar("~/code", io.LimitReader(bytes.NewReader(tarball.Bytes()), 40)); err == nil {
		t.Fatal("a broken tar unpacked")
	}
	if left, _ := filepath.Glob(filepath.Join(home, "code", "proj 3")); len(left) > 0 {
		t.Fatal("a broken tar left a folder")
	}
	var n atomic.Int64
	if _, err := Pack(context.Background(), io.Discard, filepath.Join(home, "proj"), &n); err != nil || n.Load() != 3 {
		t.Fatalf("packed %d bytes of files: %v", n.Load(), err)
	}
	if c := Count(filepath.Join(home, "proj"), time.Second); c != 3 {
		t.Fatalf("counted %d", c)
	}
}

func TestFolderNamesTheTrashAndDeleting(t *testing.T) {
	m, home := testFiles(t)
	if _, err := m.Op(fleetapi.FileOp{Op: fleetapi.OpMkdir, Path: "~/new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Op(fleetapi.FileOp{Op: fleetapi.OpMkdir, Path: "~/new"}); !errors.Is(err, ErrExists) {
		t.Fatalf("a folder that is there: %v", err)
	}
	d, err := m.Op(fleetapi.FileOp{Op: fleetapi.OpRename, Path: "~/new", To: "newer"})
	if err != nil || d.Path != filepath.Join(home, "newer") {
		t.Fatalf("renamed: %+v %v", d, err)
	}
	if _, err := m.Op(fleetapi.FileOp{Op: fleetapi.OpRename, Path: "~/newer", To: "code"}); !errors.Is(err, ErrExists) {
		t.Fatalf("a rename over something: %v", err)
	}
	d, err = m.Op(fleetapi.FileOp{Op: fleetapi.OpTrash, Paths: []string{"~/newer"}})
	if err != nil || len(d.Paths) != 1 || !strings.HasPrefix(d.Paths[0], filepath.Join(m.State, "trash")) {
		t.Fatalf("trashed: %+v %v", d, err)
	}
	if _, err := os.Stat(d.Paths[0]); err != nil {
		t.Fatal("not in the trash")
	}
	if _, err := m.Op(fleetapi.FileOp{Op: fleetapi.OpDelete, Paths: []string{"~/.hidden"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".hidden")); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
	for _, p := range []string{"/", "~", filepath.Dir(home), m.State, filepath.Dir(m.State)} {
		for _, op := range []string{fleetapi.OpTrash, fleetapi.OpDelete} {
			if _, err := m.Op(fleetapi.FileOp{Op: op, Paths: []string{p}}); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s %s: %v", op, p, err)
			}
		}
	}
	// The trash is emptied a week on.
	m.Now = func() time.Time { return time.Now().Add(9 * 24 * time.Hour) }
	m.Sweep()
	if _, err := os.Stat(d.Paths[0]); !os.IsNotExist(err) {
		t.Fatal("the trash was kept")
	}
}

func TestAPreviewIsTheSizeItIsShownAt(t *testing.T) {
	m, home := testFiles(t)
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := range 400 {
		for y := range 200 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "pic.png"), b.String())
	out, typ, err := m.Preview(context.Background(), "~/pic.png", 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := image.Decode(bytes.NewReader(out))
	if err != nil || typ != "image/png" || got.Bounds().Dx() != 100 || got.Bounds().Dy() != 50 {
		t.Fatalf("%s %v %v", typ, got.Bounds(), err)
	}
	if _, _, err := m.Preview(context.Background(), "~/code/app/main.go", 100, 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a text file: %v", err)
	}
}

func TestAPhotoIsTurnedTheWayItsCameraSays(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 40, 20)) // wide, as stored
	var j bytes.Buffer
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	// An APP1 EXIF segment, big-endian, orientation 6 (turn right).
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0}
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := append([]byte{0xFF, 0xE1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}, app1...)
	photo := append(append([]byte{0xFF, 0xD8}, seg...), j.Bytes()[2:]...)
	if o := exifOrientation(bytes.NewReader(photo)); o != 6 {
		t.Fatalf("orientation %d", o)
	}
	out, _, err := picture(bytes.NewReader(photo), 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := image.Decode(bytes.NewReader(out))
	if got.Bounds().Dx() != 20 || got.Bounds().Dy() != 40 {
		t.Fatalf("shown %v, want it upright (20×40)", got.Bounds())
	}
}
