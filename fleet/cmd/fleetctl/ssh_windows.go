//go:build windows

package main

import (
	"context"
	"time"

	"golang.org/x/sys/windows"
)

// winch ticks when the console changes size: Windows has no SIGWINCH, so
// the size is polled every 250 ms.
func winch(ctx context.Context, size func() (int, int, bool)) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		c, r, _ := size()
		tk := time.NewTicker(250 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if nc, nr, ok := size(); ok && (nc != c || nr != r) {
					c, r = nc, nr
					select {
					case out <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return out
}

// enableVT makes the console draw the session's escape sequences, and
// returns what puts it back.
func enableVT(fd int) func() {
	if fd < 0 {
		return func() {}
	}
	h := windows.Handle(fd)
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	return func() { windows.SetConsoleMode(h, mode) }
}
