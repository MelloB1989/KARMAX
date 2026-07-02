package core

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// pendingQueue is a tiny append-only file queue of actionable items discovered
// by loops (e.g. memory-bootstrap). It decouples DISCOVERY (any loop enqueues)
// from EXECUTION (the act-on-pending loop drains + acts), and survives restarts.
var pendingMu sync.Mutex

func pendingPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".karmax")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "pending-actions.queue"), nil
}

// enqueuePending appends actionable items (one per line) to the queue.
func enqueuePending(items []string) error {
	if len(items) == 0 {
		return nil
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	path, err := pendingPath()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, it := range items {
		it = strings.ReplaceAll(strings.TrimSpace(it), "\n", " ")
		if it != "" {
			w.WriteString(it)
			w.WriteByte('\n')
		}
	}
	return w.Flush()
}

// drainPending atomically reads and clears the queue, returning its items.
func drainPending() ([]string, error) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	path, err := pendingPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var items []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			items = append(items, line)
		}
	}
	// Clear the queue now that we've taken ownership of the items.
	_ = os.Remove(path)
	return items, nil
}

// requeuePending puts items back (e.g. if execution failed), so they aren't lost.
func requeuePending(items []string) {
	_ = enqueuePending(items)
}
