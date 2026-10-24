package filexfer

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, link string
	flag       byte
	mode       int64
	body       string
}

func archive(t *testing.T, es ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range es {
		if e.flag == 0 {
			e.flag = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: e.flag, Mode: e.mode, Linkname: e.link}
		if e.flag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.flag == tar.TypeReg {
			_, _ = io.WriteString(tw, e.body)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestADirectoryRoundTripsWithItsModesAndRelativeLinks(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	for p, mode := range map[string]os.FileMode{"run.sh": 0o755, "secret.txt": 0o600, "sub/deep/a.txt": 0o644} {
		full := filepath.Join(src, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("body "+p), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(full, mode)
	}
	if err := os.Symlink("sub/deep/a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	var packed, checked bytes.Buffer
	if err := Pack(&packed, []string{src}); err != nil {
		t.Fatal(err)
	}
	if err := Sanitize(&checked, &packed); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := Extract(dst, &checked); err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{"run.sh": 0o755, "secret.txt": 0o600, "sub/deep/a.txt": 0o644} {
		fi, err := os.Lstat(filepath.Join(dst, "proj", p))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v", p, fi, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "proj", p)); string(b) != "body "+p {
			t.Errorf("%s holds %q", p, b)
		}
	}
	if to, err := os.Readlink(filepath.Join(dst, "proj", "link")); err != nil || to != "sub/deep/a.txt" {
		t.Errorf("link = %q, %v", to, err)
	}
}

func TestAnArchiveThatLeavesTheTreeIsRefused(t *testing.T) {
	for name, es := range map[string][]entry{
		"an absolute name":             {{name: "/etc/passwd"}},
		"a dot-dot name":               {{name: "a/../../x"}},
		"a hard link":                  {{name: "h", flag: tar.TypeLink, link: "x"}},
		"a device":                     {{name: "d", flag: tar.TypeChar}},
		"an absolute symlink":          {{name: "l", flag: tar.TypeSymlink, link: "/etc"}},
		"a symlink out of the tree":    {{name: "l", flag: tar.TypeSymlink, link: "../x"}},
		"a symlink out from a subdir":  {{name: "a/b/l", flag: tar.TypeSymlink, link: "../../../x"}},
		"a file through a symlink":     {{name: "l", flag: tar.TypeSymlink, link: "."}, {name: "l/x"}},
		"a file over a symlink":        {{name: "l", flag: tar.TypeSymlink, link: "a"}, {name: "l"}},
		"a symlink over a made name":   {{name: "a/x"}, {name: "a", flag: tar.TypeSymlink, link: "b"}},
		"a symlink to nowhere (empty)": {{name: "l", flag: tar.TypeSymlink}},
	} {
		data := archive(t, es...)
		if err := Sanitize(io.Discard, bytes.NewReader(data)); err == nil {
			t.Errorf("Sanitize let through %s", name)
		}
		dir := t.TempDir()
		if err := Extract(dir, bytes.NewReader(data)); err == nil {
			t.Errorf("Extract let through %s", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "..", "x")); err == nil {
			t.Errorf("%s wrote outside", name)
		}
	}
}

func TestSanitizeDropsOwnersAndSpecialBits(t *testing.T) {
	data := archive(t, entry{name: "f", mode: 0o4755, body: "x"})
	var out bytes.Buffer
	if err := Sanitize(&out, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(&out).Next()
	if err != nil || h.Mode != 0o755 || h.Uid != 0 || h.Uname != "" {
		t.Fatalf("%+v, %v", h, err)
	}
}

func TestExtractNeverWritesThroughALinkAlreadyInTheDirectory(t *testing.T) {
	outside, dst := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "out")); err != nil {
		t.Fatal(err)
	}
	err := Extract(dst, bytes.NewReader(archive(t, entry{name: "out/pwned", body: "x"})))
	if err == nil {
		t.Error("extract went through the link")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("a file landed outside the directory")
	}
}

func TestPackRefusesALinkOutOfTheTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "proj")
	_ = os.MkdirAll(src, 0o755)
	if err := os.Symlink("/etc/hosts", filepath.Join(src, "abs")); err != nil {
		t.Fatal(err)
	}
	err := Pack(io.Discard, []string{src})
	if err == nil || !strings.Contains(err.Error(), "abs") {
		t.Fatalf("err = %v", err)
	}
}

func TestPackDoesNotFollowASymlinkNamedOnItsOwn(t *testing.T) {
	d := t.TempDir()
	_ = os.WriteFile(filepath.Join(d, "real"), []byte("x"), 0o644)
	_ = os.Symlink("real", filepath.Join(d, "ln"))
	var b bytes.Buffer
	if err := Pack(&b, []string{filepath.Join(d, "ln")}); err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(&b).Next()
	if err != nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "real" {
		t.Fatalf("%+v, %v", h, err)
	}
}

func TestCheckPathAllowsWorkAndTmpOnly(t *testing.T) {
	for p, want := range map[string]string{"/work/inbox": "/work/inbox", "/tmp/x/../y": "/tmp/y", "notes": "/work/notes", "/work": "/work"} {
		if got, err := CheckPath(p); err != nil || got != want {
			t.Errorf("%q = %q, %v", p, got, err)
		}
	}
	for _, p := range []string{"/etc", "/work/../etc/passwd", "/workshop", "/tmpfoo", "/home/op", "/", "/work\x00"} {
		if got, err := CheckPath(p); err == nil {
			t.Errorf("%q allowed as %q", p, got)
		}
	}
}

func TestLimitCutsAStreamOverTheCapButNotOneExactlyAtIt(t *testing.T) {
	if _, err := io.ReadAll(Limit(strings.NewReader("12345"), 5)); err != nil {
		t.Errorf("at the cap: %v", err)
	}
	_, err := io.ReadAll(Limit(strings.NewReader("123456"), 5))
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("over the cap: %v", err)
	}
}
