package karmahelper

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karma/ai"
	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
)

// FallbackModel defines an alternative provider+model to try when the primary fails.
type FallbackModel struct {
	Provider string
	Model    string
}

// TokenInfo holds token usage from a single chat completion call.
type TokenInfo struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	// CacheReadTokens is the part of the prompt served from cache, billed at
	// roughly a tenth of fresh input; CacheWriteTokens is what was written this
	// call. Both zero on providers without prompt caching.
	CacheReadTokens  int
	CacheWriteTokens int
}

type SessionConfig struct {
	Provider       string
	Model          string
	SystemPrompt   string
	Temperature    float32
	MaxTokens      int
	FallbackModels []FallbackModel
	// MaxToolPasses bounds tool round-trips per turn. Zero keeps the default
	// (8). A voice turn wants 2: at roughly a second per model pass, an
	// eight-pass deliberation is a hung-up phone.
	MaxToolPasses int
	// MaxRetries bounds same-model retries before falling back. Zero keeps the
	// default (3). Retries carry one- and two-second backoffs, so on a latency
	// budget a retry IS the failure — voice wants 1, reaching the fallback
	// model while a patient caller would still be waiting out the first backoff.
	MaxRetries int
	// Kind and AgentID label this session's spend. Every session reports its
	// usage through the package meter (see OnUsage); these say which part of
	// the system to bill it to. A session that sets neither still reports —
	// as "unlabelled", which is a bug worth seeing rather than spend worth
	// losing.
	Kind    string
	AgentID string
}

// Usage is one model call's cost, as reported to the meter.
type Usage struct {
	Kind         string
	AgentID      string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
}

// usageMeter receives every model call made through this package.
//
// Accounting lives HERE, not at the call sites, because it was at the call
// sites: three of twelve sessions recorded their spend and the other nine did
// not, so the tracker reported a fifth of a bill that was five times bigger.
// A session that forgets to report is the default failure mode of per-site
// accounting; there is no way to forget from inside the constructor.
var usageMeter atomic.Pointer[func(Usage)]

// OnUsage installs the meter. Called once at startup.
func OnUsage(fn func(Usage)) {
	if fn == nil {
		usageMeter.Store(nil)
		return
	}
	usageMeter.Store(&fn)
}

func reportUsage(cfg SessionConfig, provider, model string, t TokenInfo) {
	fn := usageMeter.Load()
	if fn == nil || (t.InputTokens == 0 && t.OutputTokens == 0) {
		return
	}
	kind := cfg.Kind
	if kind == "" {
		kind = "unlabelled"
	}
	(*fn)(Usage{
		Kind: kind, AgentID: cfg.AgentID, Provider: provider, Model: model,
		InputTokens: t.InputTokens, OutputTokens: t.OutputTokens,
		CacheRead: t.CacheReadTokens, CacheWrite: t.CacheWriteTokens,
	})
}

type Session struct {
	cfg        SessionConfig
	tools      []tools.Tool
	history    models.AIChatHistory
	kai        *ai.KarmaAI
	LastTokens TokenInfo
	rec        *callRecorder
}

// callRecorder captures what the model actually invoked during one turn.
//
// karma executes tools itself (UseMCPExecution defaults on) and its terminal
// response carries no ToolCalls, so the only place that knows a tool ran is the
// wrapper around the tool's own Execute. Without this the caller sees zero tool
// calls on every turn — which made the act-evidence guard re-prompt "you called
// NO tool" at agents that had just called one, and the model do the work twice.
type callRecorder struct {
	mu    sync.Mutex
	calls []ToolCallRecord
}

func (c *callRecorder) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

func (c *callRecorder) add(r ToolCallRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r)
}

// take returns this turn's calls and clears them.
func (c *callRecorder) take() []ToolCallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

type ToolCallRecord struct {
	ID     string
	Name   string
	Input  map[string]any
	Result tools.ToolResult
	Error  error
}

func NewSession(cfg SessionConfig, agentTools []tools.Tool) *Session {
	rec := &callRecorder{}
	return &Session{
		cfg:   cfg,
		tools: agentTools,
		kai:   buildKarmaAI(cfg, agentTools, rec),
		rec:   rec,
		history: models.AIChatHistory{
			Messages: []models.AIMessage{},
		},
	}
}

