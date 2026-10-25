//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// winch ticks when the terminal changes size (SIGWINCH).
func winch(ctx context.Context, _ func() (int, int, bool)) <-chan struct{} {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	out := make(chan struct{}, 1)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}

// enableVT: a Unix terminal reads escape sequences already.
func enableVT(int) func() { return func() {} }
