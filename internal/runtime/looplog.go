package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/safety"
	"go.uber.org/zap"
)

// A loop's log, one write per run.
//
// What a loop remembers about its runs — Kit.Remember, a signed loop's
// remember host call — is its log (memory.CategoryLog). Written a line at a
// time it was most of what the store was asked to write: 343 of one night's
// 550 writes, chat-sweep alone 3.7 a run, mostly lines saying a chat was still
// waiting. Every write makes the store re-upload its whole repository.
//
// So a run's lines are held until the run ends and filed as ONE entry, whether
// the run succeeded or failed. A line that says what the loop's previous run
// said, but for when it was said, is left out (memory.LogLineKey), so a run
// with nothing new to say writes nothing. This is the runtime's, for every
// loop: no loop opts in and none is named.

const (
	// logRunGroup keeps a run's lines until they are filed, so the lines of a
	// run the daemon died in are filed by the loop's next run.
	logRunGroup = "looplog-run"
	// logLastGroup keeps, per loop, the keys of what its last run said.
	logLastGroup = "looplog-last"
	logStateTTL  = 30 * 24 * time.Hour
)

// runLog is the lines one run of a loop has remembered so far.
type runLog struct {
	loop    string
	started time.Time

	mu     sync.Mutex
	lines  []string
	closed bool
}

// savedRunLog is a run's lines as they are kept until filed.
type savedRunLog struct {
	Started time.Time `json:"started"`
	Lines   []string  `json:"lines"`
}

// beginRunLog opens a run's log. Lines left by a run the daemon died in are
// filed first, as that run's.
func (rt *KarmaxRuntime) beginRunLog(loop string) *runLog {
	if raw, ok, _ := rt.store.KVGet(logRunGroup, loop); ok {
		var left savedRunLog
		if json.Unmarshal([]byte(raw), &left) == nil && len(left.Lines) > 0 {
			rt.log.Info("filing the log of a run the daemon stopped in", zap.String("loop", loop), zap.Int("lines", len(left.Lines)))
			rt.fileRunLog(loop, left.Started, left.Started.Add(loopRunTimeout), left.Lines, true)
		} else {
			_ = rt.store.KVDelete(logRunGroup, loop)
		}
	}
	rl := &runLog{loop: loop, started: time.Now()}
	rt.runLogs.Store(loop, rl)
	return rl
}

// logLine takes a line for the loop's run in flight. held is false when no
// run of the loop is in flight — a tool a loop lends the agent, say — and the
// caller writes the line itself.
func (rt *KarmaxRuntime) logLine(loop, line string) (held bool, err error) {
	v, ok := rt.runLogs.Load(loop)
	if !ok {
		return false, nil
	}
	rl := v.(*runLog)
	line = strings.TrimSpace(line)
	if line == "" {
		return true, nil
	}
	// Checked line by line, as each write was: one refused line must not
	// cost the run its whole log.
	if err := safety.CheckWrite(line); err != nil {
		return true, err
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.closed {
		return false, nil
	}
	rl.lines = append(rl.lines, line)
	if raw, err := json.Marshal(savedRunLog{Started: rl.started, Lines: rl.lines}); err == nil {
		if err := rt.store.KVSet(logRunGroup, loop, string(raw), logStateTTL); err != nil {
			rt.log.Warn("could not keep a loop's log line until its run ends", zap.String("loop", loop), zap.Error(err))
		}
	}
	return true, nil
}

// endRunLog files what the run said, in one write.
func (rt *KarmaxRuntime) endRunLog(rl *runLog, failed bool) {
	rt.runLogs.CompareAndDelete(rl.loop, rl)
	rl.mu.Lock()
	rl.closed = true
	lines := rl.lines
	rl.mu.Unlock()
	rt.fileRunLog(rl.loop, rl.started, time.Now(), lines, failed)
}

// fileRunLog writes a run's new lines as one entry in the loop's log and
// remembers what the run said, for the next run to compare with.
//
// A failed run's lines are added to what the previous run said rather than
// replacing it: a run that stopped part-way did not find the rest unchanged.
func (rt *KarmaxRuntime) fileRunLog(loop string, from, to time.Time, lines []string, failed bool) {
	defer func() { _ = rt.store.KVDelete(logRunGroup, loop) }()
	prev := rt.lastRunSaid(loop)
	said := map[string]bool{}
	var news []string
	for _, l := range lines {
		k := lineKey(memory.LogLineKey(l, from, to, time.Local))
		if !prev[k] && !said[k] {
			news = append(news, l)
		}
		said[k] = true
	}
	if e, ok := memory.LoopRunEntry(loop, news, from); ok {
		mem := rt.loopMemory()
		if mem == nil {
			return
		}
		if err := mem.Write(e); err != nil {
			// Not recorded as said, so the next run that says it writes it.
			rt.log.Warn("could not file a loop run's log", zap.String("loop", loop), zap.Int("lines", len(news)), zap.Error(err))
			return
		}
	}
	if failed {
		for k := range prev {
			said[k] = true
		}
	}
	keys := make([]string, 0, len(said))
	for k := range said {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	raw, _ := json.Marshal(keys)
	if err := rt.store.KVSet(logLastGroup, loop, string(raw), logStateTTL); err != nil {
		rt.log.Warn("could not keep what a loop's run said", zap.String("loop", loop), zap.Error(err))
	}
}

// lastRunSaid is the keys of what the loop's last run said.
func (rt *KarmaxRuntime) lastRunSaid(loop string) map[string]bool {
	out := map[string]bool{}
	raw, ok, err := rt.store.KVGet(logLastGroup, loop)
	if err != nil || !ok {
		return out
	}
	var keys []string
	_ = json.Unmarshal([]byte(raw), &keys)
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// lineKey keeps a line's key short enough to store a run's worth of them.
func lineKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// loopMemory is the memory loops remember into.
func (rt *KarmaxRuntime) loopMemory() *memory.Manager {
	if rt.memory == nil {
		return nil
	}
	return rt.memory.For(rt.loopDefaultAgent, rt.loopNamespace())
}
