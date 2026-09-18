package instagram

// Talking to the Python helper.
//
// One child process, one request at a time, newline-delimited JSON over its
// stdin and stdout. Serialised deliberately: the pipe is a single stream with
// no multiplexing, and instagrapi's own pacing makes every call slow anyway —
// so a queue is both the simple implementation and the one that does not have
// two calls racing at an account that bans for looking automated.
//
// The helper's source is embedded rather than installed alongside the binary,
// because KARMAX ships as one file and an install that has to keep a .py next
// to the executable is one that breaks the first time somebody moves it.

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed helper/main.py
var helperSource []byte

//go:embed helper/install.sh
var installSource []byte

// callTimeout bounds one request. Generous: a login carries instagrapi's own
// 5–10s pacing plus however long Instagram takes, and a timeout that fires
// mid-login leaves the account's state ambiguous, which is worse than waiting.
const callTimeout = 3 * time.Minute

// Error is a failure the helper reported, carrying instagrapi's own exception
// name so an operator searching for it finds what everyone else found.
type Error struct {
	Type    string
	Message string
	// HardStop marks Instagram's anti-abuse signals. Never retry one: they mean
	// stop, and continuing past one risks the account rather than the call.
	HardStop bool
}

func (e *Error) Error() string {
	if e.HardStop {
		return fmt.Sprintf("instagram: %s — %s (stop; do not retry)", e.Type, e.Message)
	}
	return fmt.Sprintf("instagram: %s — %s", e.Type, e.Message)
}

type request struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		HardStop bool   `json:"hard_stop"`
	} `json:"error,omitempty"`
}

type helper struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
}

// helperDir is where the embedded script and the venv live, 0700 because the
// same directory holds the cached device fingerprint.
func helperDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".karmax", "instagram")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// materialise writes the embedded script to disk if what is there is not
// already it. Rewritten on change so an upgraded KARMAX does not keep running
// the previous version's helper.
func materialise(dir, name string, content []byte, mode os.FileMode) (string, error) {
	p := filepath.Join(dir, name)
	if old, err := os.ReadFile(p); err == nil && string(old) == string(content) {
		return p, nil
	}
	if err := os.WriteFile(p, content, mode); err != nil {
		return "", err
	}
	return p, nil
}

// start spawns the helper, installing its environment first if needed.
func (h *helper) start(ctx context.Context) error {
	if h.cmd != nil {
		return nil
	}
	dir, err := helperDir()
	if err != nil {
		return fmt.Errorf("instagram: no home directory to install into: %w", err)
	}
	python, err := ensureInstalled(ctx, dir)
	if err != nil {
		return err
	}
	script, err := materialise(dir, "main.py", helperSource, 0o600)
	if err != nil {
		return fmt.Errorf("instagram: could not write the helper: %w", err)
	}

	// Not exec.CommandContext: the helper's life is tied to the connector, not
	// to whichever call happened to start it.
	cmd := exec.Command(python, script)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("instagram: could not start the helper: %w", err)
	}

	// The helper's diagnostics, kept out of the protocol stream and off the
	// floor. Without this the pipe fills and the helper blocks on its own
	// logging, which looks exactly like Instagram being slow.
	go func() {
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			logf("instagram helper: %s", s.Text())
		}
	}()

	h.cmd, h.stdin, h.stdout = cmd, stdin, bufio.NewReader(stdout)
	return nil
}

// stop ends the helper. Any cached Instagram session goes with it, which is
// the point: nothing outlives the process holding the pipe.
func (h *helper) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kill()
}

func (h *helper) kill() {
	if h.cmd == nil {
		return
	}
	_ = h.stdin.Close()
	if p := h.cmd.Process; p != nil {
		_ = p.Kill()
	}
	_ = h.cmd.Wait()
	h.cmd, h.stdin, h.stdout = nil, nil, nil
}

// call sends one request and returns its result.
func (h *helper) call(ctx context.Context, method string, params map[string]any, out any) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.start(ctx); err != nil {
		return err
	}

	h.nextID++
	line, err := json.Marshal(request{ID: h.nextID, Method: method, Params: params})
	if err != nil {
		return err
	}
	if _, err := h.stdin.Write(append(line, '\n')); err != nil {
		// A closed pipe means the helper died. Clear it so the next call gets
		// a fresh one rather than writing into a corpse forever.
		h.kill()
		return fmt.Errorf("instagram: the helper stopped listening: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
		s, err := h.stdout.ReadString('\n')
		done <- read{s, err}
	}()

	select {
	case <-ctx.Done():
		// The reply may still be in flight, and reading it later would pair it
		// with the wrong request. A desynchronised stream is not recoverable,
		// so the process goes.
		h.kill()
		return fmt.Errorf("instagram: %s timed out", method)
	case r := <-done:
		if r.err != nil {
			h.kill()
			return fmt.Errorf("instagram: the helper stopped talking: %w", r.err)
		}
		var resp response
		if err := json.Unmarshal([]byte(strings.TrimSpace(r.line)), &resp); err != nil {
			h.kill()
			return fmt.Errorf("instagram: unreadable reply from the helper: %w", err)
		}
		if !resp.OK {
			if resp.Error == nil {
				return fmt.Errorf("instagram: %s failed, with no reason given", method)
			}
			return &Error{Type: resp.Error.Type, Message: resp.Error.Message, HardStop: resp.Error.HardStop}
		}
		if out == nil || len(resp.Result) == 0 {
			return nil
		}
		return json.Unmarshal(resp.Result, out)
	}
}