// GetLastTokens returns the token usage from the most recent chat call.
func (s *Session) GetLastTokens() TokenInfo {
	return s.LastTokens
}

// GetHistory returns a pointer to the current in-memory chat history.
func (s *Session) GetHistory() models.AIChatHistory {
	return s.history
}

// SetHistory replaces the session history after loading persisted state or
// compacting long conversations.
func (s *Session) SetHistory(history models.AIChatHistory) {
	sanitizeHistory(&history)
	s.history = history
}

// SetContext sets the Context field on the session's history so the LLM
// receives dynamic context alongside the system prompt.
func (s *Session) SetContext(ctx string) {
	s.history.Context = CleanContent(ctx)
}

// Chat sends a user message through the model and returns the response,
// tool call records, token info, and any error. It sanitizes history to
// remove stale tool call metadata before calling the API, retries on
// transient errors with exponential backoff, and falls back to alternative
// models if the primary is exhausted.
func (s *Session) Chat(ctx context.Context, userMessage string) (string, []ToolCallRecord, TokenInfo, error) {
	return s.ChatWithExtraTools(ctx, userMessage, nil)
}

// ChatWithExtraTools is Chat with tools available for this turn only.
//
// Used to lend a WASM workflow the agent's attention: the tools a workflow
// provides belong on the turns that workflow caused, and nowhere else. A
// hundred installed workflows must not put a hundred tools in every prompt.
//
// The model is rebuilt for the turn rather than the session being mutated,
// which is what makes "this turn only" true even if the call fails partway.
// buildKarmaAI is pure construction, so it is cheap enough to do per turn.
func (s *Session) ChatWithExtraTools(ctx context.Context, userMessage string, extra []tools.Tool) (string, []ToolCallRecord, TokenInfo, error) {
	return s.ChatWithTurnTools(ctx, userMessage, extra, nil)
}

// ChatWithTurnTools is one turn with tools added and tools withheld.
//
// Withholding is what a prompt cannot do. The hot-sync pass is told, at length
// and in capitals, that it does not speak — "do not call comms.send, do not
// call wacli send, and do not delegate a send" — and it answered a question in
// the operator's chat anyway, an hour and a half after the question had already
// been answered. A capability the model holds is a capability it will
// eventually use, whatever the instructions around it say. A pass that must not
// speak is handed no way to speak.
func (s *Session) ChatWithTurnTools(ctx context.Context, userMessage string, extra []tools.Tool, withhold map[string]bool) (string, []ToolCallRecord, TokenInfo, error) {
	if len(extra) == 0 && len(withhold) == 0 {
		return s.chat(ctx, userMessage, s.kai, s.tools)
	}
	turnTools := turnToolSet(s.tools, extra, withhold)
	return s.chat(ctx, userMessage, buildKarmaAI(s.cfg, turnTools, s.rec), turnTools)
}

