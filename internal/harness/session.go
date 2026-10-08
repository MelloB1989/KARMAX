package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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
	// Pinned says Model was asked for by a turn rather than chosen by policy,
	// so a later turn that asks for no model knows to go back to the default.
	Pinned bool
	// Effort is the CLI's --effort level (low|medium|high|xhigh|max). Fixed at
	// spawn exactly like Model — the CLI reads it once, on the way up, and a
	// running process cannot be told to think harder mid-turn.
	Effort string
	// Thinking is fixed when the process spawns, so like Model it takes
	// effect on a new session or on the next resume — never mid-conversation.
	Thinking bool
	// MCPConfig is threaded from Options the same way Model and Thinking are,
	// taking effect on the next spawn.
	MCPConfig string
	// Name and Launch come from the kind's policy, fixed at spawn.
	Name   string
	Launch []string

	cmd    *exec.Cmd
	stdin  *bufio.Writer
	pipe   io.WriteCloser // stdin's own end, closed by Close to send EOF
	events chan event
	closed chan struct{}
	// exited is closed once the process has been reaped. Signal-0 liveness is
	// not enough on its own: a killed process that nobody has waited on is a
	// zombie, and the kernel still answers "yes, that pid is yours" — so a
	// crashed session looked healthy and the next write hit a dead pipe.
	exited chan struct{}

	mu   sync.Mutex // one turn at a time
	once sync.Once

	// writeMu guards stdin specifically — separate from mu, which Send holds
	// for an entire turn (routinely minutes). Close must be able to flush
	// and interrupt without waiting out whatever turn is currently running,
	// so it cannot take mu; but Close's own Flush and Send's Write+Flush
	// both call methods on the same *bufio.Writer, which is not safe for
	// concurrent use on its own. writeMu is held only around those two brief
	// operations — never across a whole turn — so Close stays non-blocking
	// while the writer itself is never touched by two goroutines at once.
	writeMu sync.Mutex

	// route is how the reader hands events out. A Send that is waiting gets
	// them; otherwise they are a turn the session started by itself — another
	// session's message arriving while this one was idle — and are assembled
	// here and given to onBackground. Without this, such a turn filled the
	// event buffer with nobody reading it, stalled the process, and its result
	// was handed to the next caller as their reply.
	route        sync.Mutex
	waiting      bool       // a Send is reading events
	bg           *bgTurn    // an unsolicited turn in progress
	prelude      []event    // housekeeping lines seen between turns
	onBackground func(Turn) // set before spawn; nil drops background turns

	// busy is true while a turn is in flight.
	//
	// Needed because the only other signal of activity is the stored
	// LastActivityAt, which is written when a turn FINISHES. A session part-way
	// through a long piece of work therefore looks like the least recently used
	// one in the table, and both the eviction and the idle reaper would close
	// it — which is exactly what "harness exited mid-turn" was.
	busy atomic.Bool
}

// extraArgs returns the flags granting the tools in opt, for when MCPConfig
// is set.
func extraArgs(opt Options) []string {
	var args []string
	if opt.MCPConfig != "" {
		args = append(args, "--mcp-config", opt.MCPConfig)
	}
	return args
}

// spawnArgs assembles the CLI's argument list.
//
// Split out of spawn so ordering can be tested without starting a process.
func spawnArgs(s *Session, resume bool, fallbackModel string) []string {
	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", // stream-json emits nothing without it
		"--dangerously-skip-permissions",
		// Without this, text arrives per completed block; with it, per token.
		// The chat is the only caller that shows text as it lands.
		"--include-partial-messages",
	}
	if resume {
		args = append(args, "--resume", s.ID)
	} else {
		args = append(args, "--session-id", s.ID)
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
	}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	// The CLI's own degradation, one layer below the breaker's. The breaker
	// acts on the account's published quota between turns; this catches a
	// single model being overloaded DURING one, where there is nothing for
	// KARMAX to observe and react to in time.
	if fallbackModel != "" && fallbackModel != s.Model {
		args = append(args, "--fallback-model", fallbackModel)
	}
	// No positional prompt here to collide with; still appended last, and tested.
	return append(args, extraArgs(Options{MCPConfig: s.MCPConfig})...)
}

