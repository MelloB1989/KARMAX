package console

import (
	"context"
	"io"
	"sync"
)

// fakeRunner answers every host command through fn and records argv.
type fakeRunner struct {
	mu   sync.Mutex
	fn   func(argv []string) ([]byte, error)
	seen [][]string
}

func (f *fakeRunner) Run(_ context.Context, argv []string, _ io.Reader) ([]byte, error) {
	f.mu.Lock()
	f.seen = append(f.seen, argv)
	f.mu.Unlock()
	return f.fn(argv)
}
