// Package reflex is KARMAX's System One.
//
// Every event used to cost a full turn on a coding harness: minutes of latency
// and a large token bill to decide, often, that nothing needed doing. Reflex
// puts a cheap calibrated model in front of that. One evaluation per event
// answers the whole decision sheet at once — what to do with it, how urgent it
// is, what acting on it risks, whether it is worth remembering, how much
// machinery it deserves — and the expensive brain is woken only for the events
// that earned it.
//
// The contract that makes this safe to put on the hot path: reflex never fails
// closed. No key, a timeout, a rate limit, an unreadable answer — every one of
// those returns a verdict to handle the event exactly as KARMAX did before this
// package existed.
package reflex

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karma/ai/jev"
	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// Config configures the evaluator.
type Config struct {
	Enabled bool `yaml:"enabled"`
	// APIKey overrides TYPESAFE_API_KEY.
	APIKey string `yaml:"api_key"`
	// Model is a jev alias or a pinned version. Empty uses jev-latest.
	Model string `yaml:"model"`
	// Timeout bounds one screening. Short on purpose: reflex sits in front of
	// every event, so a slow answer is worse than no answer.
	Timeout time.Duration `yaml:"timeout"`
	// Thresholds turn the probabilities into decisions.
	Thresholds Thresholds `yaml:"thresholds"`
	// Kinds limits screening to these event kinds. Empty screens everything.
	Kinds []string `yaml:"kinds"`
}

// defaultTimeout bounds one screening call.
const defaultTimeout = 6 * time.Second

// breakerCooldown is how long reflex stops calling after repeated failures, so
// a TypeSafe outage costs one timeout per cooldown rather than one per event.
const breakerCooldown = 30 * time.Second

// breakerTrip is the consecutive-failure count that opens the breaker.
const breakerTrip = 3

// Evaluator screens events. Safe for concurrent use; nil is a working
// evaluator that fails open on everything.
type Evaluator struct {
	client     *jev.Client
	thresholds Thresholds
	timeout    time.Duration
	kinds      map[string]bool
	log        *zap.Logger

	// sink records every verdict for calibration. Optional.
	sink Sink

	screened atomic.Int64
	dropped  atomic.Int64
	failed   atomic.Int64
	tokens   atomic.Int64

	mu       sync.Mutex
	failures int
	openUnti time.Time
}

// Sink receives every verdict, for calibration and the status view.
type Sink func(evt bus.Event, v Verdict)

// New builds an evaluator. A disabled config, or a missing key, returns a nil
// evaluator and no error: reflex is an optimisation, and its absence must not
// stop the daemon.
func New(cfg Config, log *zap.Logger) (*Evaluator, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	opts := []jev.Option{}
	if cfg.APIKey != "" {
		opts = append(opts, jev.WithAPIKey(cfg.APIKey))
	}
	if cfg.Model != "" {
		opts = append(opts, jev.WithModel(cfg.Model))
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	opts = append(opts, jev.WithTimeout(timeout))

	client, err := jev.New(opts...)
	if err != nil {
		if errors.Is(err, jev.ErrNoAPIKey) {
			log.Warn("reflex is enabled but has no TypeSafe key; every event will be handled the old way")
			return nil, nil
		}
		return nil, err
	}

	var kinds map[string]bool
	if len(cfg.Kinds) > 0 {
		kinds = make(map[string]bool, len(cfg.Kinds))
		for _, k := range cfg.Kinds {
			kinds[k] = true
		}
	}
	return &Evaluator{
		client:     client,
		thresholds: cfg.Thresholds.withDefaults(),
		timeout:    timeout,
		kinds:      kinds,
		log:        log,
	}, nil
}

// SetSink installs the verdict recorder.
func (e *Evaluator) SetSink(s Sink) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.sink = s
	e.mu.Unlock()
}

