//go:build linux || darwin || freebsd || openbsd || netbsd

package tui

import (
	"os"

	"golang.org/x/sys/unix"
)

// termCellPx is a cell's pixels as the terminal tells the tty; 0, 0 when
// it does not.
func termCellPx() (int, int) {
	for _, f := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err != nil || ws.Col == 0 || ws.Row == 0 || ws.Xpixel == 0 || ws.Ypixel == 0 {
			continue
		}
		return int(ws.Xpixel) / int(ws.Col), int(ws.Ypixel) / int(ws.Row)
	}
	return 0, 0
}
