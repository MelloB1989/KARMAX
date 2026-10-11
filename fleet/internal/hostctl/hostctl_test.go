package hostctl

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A grandchild that outlives the command and holds its pipes must not hang
// the runner once the context ends (the same hang as target.Exec's).
func TestExecRunnerDoesNotHangOnGrandchild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var b bytes.Buffer
		done <- ExecRunner(ctx, []string{"sh", "-c", "sleep 30 & wait"}, &b)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error from the killed command")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecRunner still blocked 10s after its context ended")
	}
}