// Screen decides what to do with one event. It never returns an error: every
// failure is a verdict to handle the event the old way.
func (e *Evaluator) Screen(ctx context.Context, evt bus.Event, hint Hint) Verdict {
	if e == nil || e.client == nil {
		return openVerdict("reflex disabled")
	}
	if e.kinds != nil && !e.kinds[string(evt.Kind)] {
		return openVerdict("kind not screened")
	}
	if e.breakerOpen() {
		return openVerdict("reflex breaker open")
	}

	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	res, err := e.client.Evaluate(callCtx, StateOf(evt, hint), Sheet())
	elapsed := time.Since(started)

	var v Verdict
	if err != nil {
		e.recordFailure()
		e.failed.Add(1)
		v = openVerdict("evaluate: " + err.Error())
		e.log.Warn("reflex could not screen an event; handling it the old way",
			zap.String("kind", string(evt.Kind)), zap.String("event", evt.ID),
			zap.Duration("elapsed", elapsed), zap.Error(err))
	} else {
		e.recordSuccess()
		v = Decide(res, hint, e.thresholds)
		e.tokens.Add(int64(res.Usage.InputTokens))
		v.InputTokens = res.Usage.InputTokens
	}
	v.Elapsed = elapsed

	e.screened.Add(1)
	if v.Action == ActionDrop {
		e.dropped.Add(1)
	}

	e.log.Debug("reflex verdict",
		zap.String("kind", string(evt.Kind)), zap.String("event", evt.ID),
		zap.String("action", string(v.Action)), zap.String("effort", string(v.Effort)),
		zap.Float64("confidence", v.Confidence), zap.Float64("urgency", v.Urgency),
		zap.Float64("risk", v.Risk), zap.Bool("operator", hint.Operator),
		zap.Bool("failed_open", v.FailedOpen), zap.Duration("elapsed", elapsed))

	e.mu.Lock()
	sink := e.sink
	e.mu.Unlock()
	if sink != nil {
		sink(evt, v)
	}
	return v
}

// Ask evaluates an arbitrary state against arbitrary questions. This is what a
// loop reaches through: the questions are the loop's, not KARMAX's, so a loop
// can decide whether to reply, how to triage, or anything else it can phrase —
// without spending a model turn.
func (e *Evaluator) Ask(ctx context.Context, state any, qs jev.Questions) (*jev.Result, error) {
	if e == nil || e.client == nil {
		return nil, ErrUnavailable
	}
	if e.breakerOpen() {
		return nil, ErrUnavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	res, err := e.client.Evaluate(callCtx, state, qs)
	if err != nil {
		e.recordFailure()
		e.failed.Add(1)
		return nil, err
	}
	e.recordSuccess()
	e.screened.Add(1)
	e.tokens.Add(int64(res.Usage.InputTokens))
	return res, nil
}

// ErrUnavailable means reflex is off, unconfigured, or in cooldown. A caller
// that gets it should do whatever it did before reflex existed.
var ErrUnavailable = errors.New("reflex: unavailable")

// Available reports whether a screening would actually reach the model.
func (e *Evaluator) Available() bool {
	return e != nil && e.client != nil && !e.breakerOpen()
}

// Stats is the status view.
type Stats struct {
	Screened    int64 `json:"screened"`
	Dropped     int64 `json:"dropped"`
	Failed      int64 `json:"failed"`
	InputTokens int64 `json:"input_tokens"`
	BreakerOpen bool  `json:"breaker_open"`
}

// Stats reports what reflex has done.
func (e *Evaluator) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	return Stats{
		Screened:    e.screened.Load(),
		Dropped:     e.dropped.Load(),
		Failed:      e.failed.Load(),
		InputTokens: e.tokens.Load(),
		BreakerOpen: e.breakerOpen(),
	}
}

func (e *Evaluator) breakerOpen() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return time.Now().Before(e.openUnti)
}

func (e *Evaluator) recordFailure() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures++
	if e.failures >= breakerTrip {
		e.openUnti = time.Now().Add(breakerCooldown)
		e.failures = 0
		e.log.Warn("reflex is failing; pausing screening",
			zap.Duration("cooldown", breakerCooldown))
	}
}

func (e *Evaluator) recordSuccess() {
	e.mu.Lock()
	e.failures = 0
	e.mu.Unlock()
}

// Thresholds reports the cuts this evaluator is using, for the status view.
func (e *Evaluator) Thresholds() Thresholds {
	if e == nil {
		return DefaultThresholds
	}
	return e.thresholds
}
