package tui

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/hostfiles"
)

// trashHere moves a file of this machine's into its own trash: a Mac's
// ~/.Trash, or the freedesktop trash elsewhere (with the note that lets a
// file manager put it back).
func trashHere(p string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	if abs == home || strings.HasPrefix(home+string(filepath.Separator), abs+string(filepath.Separator)) {
		return fmt.Errorf("%s holds the home folder: not that", abs)
	}
	var to string
	switch runtime.GOOS {
	case "darwin":
		to = hostfiles.Unique(filepath.Join(home, ".Trash"), filepath.Base(abs))
	case "windows":
		return errors.New("no trash this can reach here: X deletes it for good")
	default:
		base := filepath.Join(home, ".local", "share", "Trash")
		if err := os.MkdirAll(filepath.Join(base, "files"), 0o700); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(base, "info"), 0o700); err != nil {
			return err
		}
		to = hostfiles.Unique(filepath.Join(base, "files"), filepath.Base(abs))
		info := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
			(&url.URL{Path: abs}).EscapedPath(), time.Now().Format("2006-01-02T15:04:05"))
		note := filepath.Join(base, "info", filepath.Base(to)+".trashinfo")
		if err := os.WriteFile(note, []byte(info), 0o600); err != nil {
			return err
		}
		defer func() {
			if _, err := os.Lstat(to); err != nil { // it did not go: no note of it
				os.Remove(note)
			}
		}()
	}
	if err := os.Rename(abs, to); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("%s is on another disk than the trash: X deletes it for good", filepath.Base(abs))
		}
		return err
	}
	return nil
}