// spawn starts a harness process for this session.
//
// resume decides which of the two mutually exclusive session flags is used: the
// CLI rejects --session-id together with --resume, so a revived session passes
// only --resume and a new one only --session-id. Minting the uuid ourselves is
// what makes the session addressable before it has said anything.
func spawn(ctx context.Context, bin string, s *Session, workdir string, resume bool, env []string, fallbackModel string) error {
	args := spawnArgs(s, resume, fallbackModel)

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("harness workdir: %w", err)
	}

	name, argv := launchArgv(s.Launch, workdir, bin, args)
	cmd := exec.Command(name, argv...)
	cmd.Dir = workdir
	cmd.Env = env
	if s.Thinking {
		// Extended thinking is off unless the child is given a budget for it.
		cmd.Env = append(cmd.Env, "MAX_THINKING_TOKENS=8000")
	}

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
	s.pipe = stdin
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
		defer s.endBackground()
		sc := bufio.NewScanner(stdout)
		// A single event can carry a whole tool result, which is far larger
		// than the default 64KB line budget.
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue // an unknown line is not a reason to kill a session
			}
			if !s.dispatch(ev) {
				return
			}
		}
	}()
	return nil
}

// bgTurn is an unsolicited turn being assembled by the reader.
type bgTurn struct {
	turn Turn
	text strings.Builder
	done chan struct{} // closed when the turn ends, or the process does
}

// maxPrelude caps the housekeeping lines kept between turns.
const maxPrelude = 32

// opensTurn reports whether an event can only be part of a turn. Anything
// else seen between turns (system notices, rate-limit updates) is
// housekeeping, and must never make the next caller wait.
func opensTurn(ev event) bool {
	switch ev.Type {
	case "assistant", "user", "stream_event":
		return true
	}
	return false
}

// dispatch hands one event to a waiting Send, or folds it into a background
// turn. It reports false once the session is closed.
func (s *Session) dispatch(ev event) bool {
	s.route.Lock()
	if s.waiting {
		s.route.Unlock()
		select {
		case s.events <- ev:
			return true
		case <-s.closed:
			return false
		}
	}
	if s.bg == nil {
		if !opensTurn(ev) {
			// A result with no turn open closes nothing; drop it.
			if ev.Type != "result" {
				if len(s.prelude) == maxPrelude {
					s.prelude = s.prelude[1:]
				}
				s.prelude = append(s.prelude, ev)
			}
			s.route.Unlock()
			return true
		}
		s.bg = &bgTurn{done: make(chan struct{})}
		for _, p := range s.prelude {
			s.bg.turn.absorb(p)
		}
		s.prelude = nil
	}
	b := s.bg
	b.turn.absorb(ev)
	if ev.Type == "assistant" {
		for _, c := range ev.Message.Content {
			if c.Type == "text" {
				b.text.WriteString(c.Text)
			}
		}
	}
	if ev.Type != "result" {
		s.route.Unlock()
		return true
	}
	s.bg = nil
	s.route.Unlock()
	if ev.Result == "" {
		b.turn.Text = strings.TrimSpace(b.text.String())
	}
	close(b.done)
	// Off the reader: a handler that sends to this same session would
	// otherwise wait on events nobody is left to read.
	if s.onBackground != nil {
		go s.onBackground(b.turn)
	}
	return true
}

// endBackground releases anyone waiting on a background turn when the
// process goes; that turn is never finished.
func (s *Session) endBackground() {
	s.route.Lock()
	defer s.route.Unlock()
	if s.bg != nil {
		close(s.bg.done)
		s.bg = nil
	}
}

// inBackground reports whether an unsolicited turn is in progress.
func (s *Session) inBackground() bool {
	s.route.Lock()
	defer s.route.Unlock()
	return s.bg != nil
}

// launchArgv puts a kind's launch prefix before the binary, filling in
// {workdir}. Without a prefix it is the binary and its arguments unchanged.
func launchArgv(prefix []string, workdir, bin string, args []string) (string, []string) {
	if len(prefix) == 0 {
		return bin, args
	}
	argv := make([]string, 0, len(prefix)+len(args))
	for _, p := range prefix[1:] {
		argv = append(argv, strings.ReplaceAll(p, "{workdir}", workdir))
	}
	argv = append(argv, bin)
	return strings.ReplaceAll(prefix[0], "{workdir}", workdir), append(argv, args...)
}

// EventKind is one thing a harness can say while a turn is running.
type EventKind string

const (
	KindMessage    EventKind = "message"     // the reply, as deltas
	KindThought    EventKind = "thought"     // reasoning, when it is enabled
	KindTool       EventKind = "tool"        // a call, announced
	KindToolUpdate EventKind = "tool_update" // the same call, resolved
	KindPlan       EventKind = "plan"
)

// Event is one thing worth telling a caller while a turn is still running.
//
// Shaped after ACP's SessionUpdate rather than after any one CLI's output, so
// that a second harness is an adapter rather than a second vocabulary. The set
// is still narrower than the wire's: "conversation" and "done" are the
// endpoint's, added there because the harness has no business knowing either.
type Event struct {
	Kind EventKind

	// Text carries KindMessage and KindThought. The wire's own "error" kind
	// is written directly by internal/api/chat.go, never through an Event.
	Text string

	// Tool is set for KindTool and KindToolUpdate.
	Tool *ToolEvent

	// Plan replaces the whole plan each time. The agent revises it wholesale,
	// and merging entry by entry would invent a history it does not have.
	Plan []PlanEntry

	JobID string
}

