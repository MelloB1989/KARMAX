package console

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/observe"
	"github.com/MelloB1989/karmax/fleet/internal/target"
)

// statusScript prints the status file's mtime, then the file. Same path
// fleetd reads (agent.snapshotScript).
const statusScript = `f="$HOME/.claude/fleet/status.json"; stat -c %Y "$f" && cat "$f"`

// StatusLine is one agent's last status-line snapshot.
type StatusLine struct {
	Model          string
	ContextPct     float64
	FiveHourPct    float64
	SevenDayPct    float64
	FiveHourResets time.Time
	At             time.Time // the file's mtime, not when it was fetched
}

type statuslineSrc struct {
	mu    sync.Mutex
	by    map[string]StatusLine
	trs   map[string]*tracker
	names []string
}

func newStatuslineSrc(live []string) *statuslineSrc {
	s := &statuslineSrc{by: map[string]StatusLine{}, trs: map[string]*tracker{}, names: live}
	for _, n := range live {
		s.trs[n] = newTracker(CadenceStatusline, false)
	}
	return s
}

func parseStatusOut(out []byte) (StatusLine, error) {
	first, rest, _ := strings.Cut(string(out), "\n")
	mtime, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return StatusLine{}, fmt.Errorf("status.json mtime %q", first)
	}
	st, err := observe.ParseStatus([]byte(rest))
	if err != nil {
		return StatusLine{}, fmt.Errorf("status.json: %w", err)
	}
	sl := StatusLine{Model: st.Model.DisplayName, ContextPct: st.ContextPct(), At: time.Unix(mtime, 0)}
	if sl.Model == "" {
		sl.Model = st.Model.ID
	}
	if w, ok := st.FiveHour(); ok {
		sl.FiveHourPct, sl.FiveHourResets = w.Used, w.Resets
	}
	if w, ok := st.SevenDay(); ok {
		sl.SevenDayPct = w.Used
	}
	return sl, nil
}

// poll reads every live agent's status line once.
func (s *statuslineSrc) poll(ctx context.Context, cs map[string]target.Container, now time.Time) {
	var wg sync.WaitGroup
	for _, n := range s.names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := cs[n].Sh(ctx, statusScript)
			var sl StatusLine
			if err == nil {
				sl, err = parseStatusOut(out)
			}
			if err != nil {
				s.trs[n].failure(err)
				return
			}
			s.mu.Lock()
			s.by[n] = sl
			s.mu.Unlock()
			s.trs[n].success(now)
		}()
	}
	wg.Wait()
}

func (s *statuslineSrc) get(name string) (StatusLine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.by[name]
	return v, ok
}

// source folds the per-agent trackers: ok only if every live agent is; At is
// the oldest last success.
func (s *statuslineSrc) source(now time.Time) Source {
	return foldSources(s.names, func(n string) Source { return s.trs[n].source(now) })
}

func foldSources(names []string, get func(string) Source) Source {
	out := Source{OK: true}
	var bad []string
	for _, n := range names {
		src := get(n)
		if !src.OK {
			out.OK = false
			msg := n
			if src.Error != "" {
				msg += ": " + src.Error
			} else {
				msg += ": stale"
			}
			bad = append(bad, msg)
		}
		if !src.At.IsZero() && (out.At.IsZero() || src.At.Before(out.At)) {
			out.At = src.At
		}
	}
	out.Error = strings.Join(bad, "; ")
	if len(names) == 0 {
		out.OK, out.Error = false, "no live agents"
	}
	return out
}
