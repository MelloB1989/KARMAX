// Package clipboard is the clipboard of the machine fleetctl runs on, as
// the desk needs it: the files or the image on it (a terminal hands a
// program only text), and a file put on it. On a Mac through osascript
// (AppKit's pasteboard, nothing to install); on Linux through wl-clipboard
// or xclip.
package clipboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Clipboard is this machine's clipboard.
type Clipboard interface {
	// Read says what the clipboard holds: files (absolute paths), else an
	// image, written as a PNG to png. Text alone is neither.
	Read(ctx context.Context, png string) (Got, error)
	// PutFile puts the file at path on the clipboard, to paste as a file.
	PutFile(ctx context.Context, path string) error
}

// Got is what Read found.
type Got struct {
	Files []string `json:"files"`
	Image string   `json:"image"` // the PNG written, or ""
}

// clipTimeout bounds one look at the clipboard.
const clipTimeout = 10 * time.Second

// Bridge is this machine's clipboard; nil where there is no way to it.
func Bridge() Clipboard {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err == nil {
			return macClipboard{}
		}
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			if _, err := exec.LookPath("wl-paste"); err == nil {
				return linuxClipboard{wayland: true}
			}
		}
		if os.Getenv("DISPLAY") != "" {
			if _, err := exec.LookPath("xclip"); err == nil {
				return linuxClipboard{}
			}
		}
	}
	return nil
}

func runClip(ctx context.Context, stdin []byte, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, clipTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		if m := strings.TrimSpace(errb.String()); m != "" {
			return out.Bytes(), fmt.Errorf("%s: %s", filepath.Base(argv[0]), m)
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

// ---- macOS ----

type macClipboard struct{}

// macRead is JavaScript for Automation over AppKit: each pasteboard item's
// file URL (Finder's copy: one item a file), else the image as PNG (a
// screenshot to the clipboard is PNG; Preview's copy, TIFF, is made PNG).
// argv[0] is where the PNG goes.
const macRead = `ObjC.import("AppKit");
function run(argv) {
	var pb = $.NSPasteboard.generalPasteboard;
	var out = {files: [], image: ""};
	var items = pb.pasteboardItems;
	var n = items.isNil() ? 0 : items.count;
	for (var i = 0; i < n; i++) {
		var s = items.objectAtIndex(i).stringForType($("public.file-url"));
		if (s.isNil()) continue;
		var u = $.NSURL.URLWithString(s);
		if (!u.isNil() && u.isFileURL) out.files.push(ObjC.unwrap(u.path));
	}
	if (out.files.length == 0) {
		var data = pb.dataForType($("public.png"));
		if (data.isNil()) {
			var tiff = pb.dataForType($("public.tiff"));
			if (!tiff.isNil()) {
				var rep = $.NSBitmapImageRep.imageRepWithData(tiff);
				if (!rep.isNil()) data = rep.representationUsingTypeProperties(4, $({})); // NSBitmapImageFileTypePNG
			}
		}
		if (!data.isNil() && data.writeToFileAtomically($(argv[0]), true)) out.image = argv[0];
	}
	return JSON.stringify(out);
}`

// macPut puts argv[0], a file, on the pasteboard as a file URL.
const macPut = `ObjC.import("AppKit");
function run(argv) {
	var pb = $.NSPasteboard.generalPasteboard;
	pb.clearContents;
	var ok = pb.writeObjects($.NSArray.arrayWithObject($.NSURL.fileURLWithPath($(argv[0]))));
	return ok ? "ok" : "refused";
}`

func (macClipboard) Read(ctx context.Context, png string) (Got, error) {
	out, err := runClip(ctx, nil, "osascript", "-l", "JavaScript", "-e", macRead, png)
	if err != nil {
		return Got{}, err
	}
	var g Got
	if err := json.Unmarshal(bytes.TrimSpace(out), &g); err != nil {
		return Got{}, fmt.Errorf("osascript said %q: %w", bytes.TrimSpace(out), err)
	}
	return g, nil
}

func (macClipboard) PutFile(ctx context.Context, path string) error {
	out, err := runClip(ctx, nil, "osascript", "-l", "JavaScript", "-e", macPut, path)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(string(out)); s != "ok" {
		return fmt.Errorf("the pasteboard %s the file", s)
	}
	return nil
}

// ---- Linux ----

type linuxClipboard struct{ wayland bool }

func (l linuxClipboard) get(ctx context.Context, typ string) ([]byte, error) {
	if l.wayland {
		if typ == "" {
			return runClip(ctx, nil, "wl-paste", "--list-types")
		}
		return runClip(ctx, nil, "wl-paste", "--no-newline", "--type", typ)
	}
	if typ == "" {
		typ = "TARGETS"
	}
	return runClip(ctx, nil, "xclip", "-selection", "clipboard", "-o", "-t", typ)
}

func (l linuxClipboard) Read(ctx context.Context, png string) (Got, error) {
	types, err := l.get(ctx, "")
	if err != nil { // an empty clipboard: nothing on it
		return Got{}, nil
	}
	has := map[string]bool{}
	for _, t := range strings.Fields(string(types)) {
		has[t] = true
	}
	if has["text/uri-list"] || has["x-special/gnome-copied-files"] {
		typ := "text/uri-list"
		if !has[typ] {
			typ = "x-special/gnome-copied-files"
		}
		b, err := l.get(ctx, typ)
		if err != nil {
			return Got{}, err
		}
		if files := fileURIs(string(b)); len(files) > 0 {
			return Got{Files: files}, nil
		}
	}
	if has["image/png"] {
		b, err := l.get(ctx, "image/png")
		if err != nil {
			return Got{}, err
		}
		if len(b) > 0 {
			if err := os.WriteFile(png, b, 0o600); err != nil {
				return Got{}, err
			}
			return Got{Image: png}, nil
		}
	}
	return Got{}, nil
}

// fileURIs is the local files a uri-list names (GNOME's copied-files list
// starts with copy or cut).
func fileURIs(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "file://") {
			continue
		}
		u, err := url.Parse(l)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			continue
		}
		out = append(out, u.Path)
	}
	return out
}

func (l linuxClipboard) PutFile(ctx context.Context, path string) error {
	uri := (&url.URL{Scheme: "file", Path: path}).String() + "\n"
	if l.wayland {
		_, err := runClip(ctx, []byte(uri), "wl-copy", "--type", "text/uri-list")
		return err
	}
	// xclip stays to serve the selection: it must not hold this call.
	cmd := exec.Command("xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i")
	cmd.Stdin = strings.NewReader(uri)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
