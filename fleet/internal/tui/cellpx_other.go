//go:build !(linux || darwin || freebsd || openbsd || netbsd)

package tui

func termCellPx() (int, int) { return 0, 0 }