// emit hands one parsed CLI event to the sink, if there is one.
//
// Stateless on purpose. A tool call is announced from the assistant message,
// which already carries its complete input, and resolved from the tool_result
// that follows — so nothing has to be remembered between events, and an
// update carrying only an id is merged by whoever is keeping the transcript.
func emit(sink func(Event), ev event) {
	if sink == nil {
		return
	}
	switch ev.Type {
	case "stream_event":
		if ev.StreamEvent.Type != "content_block_delta" {
			return
		}
		switch ev.StreamEvent.Delta.Type {
		case "text_delta":
			// Empty deltas arrive under subscription auth (no actual text to forward) or mid-streaming;
			// both are waste: empty traffic for no words, and a JSON encode-decode for the sink each.
			if ev.StreamEvent.Delta.Text != "" {
				sink(Event{Kind: KindMessage, Text: ev.StreamEvent.Delta.Text})
			}
		case "thinking_delta":
			// Same as text_delta: subscription auth sends empty thinking blocks by the dozen.
			if ev.StreamEvent.Delta.Thinking != "" {
				sink(Event{Kind: KindThought, Text: ev.StreamEvent.Delta.Thinking})
			}
		}

	case "assistant":
		// Text is not emitted here: the deltas above already streamed it, and
		// this block is that same text again, sent whole — emitting it too
		// would double every reply.
		for _, c := range ev.Message.Content {
			if c.Type != "tool_use" {
				continue
			}
			// The plan is the useful artifact; a line saying "kept track" is
			// not. The tool_result that follows is dropped by the consumer,
			// which ignores updates for calls it never saw announced.
			if c.Name == "TodoWrite" {
				if plan := planFrom(c.Input); plan != nil {
					sink(Event{Kind: KindPlan, Plan: plan})
				}
				continue
			}
			sink(Event{Kind: KindTool, Tool: &ToolEvent{
				ID:        c.ID,
				Title:     toolTitle(c.Name, c.Input),
				Kind:      toolKind(c.Name),
				Status:    StatusInProgress,
				Locations: toolLocations(c.Name, c.Input),
				Input:     truncateJSONStrings(c.Input),
			}})
		}

	case "user":
		for _, c := range ev.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			status := StatusCompleted
			if c.IsError {
				status = StatusFailed
			}
			sink(Event{Kind: KindToolUpdate, Tool: &ToolEvent{
				ID:     c.ToolUseID,
				Status: status,
				Output: truncateOutput(toolResultText(c.Content)),
			}})
		}
	}
}

// replay drives emit over a fixed list, so the sink can be tested without a
// subprocess.
func replay(sink func(Event), evs []event) {
	for _, ev := range evs {
		emit(sink, ev)
	}
}

// absorb folds one CLI event into the turn being assembled.
//
// Split out of Send so the turn's own bookkeeping can be tested without a
// subprocess: Send owns the loop and the timeouts, this owns the fields.
func (t *Turn) absorb(ev event) {
	switch ev.Type {
	case "rate_limit_event":
		t.Limits = ev.RateLimitInfo
	case "assistant":
		if ev.Message.Model != "" {
			t.Model = ev.Message.Model
		}
		for _, c := range ev.Message.Content {
			if c.Type == "tool_use" {
				t.ToolCalls = append(t.ToolCalls, ToolCall{
					Name:    c.Name,
					Input:   c.Input,
					Command: shellCommand(c.Name, c.Input),
				})
			}
		}
	case "result":
		t.Usage = ev.Usage
		t.CostUSD = ev.TotalCostUSD
		t.NumTurns = ev.NumTurns
		t.Duration = time.Duration(ev.DurationMS) * time.Millisecond
		if ev.Result != "" {
			t.Text = ev.Result
		}
		if ev.IsError {
			t.Err = fmt.Errorf("harness error: %s", firstNonEmpty(ev.APIErrorState, ev.Subtype))
		}
	}
}

