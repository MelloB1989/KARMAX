package karmahelper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Inference on Claude Code.
//
// A session whose provider is ProviderClaudeCode never reaches a model API. Its
// turns run inside a warm Claude Code session, the same engine the agent's own
// brain already runs on, so everything KARMAX thinks with draws on one account
// and one set of credentials.
//
// Tools are the part that needed building. An API session hands the model its
// tools as function schemas; a Claude Code session has a shell instead. So a
// session's tools are registered for the length of one turn under a random
// token, the prompt tells the model to call them with
// `karmax tool call --turn <token> <name> …`, and the API resolves that token
// back to exactly these tools — including ones that exist only inside this
// session, like the memory sub-agent's tree navigation, which the global
// registry has never heard of. Calls made that way are recorded, so what the
// turn did is as visible as it was on the API path.

// ProviderClaudeCode routes a session through Claude Code.
const ProviderClaudeCode = "claude-code"

// ClaudeCodeTurn is one turn handed to the runner.
type ClaudeCodeTurn struct {
	// Key names the Claude Code session. The same key continues the same
	// conversation, so a session that sets SessionConfig.SessionKey is warm
	// across instances and one that does not gets a conversation of its own.
	Key string
	// Kind is the session's usage kind — main, memory, summary, voice… — and
	// is what the runner maps to a harness kind, and so to a model.
	Kind   string
	Prompt string
}

// ClaudeCodeRunner runs one turn and returns what the model said.
type ClaudeCodeRunner func(ctx context.Context, turn ClaudeCodeTurn) (string, error)

var (
	ccMu     sync.RWMutex
	ccRunner ClaudeCodeRunner
	ccSeq    atomic.Int64

	// ccPrimed records which session keys have been given their system
	// prompt in this process. A warm key is told once, not on every turn.
	ccPrimedMu sync.Mutex
	ccPrimed   = map[string]bool{}
)

// SetClaudeCodeRunner installs the path claude-code sessions run on. Called
// once by the runtime; until it is, those sessions fail rather than guess.
func SetClaudeCodeRunner(fn ClaudeCodeRunner) {
	ccMu.Lock()
	ccRunner = fn
	ccMu.Unlock()
}

func claudeCodeRunner() ClaudeCodeRunner {
	ccMu.RLock()
	defer ccMu.RUnlock()
	return ccRunner
}

// IsClaudeCode reports whether a provider name selects Claude Code.
func IsClaudeCode(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderClaudeCode, "claude_code", "claudecode":
		return true
	}
	return false
}

// sessionKey is the Claude Code session this Session's turns go to.
func (s *Session) sessionKey() string {
	if k := strings.TrimSpace(s.cfg.SessionKey); k != "" {
		return "api/" + k
	}
	s.ccKeyOnce.Do(func() {
		kind := s.cfg.Kind
		if kind == "" {
			kind = "unlabelled"
		}
		s.ccKey = fmt.Sprintf("api/%s/%s/%d", kind, s.cfg.AgentID, ccSeq.Add(1))
	})
	return s.ccKey
}

// chatViaClaudeCode runs one turn on Claude Code.
func (s *Session) chatViaClaudeCode(ctx context.Context, userMessage string, turnTools []tools.Tool) (string, []ToolCallRecord, TokenInfo, error) {
	run := claudeCodeRunner()
	if run == nil {
		return "", nil, TokenInfo{}, errors.New("claude-code provider is not available in this process")
	}
	s.setActor(connectorkit.ActorFrom(ctx))
	userMessage = CleanContent(userMessage)
	s.history.Messages = append(s.history.Messages, models.AIMessage{Role: models.User, Message: userMessage})

	var token string
	var reg *turnRegistration
	if len(turnTools) > 0 {
		token, reg = registerTurnTools(turnTools, s.currentActor)
		defer unregisterTurnTools(token)
	}

	key := s.sessionKey()
	prompt := s.composeClaudeCodePrompt(key, userMessage, token, turnTools)

	text, err := run(ctx, ClaudeCodeTurn{Key: key, Kind: s.cfg.Kind, Prompt: prompt})
	if err != nil {
		return "", nil, TokenInfo{}, fmt.Errorf("claude-code: %w", err)
	}
	text = CleanContent(text)
	s.history.Messages = append(s.history.Messages, models.AIMessage{Role: models.Assistant, Message: text})

	var records []ToolCallRecord
	if reg != nil {
		records = reg.take()
	}
	// Token counts are the harness's own affair and are metered there. What
	// is reported here is that a turn happened, and how long it took, so the
	// meter still shows where the work went.
	info := TokenInfo{}
	reportUsage(s.cfg, ProviderClaudeCode, s.cfg.Kind, info)
	s.LastTokens = info
	return text, records, info, nil
}

// composeClaudeCodePrompt assembles the turn: the session's standing
// instructions the first time this key is used, then the per-turn context,
// then the tools, then the message — the order the API path gives the model.
func (s *Session) composeClaudeCodePrompt(key, userMessage, token string, turnTools []tools.Tool) string {
	var b strings.Builder

	ccPrimedMu.Lock()
	first := !ccPrimed[key]
	ccPrimed[key] = true
	ccPrimedMu.Unlock()
	if first && strings.TrimSpace(s.cfg.SystemPrompt) != "" {
		b.WriteString("<instructions>\n")
		b.WriteString(strings.TrimSpace(s.cfg.SystemPrompt))
		b.WriteString("\n</instructions>\n\n")
	}

	if ctxText := strings.TrimSpace(s.history.Context); ctxText != "" {
		b.WriteString("<context>\n")
		b.WriteString(ctxText)
		b.WriteString("\n</context>\n\n")
	}

	if token != "" {
		b.WriteString(describeTurnTools(token, turnTools))
		b.WriteString("\n")
	}
	b.WriteString(userMessage)
	return b.String()
}

