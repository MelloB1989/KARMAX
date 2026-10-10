// Package proctrack keeps a pidfile of the harness processes this engine has
// started, so a later engine can finish off any that outlived it.
//
// The problem it solves: the engine is SIGKILLed (or crashes) while a harness
// CLI is mid-task. The child, and everything the CLI spawned, keeps running,
// and the task is re-run by the next engine, so its side effects happen twice.
// A normal shutdown already kills the children through context cancellation;
// a SIGKILL gives the engine no chance to, so the only defence is to remember
// them on disk and reap them at the next start.
//
// A recorded pid is never trusted on its own: pids are reused. An entry is
// acted on only if the process now holding that pid started at the same time
// as the one recorded, or if the pid is gone and a process group of that id
// still has members (the OS does not hand a pid out while a group uses it).
package proctrack

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Entry is one tracked harness process. PGID equals PID for a process started
// with SetGroup; it is recorded separately so the file says what will be killed.
type Entry struct {
	PID   int    `json:"pid"`
	PGID  int    `json:"pgid"`
	Start string `json:"start"` // OS-reported process start time; "" when unknown
	Cmd   string `json:"cmd,omitempty"`
}

// Logger is the slice of a logger this package needs.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

type nopLog struct{}

func (nopLog) Info(string, ...any) {}
func (nopLog) Warn(string, ...any) {}

var (
	mu      sync.Mutex
	path    string
	entries = map[int]Entry{}
)

// FileName is the pidfile's name under the engine's data dir.
const FileName = "harness-pids.json"

// Init points the tracker at dataDir and reaps whatever a previous engine
// process left in its pidfile. Call once at engine start, before any harness
// process is started. It returns the entries that were killed.
func Init(dataDir string, log Logger) []Entry {
	if log == nil {
		log = nopLog{}
	}
	mu.Lock()
	defer mu.Unlock()
	path = filepath.Join(dataDir, FileName)
	entries = map[int]Entry{}
	stale := readLocked()
	killed := reap(stale, log)
	writeLocked() // the file now describes only this engine
	return killed
}

// Register records a started command. It is a no-op until Init has run, or
// when the command is not running.
func Register(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	e := Entry{PID: pid, PGID: pid, Start: processStart(pid)}
	if len(cmd.Args) > 0 {
		e.Cmd = filepath.Base(cmd.Args[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if path == "" {
		return
	}
	entries[pid] = e
	writeLocked()
}

// Unregister forgets a command that has exited.
func Unregister(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := entries[cmd.Process.Pid]; !ok {
		return
	}
	delete(entries, cmd.Process.Pid)
	writeLocked()
}

// Tracked returns the currently recorded entries, for tests and diagnostics.
func Tracked() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	return out
}

func readLocked() []Entry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var es []Entry
	if json.Unmarshal(b, &es) != nil {
		return nil
	}
	return es
}

func writeLocked() {
	es := make([]Entry, 0, len(entries))
	for _, e := range entries {
		es = append(es, e)
	}
	b, err := json.Marshal(es)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// reap kills the process groups of entries that are verifiably still ours.
func reap(es []Entry, log Logger) []Entry {
	var killed []Entry
	for _, e := range es {
		if e.PID <= 1 || e.PGID <= 1 || e.PID == os.Getpid() {
			continue
		}
		if !ours(e) {
			continue
		}
		killGroup(e.PGID)
		log.Info("killed a harness process group left by a previous engine",
			"pid", e.PID, "pgid", e.PGID, "cmd", e.Cmd)
		killed = append(killed, e)
	}
	return killed
}

// ours decides whether e still names the process group it was recorded for.
func ours(e Entry) bool {
	start, alive := processStartAlive(e.PID)
	if alive {
		// The pid is held by a live process: it is the recorded one only if it
		// started at the same instant. An unknown start time is not a match.
		return e.Start != "" && start == e.Start
	}
	// Leader gone: its group may still hold children (the CLI's own
	// subprocesses). A group id cannot be reused while it has members, so
	// members found there are the old ones.
	return groupAlive(e.PGID)
}