func (s *Session) chat(ctx context.Context, userMessage string, kai *ai.KarmaAI, turnTools []tools.Tool) (string, []ToolCallRecord, TokenInfo, error) {
	userMessage = CleanContent(userMessage)
	s.history.Messages = append(s.history.Messages, models.AIMessage{
		Role:    models.User,
		Message: userMessage,
	})

	// --- Sanitize history: strip stale tool call metadata ---
	sanitizeHistory(&s.history)

	// Cleared per turn so a retry or a fallback model cannot inherit the calls
	// of the attempt before it.
	if s.rec != nil {
		s.rec.reset()
	}

	// --- Try primary model with retries ---
	retries := s.cfg.MaxRetries
	if retries <= 0 {
		retries = 3
	}
	resp, err := chatWithRetry(ctx, kai, &s.history, retries)
	if err == nil {
		// A gateway that pre-prompts the model with its OWN identity sometimes
		// answers as that identity instead of as this agent. It arrives as a
		// normal 200 with a well-formed body, so nothing on the error path sees
		// it — caught here or it reaches the operator verbatim.
		if isPersonaBreak(resp.AIResponse) && len(resp.ToolCalls) == 0 {
			log.Printf("[karmahelper] the model answered as something other than this agent; retrying")
			retry, rerr := chatWithRetry(ctx, kai, &s.history, 1)
			switch {
			case rerr == nil && !isPersonaBreak(retry.AIResponse):
				return s.processResponse(retry)
			case transportFallback() != nil && len(turnTools) == 0:
				log.Printf("[karmahelper] still out of character; trying the out-of-band path")
				if out, ferr := transportFallback()(ctx, userMessage); ferr == nil {
					return CleanContent(out), nil, TokenInfo{}, nil
				}
			}
			// Returned as a failure rather than passed through: the operator
			// reading "I'm <the gateway>" is the bug being fixed here.
			return "", nil, TokenInfo{}, fmt.Errorf("model answered out of character and no path recovered it")
		}
		return s.processResponse(resp)
	}

	primaryErr := err
	log.Printf("[karmahelper] primary model %s/%s failed after retries: %v", s.cfg.Provider, s.cfg.Model, err)

	// --- Try fallback models ---
	for i, fb := range s.cfg.FallbackModels {
		log.Printf("[karmahelper] trying fallback model %d: %s/%s", i+1, fb.Provider, fb.Model)

		fbCfg := s.cfg
		fbCfg.Provider = fb.Provider
		fbCfg.Model = fb.Model

		// Built with the TURN's tools, not the session's: a fallback that
		// silently dropped the lent ones would answer without the tools the
		// question needed and look like the model simply chose not to use them.
		fbKai := buildKarmaAI(fbCfg, turnTools, s.rec)

		// Re-sanitize before each fallback attempt
		sanitizeHistory(&s.history)

		resp, err = chatWithRetry(ctx, fbKai, &s.history, 2)
		if err == nil {
			log.Printf("[karmahelper] fallback model %s/%s succeeded", fb.Provider, fb.Model)
			return s.processResponse(resp)
		}

		log.Printf("[karmahelper] fallback model %s/%s failed: %v", fb.Provider, fb.Model, err)
	}

	// Last resort: a path that does not share the primary's transport.
	//
	// The fallback models above are NOT redundancy in the deployment this runs
	// in — every one of them is reached through the same ANTHROPIC_BASE_URL, so
	// when that one process dies they all fail together. 141 loop failures over
	// three days were a single dead gateway wearing a fallback chain's costume.
	//
	// Installed by the runtime rather than built in, because this package must
	// not know about the harness, the store, or the CLI it shells out to.
	// Tools are the one thing that cannot cross over, so a session that lent
	// tools fails rather than quietly answering without them.
	if fn := transportFallback(); fn != nil && len(turnTools) == 0 && isTransportFailure(primaryErr) {
		log.Printf("[karmahelper] every model shares a dead transport; trying the out-of-band fallback")
		if out, ferr := fn(ctx, userMessage); ferr == nil {
			return CleanContent(out), nil, TokenInfo{}, nil
		} else {
			log.Printf("[karmahelper] out-of-band fallback also failed: %v", ferr)
		}
	}

	return "", nil, TokenInfo{}, fmt.Errorf("all models failed, primary error: %w", primaryErr)
}

// TransportFallback answers a prompt over a path independent of the configured
// providers. Nil means there is none.
type TransportFallback func(ctx context.Context, prompt string) (string, error)

var (
	fallbackMu sync.RWMutex
	fallbackFn TransportFallback
)

// SetTransportFallback installs the last-resort model path for every session in
// the process. Called once by the runtime at startup.
func SetTransportFallback(fn TransportFallback) {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	fallbackFn = fn
}

func transportFallback() TransportFallback {
	fallbackMu.RLock()
	defer fallbackMu.RUnlock()
	return fallbackFn
}

// isTransportFailure reports whether the model could not be REACHED, as opposed
// to having been reached and having declined. Only the former is worth trying
// on another path: a refusal or a malformed request fails identically wherever
// it is sent.
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"connection refused", "no such host", "connection reset",
		"i/o timeout", "dial tcp", "context deadline exceeded",
		"502", "503", "504", "eof",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// isPersonaBreak reports whether a reply is the model introducing itself as