// Send asks one question and reads until the turn completes.
//
// The result event is the only reliable delimiter: text arrives in pieces, tool
// calls interleave, and nothing else says "this exchange is over".
func (s *Session) Send(ctx context.Context, text string, timeout time.Duration, sink func(Event)) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy.Store(true)
	defer s.busy.Store(false)

	line, err := userEvent(text)
	if err != nil {
		return Turn{}, err
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	var turn Turn
	// A turn the session started by itself is finished before this one is
	// written, so neither one's events land on the other.
	for {
		s.route.Lock()
		if s.bg == nil {
			s.waiting = true
			for _, p := range s.prelude {
				turn.absorb(p)
			}
			s.prelude = nil
			s.route.Unlock()
			break
		}
		done := s.bg.done
		s.route.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return turn, ctx.Err()
		case <-deadline.C:
			return turn, fmt.Errorf("harness: a background turn was still running after %s", timeout)
		}
	}
	defer func() {
		s.route.Lock()
		s.waiting = false
		s.route.Unlock()
	}()
	// Lines that reached the buffer after the last turn ended belong to no
	// turn. Housekeeping is kept; anything else would be read as this turn's.
	for drained := false; !drained; {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return turn, fmt.Errorf("harness exited between turns")
			}
			if !opensTurn(ev) && ev.Type != "result" {
				turn.absorb(ev)
			}
		default:
			drained = true
		}
	}
	// The wait above is not this turn's own time.
	if !deadline.Stop() {
		<-deadline.C
	}
	deadline.Reset(timeout)

	// Held only around the write itself, not the turn that follows: a Close
	// racing in here waits a few instructions, never minutes.
	s.writeMu.Lock()
	_, writeErr := s.stdin.Write(line)
	var flushErr error
	if writeErr == nil {
		flushErr = s.stdin.Flush()
	}
	s.writeMu.Unlock()
	if writeErr != nil {
		return Turn{}, fmt.Errorf("harness stdin closed: %w", writeErr)
	}
	if flushErr != nil {
		return Turn{}, fmt.Errorf("harness stdin flush: %w", flushErr)
	}

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
			emit(sink, ev)
			turn.absorb(ev)
			switch ev.Type {
			case "system":
				if ev.SessionID != "" {
					s.ID = ev.SessionID // authoritative, in case the CLI reassigns
				}
			case "assistant":
				// The builder is the loop's own running total; absorb has no
				// access to it and only fills Text from a non-empty result.
				for _, c := range ev.Message.Content {
					if c.Type == "text" {
						sb.WriteString(c.Text)
					}
				}
			case "result":
				if ev.Result == "" {
					turn.Text = strings.TrimSpace(sb.String())
				}
				return turn, turn.Err
			}
		}
	}
}

// Close stops the process, politely then not.
//
// Never takes mu: Send holds it for a whole turn, sometimes minutes, and
// Close has to be able to interrupt that turn rather than wait it out — that
// is the entire reason a caller reaches for Close instead of just letting
// the turn finish. What it does take is writeMu, for exactly as long as its
// own Flush call: bufio.Writer is not safe for concurrent use, and without
// this, this Flush races Send's own Write+Flush on the same writer whenever
// Close is called on a session with a turn in flight — a genuine, -race
// -detected data race, independent of whether killing that turn was the
// right call (see the harness.close and harness.model call sites for that
// judgement).
//
// EOF goes first. It is the polite exit for a stream-json session, and the only
// one that crosses a launch prefix: a signal reaches the docker (or ssh) client,
// not the CLI behind it, and killing the client can strand the CLI wherever it
// runs. So a launched session is given launchedExitGrace to exit on EOF before
// it is signalled; a local one is signalled at once, as it always was, because
// Close is also how a running turn is interrupted.
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.closed)
		if s.stdin != nil {
			s.writeMu.Lock()
			_ = s.stdin.Flush()
			if s.pipe != nil {
				_ = s.pipe.Close()
			}
			s.writeMu.Unlock()
		}
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		if len(s.Launch) > 0 {
			select {
			case <-s.exited:
				return
			case <-time.After(launchedExitGrace):
			}
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

// launchedExitGrace is how long Close waits for a launched session to exit on
// EOF before signalling it.
const launchedExitGrace = 5 * time.Second

// Busy reports whether a turn is in flight, so nothing closes a session that is
// still working.
//
// A turn the session started by itself counts: closing it mid-turn would lose
// whatever another session asked of it.
func (s *Session) Busy() bool { return s != nil && (s.busy.Load() || s.inBackground()) }

// claim marks the session busy before Send has actually been called on it.
// Supervisor.open uses this the moment it decides to hand a session back
// for reuse (or hands back a freshly spawned one), while it still holds its
// own lock — so a concurrent CloseIfIdle, which checks Busy under that same
// lock, can never see the session as idle in the gap between open returning
// it and the caller's own call to Send actually starting. Send's own
// busy.Store(true) is then a harmless, idempotent confirmation once the turn
// is genuinely under way; its defer busy.Store(false) is still what clears
// the claim when the turn ends.
func (s *Session) claim() { s.busy.Store(true) }

// release undoes a claim for a session opened with no turn to run, such as a
// revived resident one.
func (s *Session) release() { s.busy.Store(false) }

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
