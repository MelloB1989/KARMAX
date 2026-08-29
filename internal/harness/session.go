package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Session is one live conversation with a harness process.
//
// Exactly one turn may be in flight at a time. The protocol carries no request
// ids, so two overlapping sends would interleave their events on one stdout and
// neither could be attributed — the mutex is what makes a turn parseable, not
// merely what makes it tidy.
type Session struct {
	Key   string
	Kind  string
	ID    string // the CLI's session uuid, for --resume
	Model string

	cmd    *exec.Cmd
	stdin  *bufio.Writer
	events chan event
	closed chan struct{}
	// exited is closed once the process has been reaped. Signal-0 liveness is
	// not enough on its own: a killed process that nobody has waited on is a
	// zombie, and the kernel still answers "yes, that pid is yours" — so a
	// crashed session looked healthy and the next write hit a dead pipe.
	exited chan struct{}

	mu   sync.Mutex // one turn at a time
	once sync.Once
}

// spawn starts a harness process for this session.
//
// resume decides which of the two mutually exclusive session flags is used: the
// CLI rejects --session-id together with --resume, so a revived session passes
// only --resume and a new one only --session-id. Minting the uuid ourselves is
// what makes the session addressable before it has said anything.
func spawn(ctx context.Context, bin string, s *Session, workdir string, resume bool, env []string) error {
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // stream-json emits nothing without it
		"--dangerously-skip-permissions",
	}
	if resume {
		args = append(args, "--resume", s.ID)
	} else {
		args = append(args, "--session-id", s.ID)
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("harness workdir: %w", err)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = workdir
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("harness stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("harness stdout: %w", err)
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start harness: %w", err)
	}

	s.cmd = cmd
	s.stdin = bufio.NewWriter(stdin)
	s.events = make(chan event, 64)
	s.closed = make(chan struct{})
	s.exited = make(chan struct{})

	go func() {
		defer close(s.events)
		// Reaping happens here, after stdout has been drained: waiting earlier
		// would race the reads, and never waiting leaves a zombie per session.
		defer func() {
			_ = cmd.Wait()
			close(s.exited)
		}()
		sc := bufio.NewScanner(stdout)
		// A single event can carry a whole tool result, which is far larger
		// than the default 64KB line budget.
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue // an unknown line is not a reason to kill a session
			}
			select {
			case s.events <- ev:
			case <-s.closed:
				return
			}
		}
	}()
	return nil
}

// Send asks one question and reads until the turn completes.
//
// The result event is the only reliable delimiter: text arrives in pieces, tool
// calls interleave, and nothing else says "this exchange is over".
func (s *Session) Send(ctx context.Context, text string, timeout time.Duration) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	line, err := userEvent(text)
	if err != nil {
		return Turn{}, err
	}
	if _, err := s.stdin.Write(line); err != nil {
		return Turn{}, fmt.Errorf("harness stdin closed: %w", err)
	}
	if err := s.stdin.Flush(); err != nil {
		return Turn{}, fmt.Errorf("harness stdin flush: %w", err)
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var turn Turn
	var sb strings.Builder
	for {
		select {
		case <-ctx.Done():
			return turn, ctx.Err()
		case <-deadline.C:
			// The session is not trustworthy after a timeout: the process may
			// still be mid-turn and its next events would land on the wrong
			// question. The caller resumes, which starts a clean process on the
			// same transcript.
			return turn, fmt.Errorf("harness turn timed out after %s", timeout)
		case ev, ok := <-s.events:
			if !ok {
				return turn, fmt.Errorf("harness exited mid-turn")
			}
			switch ev.Type {
			case "system":
				if ev.SessionID != "" {
					s.ID = ev.SessionID // authoritative, in case the CLI reassigns
				}
			case "rate_limit_event":
				turn.Limits = ev.RateLimitInfo
			case "assistant":
				for _, c := range ev.Message.Content {
					switch c.Type {
					case "text":
						sb.WriteString(c.Text)
					case "tool_use":
						turn.ToolCalls = append(turn.ToolCalls, ToolCall{
							Name:    c.Name,
							Input:   c.Input,
							Command: shellCommand(c.Name, c.Input),
						})
					}
				}
			case "result":
				turn.Usage = ev.Usage
				turn.CostUSD = ev.TotalCostUSD
				turn.NumTurns = ev.NumTurns
				if ev.Result != "" {
					turn.Text = ev.Result
				} else {
					turn.Text = strings.TrimSpace(sb.String())
				}
				if ev.IsError {
					turn.Err = fmt.Errorf("harness error: %s", firstNonEmpty(ev.APIErrorState, ev.Subtype))
				}
				return turn, turn.Err
			}
		}
	}
}

// Close stops the process, politely then not.
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.closed)
		if s.stdin != nil {
			_ = s.stdin.Flush()
		}
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		_ = s.cmd.Process.Signal(os.Interrupt)
		// The reader goroutine owns Wait; this only waits for it to finish.
		select {
		case <-s.exited:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
		}
	})
}

// Alive reports whether the process is still running.
func (s *Session) Alive() bool {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	select {
	case <-s.closed:
		return false
	case <-s.exited:
		return false
	default:
	}
	// Signal 0 asks the kernel whether the pid is still ours without disturbing
	// it. It must be syscall.Signal(0), not nil: Go type-asserts the argument to
	// syscall.Signal, so a nil signal fails that assertion and returns EINVAL —
	// which read as "dead" for every healthy process, so every send respawned
	// the session it should have reused and paid a cold start to do it.
	return s.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// PID is the process id, for the orphan sweep after a restart.
func (s *Session) PID() int {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "unknown"
}