// describeTurnTools tells the model how to reach this turn's tools.
func describeTurnTools(token string, turnTools []tools.Tool) string {
	var b strings.Builder
	b.WriteString("Tools for this turn. Call them from the shell, one per command:\n")
	b.WriteString("  karmax tool call --turn " + token + " <name> --json '<arguments as a JSON object>'\n")
	b.WriteString("Only these names work with this --turn value:\n")
	names := make([]string, 0, len(turnTools))
	byName := map[string]tools.ToolManifest{}
	for _, t := range turnTools {
		m := t.Manifest()
		names = append(names, m.Name)
		byName[m.Name] = m
	}
	sort.Strings(names)
	for _, n := range names {
		m := byName[n]
		b.WriteString("  - " + n + ": " + oneLine(m.Description, 240))
		if params := compactSchema(m.Parameters); params != "" {
			b.WriteString(" Arguments: " + params)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// compactSchema reduces a JSON schema to "name (type, required), …", which is
// all a model needs to write the arguments and a fraction of the tokens.
func compactSchema(raw json.RawMessage) string {
	var sch struct {
		Properties map[string]struct {
			Type        any    `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &sch) != nil || len(sch.Properties) == 0 {
		return ""
	}
	required := map[string]bool{}
	for _, r := range sch.Required {
		required[r] = true
	}
	names := make([]string, 0, len(sch.Properties))
	for n := range sch.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		p := sch.Properties[n]
		desc := fmt.Sprint(p.Type)
		if required[n] {
			desc += ", required"
		}
		parts = append(parts, n+" ("+desc+")")
	}
	return strings.Join(parts, ", ")
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ---- turn-scoped tools -----------------------------------------------------

type turnRegistration struct {
	tools   map[string]tools.Tool
	actor   func() string
	expires time.Time

	mu      sync.Mutex
	records []ToolCallRecord
}

func (r *turnRegistration) take() []ToolCallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.records
	r.records = nil
	return out
}

// turnTTL bounds how long a token can outlive its turn if the unregister is
// somehow missed. Longer than any turn a kind is allowed to run.
const turnTTL = 20 * time.Minute

var (
	turnMu   sync.Mutex
	turnRegs = map[string]*turnRegistration{}
)

func registerTurnTools(ts []tools.Tool, actor func() string) (string, *turnRegistration) {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	reg := &turnRegistration{tools: map[string]tools.Tool{}, actor: actor, expires: time.Now().Add(turnTTL)}
	// Keyed canonically, so a model that writes comms_send for comms.send
	// still reaches it — and still cannot reach anything it was not granted.
	for _, t := range ts {
		reg.tools[tools.CanonicalName(t.Manifest().Name)] = t
	}
	turnMu.Lock()
	now := time.Now()
	for k, r := range turnRegs {
		if now.After(r.expires) {
			delete(turnRegs, k)
		}
	}
	turnRegs[token] = reg
	turnMu.Unlock()
	return token, reg
}

func unregisterTurnTools(token string) {
	turnMu.Lock()
	delete(turnRegs, token)
	turnMu.Unlock()
}

// ErrUnknownTurn means the token belongs to no running turn.
var ErrUnknownTurn = errors.New("unknown or finished turn")

// ErrNotInTurn means the tool was not granted to that turn.
var ErrNotInTurn = errors.New("tool is not available on this turn")

// CallTurnTool runs a tool granted to a running Claude Code turn. The token is
// the grant: it names exactly the tools that turn was given, and nothing else
// is reachable through it.
func CallTurnTool(ctx context.Context, token, name string, input map[string]any) (tools.ToolResult, error) {
	turnMu.Lock()
	reg, ok := turnRegs[token]
	if ok && time.Now().After(reg.expires) {
		delete(turnRegs, token)
		ok = false
	}
	turnMu.Unlock()
	if !ok {
		return tools.ToolResult{}, ErrUnknownTurn
	}
	t, ok := reg.tools[tools.CanonicalName(name)]
	if !ok {
		return tools.ToolResult{}, ErrNotInTurn
	}
	if actor := reg.actor(); actor != "" && connectorkit.ActorFrom(ctx) == "" {
		ctx = connectorkit.WithActor(ctx, actor)
	}
	res, err := t.Execute(ctx, input)
	rec := ToolCallRecord{Name: name, Input: input, Result: res, Error: err}
	reg.mu.Lock()
	reg.records = append(reg.records, rec)
	reg.mu.Unlock()
	return res, err
}

// PrimeTurn starts a claude-code session's conversation ahead of its first
// real turn: the standing instructions, the current context and a note go in,
// the reply is thrown away, and nothing is added to the session's history.
//
// A Claude Code session is a process, and its first turn pays for starting
// one. On a phone call that cost lands on the caller's first question — a
// pause long enough to make them repeat it. Priming during the greeting moves
// it to a moment when nobody is waiting.
//
// The prompt is composed now and the turn returned to be run later, so the
// caller can run it in the background without it reading the session's
// context while a real turn is changing it. Nil for any other provider.
func (s *Session) PrimeTurn(note string) func(context.Context) error {
	if !IsClaudeCode(s.cfg.Provider) {
		return nil
	}
	key := s.sessionKey()
	prompt := s.composeClaudeCodePrompt(key, note, "", nil)
	kind := s.cfg.Kind
	return func(ctx context.Context) error {
		run := claudeCodeRunner()
		if run == nil {
			return errors.New("claude-code provider is not available in this process")
		}
		_, err := run(ctx, ClaudeCodeTurn{Key: key, Kind: kind, Prompt: prompt})
		return err
	}
}