// something other than this agent — the signature of an inference gateway whose
// own system prompt won over the one this process sent.
//
// Matched only on FIRST-PERSON identity claims. The operator works on the
// gateway itself, so a reply that merely mentions it by name is ordinary
// conversation and must not trip this.
func isPersonaBreak(response string) bool {
	s := strings.ToLower(response)
	for _, sig := range []string{
		"i'm kiro", "i am kiro", "as kiro, i",
		"i'm not karmax", "i am not karmax",
		"i'm an ai development environment", "i am an ai development environment",
		"i'm an ai-powered development environment", "i am an ai-powered development environment",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// processResponse extracts the AI response text, tool call records, and token
// info from a successful AIChatResponse.
func (s *Session) processResponse(resp *models.AIChatResponse) (string, []ToolCallRecord, TokenInfo, error) {
	tokens := TokenInfo{
		InputTokens:      resp.InputTokens,
		OutputTokens:     resp.OutputTokens,
		TotalTokens:      resp.Tokens,
		CacheReadTokens:  resp.CacheReadTokens,
		CacheWriteTokens: resp.CacheWriteTokens,
	}
	s.LastTokens = tokens
	// Every model call, whichever session made it and whether or not the
	// response is usable — those tokens were bought either way.
	reportUsage(s.cfg, s.cfg.Provider, s.cfg.Model, tokens)
	response := CleanContent(resp.AIResponse)

	// Collected before the empty-response check: a turn that ran tools and then
	// returned no text still ran them, and the caller needs to know that.
	var executed []ToolCallRecord
	if s.rec != nil {
		executed = s.rec.take()
	}

	if strings.TrimSpace(response) == "" {
		return "", executed, tokens, fmt.Errorf("empty response from model after sanitization")
	}

	// What the wrapper saw beats what the provider reported. Providers that run
	// tools server-side return none at all; the ones that hand them back
	// unexecuted are covered by the fallback below.
	if len(executed) > 0 {
		return response, executed, tokens, nil
	}

	var records []ToolCallRecord
	for _, tc := range resp.ToolCalls {
		var input map[string]any
		json.Unmarshal([]byte(tc.Function.Arguments), &input)
		records = append(records, ToolCallRecord{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	s.history.Messages = append(s.history.Messages, models.AIMessage{
		Role:    models.Assistant,
		Message: response,
	})
	sanitizeHistory(&s.history)

	return response, records, tokens, nil
}

// sanitizeHistory removes stale tool call metadata from history messages.
// This prevents "input item ID does not belong to this connection" errors
// from Anthropic and similar providers when persisted history contains
// tool call IDs from a previous API connection.
// Tool-role messages are converted to assistant messages with a prefix to
// preserve conversation context while removing stale API-specific IDs.
func sanitizeHistory(history *models.AIChatHistory) {
	cleaned := make([]models.AIMessage, 0, len(history.Messages))
	history.SystemMsg = CleanContent(history.SystemMsg)
	history.Context = CleanContent(history.Context)
	for _, msg := range history.Messages {
		msg.Message = CleanContent(msg.Message)
		// Convert tool-role messages to assistant messages to preserve context
		if msg.Role == models.Tool {
			cleaned = append(cleaned, models.AIMessage{
				Role:    models.Assistant,
				Message: CleanContent("[Tool Result] " + msg.Message),
			})
			continue
		}
		// Clear tool call fields on assistant messages
		if msg.Role == models.Assistant {
			msg.ToolCalls = nil
		}
		// Clear any stale ToolCallId
		msg.ToolCallId = ""
		cleaned = append(cleaned, msg)
	}
	history.Messages = cleaned
}

// CleanContent strips model-internal reasoning blocks and trims whitespace
// before text is sent to an API, saved, or sent back through a channel.
func CleanContent(s string) string {
	for {
		lower := strings.ToLower(s)
		start := strings.Index(lower, "<think>")
		if start == -1 {
			break
		}
		end := strings.Index(lower[start:], "</think>")
		if end == -1 {
			s = s[:start]
			break
		}
		end += start
		s = s[:start] + s[end+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

// isRetryableError determines whether the error is transient and worth retrying.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Codex transient failures (429/5xx, codeless response.failed) — classified
	// by karma so we don't rely on string matching.
	if ai.IsCodexRetryable(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	retryablePatterns := []string{
		"429",
		"500",
		"502",
		"503",
		"504",
		"connection",
		"timeout",
		"input item id",
		"does not belong",
		"rate limit",
		"overloaded",
		"server error",
		"eof",
		"broken pipe",
		"quota",
		"capacity",
		"too many requests",
		"resource exhausted",
	}
	for _, p := range retryablePatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isProxyErrorResponse detects when the proxy returns an error message
// as valid response content instead of an HTTP error code.
func isProxyErrorResponse(lower string) bool {
	errorPatterns := []string{
		"quota exceeded",
		"rate limit",
		"too many requests",
		"service unavailable",
		"internal server error",
		"capacity exceeded",
		"overloaded",
	}
	for _, p := range errorPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isStaleIDError checks for the specific Anthropic stale tool call ID error.
func isStaleIDError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "input item id") || strings.Contains(msg, "does not belong")
}

// chatWithRetry wraps ChatCompletionManaged with exponential backoff retry logic.
func chatWithRetry(ctx context.Context, kai *ai.KarmaAI, history *models.AIChatHistory, maxRetries int) (*models.AIChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s
			log.Printf("[karmahelper] retry %d/%d after %v (last error: %v)", attempt, maxRetries, backoff, lastErr)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("context cancelled during retry: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}

		resp, err := kai.ChatCompletionManaged(history)
		if err == nil {
			cleaned := strings.TrimSpace(CleanContent(resp.AIResponse))
			// Check for empty response
			if resp != nil && cleaned == "" {
				log.Printf("[karmahelper] WARNING: model returned empty response (input_tokens=%d, output_tokens=%d, history_len=%d)",
					resp.InputTokens, resp.OutputTokens, len(history.Messages))
				lastErr = fmt.Errorf("empty response from model (possible quota/rate issue)")
				continue
			}
			// Check for proxy error messages returned as content
			lower := strings.ToLower(cleaned)
			if isProxyErrorResponse(lower) {
				log.Printf("[karmahelper] WARNING: proxy returned error as content: %s", truncateLog(cleaned, 100))
				lastErr = fmt.Errorf("proxy error in response: %s", truncateLog(cleaned, 100))
				continue
			}
			return resp, nil
		}
		lastErr = err
		log.Printf("[karmahelper] attempt %d failed: %v", attempt, err)

		// On stale ID error, aggressively strip all tool-related messages
		if isStaleIDError(err) {
			log.Printf("[karmahelper] stale tool call ID error, stripping tool messages from history")
			sanitizeHistory(history)
		}

		if !isRetryableError(err) {
			return nil, fmt.Errorf("non-retryable error: %w", err)
		}
	}

	return nil, fmt.Errorf("exhausted %d retries: %w", maxRetries, lastErr)
}

// buildKarmaAI creates a new KarmaAI instance from a session config and tools.
func buildKarmaAI(cfg SessionConfig, agentTools []tools.Tool, rec *callRecorder) *ai.KarmaAI {
	provider := resolveProvider(cfg.Provider)
	model := resolveModel(cfg.Model)

	options := []ai.Option{
		ai.WithSystemMessage(cfg.SystemPrompt),
		ai.WithMaxTokens(reasoningTokenFloor(cfg.Model, cfg.MaxTokens)),
	}

	// Temperature or top_p, never both. Bedrock rejects a request carrying both
	// with "cannot both be specified for this model", and karma sets BOTH by
	// default (temperature 1, top_p 0.9) whether or not anyone asked — so every
	// call 400s the moment the transport is Bedrock rather than a permissive
	// proxy.
	//
	// top_p is suppressed unconditionally, not just when a temperature is
	// configured: a session that sets no temperature still inherits karma's
	// default of 1, so leaving top_p alone there sends both anyway. That is
	// exactly how the memory sub-agent kept failing after the main agent was
	// fixed — it never set a temperature, so the guard never fired.
	//
	// Reasoning models reject a non-default temperature outright rather than
	// ignoring it — confirmed against Azure's gpt-5 deployment: "'temperature'
	// does not support 0.7 with this model. Only the default (1) value is
	// supported." A config carrying a tuned temperature for Claude must not
	// 400 every call the moment the model is swapped.
	//
	// Which models those are is asked ONCE, here and in the token floor. The
	// two used to be written separately and had already drifted: this test
	// knew about gpt-5 and "thinking" but not o1 or o3, so an o-series model
	// would have been sent a temperature and 400d on every call, while the
	// floor beside it correctly treated the same model as reasoning.
	if !isReasoningModel(cfg.Model) {
		options = append(options, ai.WithTopP(0))
		if cfg.Temperature > 0 {
			options = append(options, ai.WithTemperature(cfg.Temperature))
		}
	}

	if len(agentTools) > 0 {
		options = append(options, ai.WithToolsEnabled())
		passes := cfg.MaxToolPasses
		if passes <= 0 {
			passes = 8
		}
		options = append(options, ai.WithMaxToolPasses(passes))

		for _, t := range agentTools {
			goTool := karmaxToolToGoFunctionTool(t, rec)
			options = append(options, ai.AddGoFunctionTool(goTool))
		}
	}

	return ai.NewKarmaAI(model, provider, options...)
}

func karmaxToolToGoFunctionTool(t tools.Tool, rec *callRecorder) ai.GoFunctionTool {
	manifest := t.Manifest()

	var params map[string]any
	json.Unmarshal(manifest.Parameters, &params)

	fp := ai.NewFuncParams()

	if props, ok := params["properties"].(map[string]any); ok {
		for name, prop := range props {
			if propMap, ok := prop.(map[string]any); ok {
				propType, _ := propMap["type"].(string)
				desc, _ := propMap["description"].(string)
				if desc == "" {
					desc = name
				}

				switch propType {
				case "string":
					fp.SetString(name, desc)
				case "integer", "number":
					fp.SetNumber(name, desc)
				case "boolean":
					fp.SetBool(name, desc)
				case "array":
					fp.SetArray(name, desc, "string")
				case "object":
					fp.SetString(name, desc+" (JSON object)")
				default:
					fp.SetString(name, desc)
				}
			}
		}
	}

	if required, ok := params["required"].([]any); ok {
		reqStrs := make([]string, 0, len(required))
		for _, r := range required {
			if s, ok := r.(string); ok {
				reqStrs = append(reqStrs, s)
			}
		}
		if len(reqStrs) > 0 {
			fp.SetRequired(reqStrs...)
		}
	}

	// Sanitize tool name: Anthropic requires ^[a-zA-Z0-9_-]{1,128}$
	sanitizedName := tools.CanonicalName(manifest.Name)

	return ai.NewGoFunctionTool(
		sanitizedName,
		manifest.Description,
		fp,
		func(ctx context.Context, args ai.FuncParams) (string, error) {
			input := make(map[string]any)
			for k, v := range args {
				if k != "__history" {
					input[k] = v
				}
			}

			result, err := t.Execute(ctx, input)
			// Recorded here because this is the only point that sees the call:
			// karma runs the tool itself and its final response says nothing
			// about what ran. Recorded on failure too — "it tried and the tool
			// refused" is a different turn from "it did nothing".
			if rec != nil {
				rec.add(ToolCallRecord{
					Name:   manifest.Name,
					Input:  input,
					Result: result,
					Error:  err,
				})
			}
			if err != nil {
				return "", err
			}
			if result.IsError {
				// A failing tool may still return the thing that makes the next
				// attempt work — the candidates behind "which Dev did you mean",
				// the field that was missing. Dropping Output on the error path
				// handed the model a dead end and kept the way out in a struct
				// nobody read.
				if result.Output != nil {
					if details, mErr := json.Marshal(result.Output); mErr == nil {
						return fmt.Sprintf("Error: %s\n%s", result.Error, capToolOutput(string(details))), nil
					}
				}
				return fmt.Sprintf("Error: %s", result.Error), nil
			}

			output, err := json.Marshal(result.Output)
			if err == nil {
				output = []byte(capToolOutput(string(output)))
			}
			if err != nil {
				return fmt.Sprintf("%v", result.Output), nil
			}
			return string(output), nil
		},
	)
}

func resolveProvider(name string) ai.Provider {
	switch name {
	case "openai":
		return ai.OpenAI
	case "azure-openai":
		// Registered at startup (internal/runtime/runtime.go) as a
		// CustomProvider when ai.providers.azure_openai is configured — this
		// name has to match ai.RegisterCustomProvider's Provider field
		// exactly, or it silently falls through to the default case below
		// and hits real OpenAI instead of the Azure deployment.
		return ai.Provider("azure-openai")
	case "anthropic":
		return ai.Anthropic
	case "codex":
		return ai.Codex
	case "groq":
		return ai.Groq
	case "google":
		return ai.Google
	case "xai":
		return ai.XAI
	case "fireworks":
		return ai.FireworksAI
	case "openrouter":
		return ai.OpenRouter
	case "bedrock":
		return ai.Bedrock
	default:
		return ai.OpenAI
	}
}

func resolveModel(name string) ai.BaseModel {
	switch name {
	case "gpt-4o":
		return ai.GPT4o
	case "gpt-4o-mini":
		return ai.GPT4oMini
	case "gpt-5":
		return ai.GPT5
	case "gpt-5-mini":
		return ai.GPT5Mini
	case "claude-4-sonnet", "claude-sonnet":
		return ai.Claude4Sonnet
	case "claude-4-opus", "claude-opus":
		return ai.Claude4Opus
	case "claude-3-5-sonnet":
		return ai.Claude35Sonnet
	case "claude-3-7-sonnet":
		return ai.Claude37Sonnet
	case "gemini-2.5-flash":
		return ai.Gemini25Flash
	case "gemini-2.5-pro":
		return ai.Gemini25Pro
	case "gemini-2.0-flash":
		return ai.Gemini20Flash
	case "gemini-3.1-flash-lite", "gemini-3-flash-preview":
		return ai.Gemini3FlashPreview
	case "gemini-3.1-pro-high", "gemini-3-pro-preview":
		return ai.Gemini3ProPreview
	case "grok-4":
		return ai.Grok4
	case "grok-3":
		return ai.Grok3
	case "llama-3.3-70b":
		return ai.Llama33_70B
	case "claude-opus-4-6-thinking":
		return ai.BaseModel("claude-opus-4-6-thinking")
	case "claude-opus-4.6", "claude-opus-4-6":
		return ai.BaseModel("claude-opus-4.6")
	case "claude-opus-4.7", "claude-opus-4-7":
		return ai.BaseModel("claude-opus-4.7")
	case "claude-sonnet-4.6", "claude-sonnet-4-6":
		return ai.BaseModel("claude-sonnet-4.6")
	case "deepseek-3.2", "deepseek-v3.2":
		return ai.BaseModel("deepseek-3.2")
	default:
		// Pass unknown model names as raw strings — allows custom proxy model IDs
		return ai.BaseModel(name)
	}
}

// maxToolOutputChars bounds what one tool result contributes to the
// conversation.
//
// A tool result is not paid for once. It joins the history and is re-sent with
// every following turn until compaction drops it — and compaction keeps the
// most recent fourteen messages, so a single large read is bought fifteen
// times. Measured on the live agent: turns carrying tool output pushed the
// per-call context from 13k to 158k tokens, and the tools.load round-trip then
// bought that context a second time within the same turn. Roughly 8k
// characters is a couple of thousand tokens: enough to answer from, small
// enough that carrying it is not the largest line in the bill.
const maxToolOutputChars = 8000

// capToolOutput trims an oversized tool result and says so, so the model reads
// a truncation rather than inferring the data simply ended.
func capToolOutput(s string) string {
	if len(s) <= maxToolOutputChars {
		return s
	}
	return s[:maxToolOutputChars] + fmt.Sprintf(
		"\n…[truncated: %d more characters. This result was too large to carry in the "+
			"conversation. Narrow the query, or ask for the specific field you need.]", len(s)-maxToolOutputChars)
}

// turnToolSet is the tools one turn may use: the session's own, plus anything
// lent, minus anything withheld. A lent tool is subject to the withhold too —
// otherwise a caller could hand back the very capability it just removed.
func turnToolSet(base, extra []tools.Tool, withhold map[string]bool) []tools.Tool {
	out := make([]tools.Tool, 0, len(base)+len(extra))
	for _, t := range append(append([]tools.Tool{}, base...), extra...) {
		if withhold[tools.CanonicalName(t.Manifest().Name)] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// reasoningTokenFloor raises a max-tokens cap that a reasoning model would
// spend entirely on thinking.
//
// On the gpt-5 family the reasoning counts against the completion budget, so a
// cap sized for the visible answer can be exhausted before a single character
// is emitted. The result is not an error but an EMPTY completion, which the
// retry logic reads as "possible quota/rate issue" and retries into the same
// wall: the memory-review loop failed 204 times in a row this way, on a judge
// sized at 400 tokens for a one-line verdict. Every session that kept working
// happened to be sized at 1200 or more.
//
// A cap is a ceiling, not a spend — raising it costs nothing on calls that stay
// small, and is the difference between an answer and silence on the ones that
// do not.
func reasoningTokenFloor(model string, want int) int {
	const floor = 2000
	if want >= floor || !isReasoningModel(model) {
		return want
	}
	return floor
}

// isReasoningModel reports models that bill their own thinking to the
// completion budget.
func isReasoningModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "gpt-5") || strings.Contains(m, "thinking") ||
		strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3")
}
