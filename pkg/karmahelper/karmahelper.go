package karmahelper

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
}

type SessionConfig struct {
	Provider       string
	Model          string
	SystemPrompt   string
	Temperature    float32
	MaxTokens      int
	FallbackModels []FallbackModel
}

type Session struct {
	cfg        SessionConfig
	tools      []tools.Tool
	history    models.AIChatHistory
	kai        *ai.KarmaAI
	LastTokens TokenInfo
}

type ToolCallRecord struct {
	ID     string
	Name   string
	Input  map[string]any
	Result tools.ToolResult
	Error  error
}

func NewSession(cfg SessionConfig, agentTools []tools.Tool) *Session {
	kai := buildKarmaAI(cfg, agentTools)

	return &Session{
		cfg:   cfg,
		tools: agentTools,
		kai:   kai,
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

// SetContext sets the Context field on the session's history so the LLM
// receives dynamic context alongside the system prompt.
func (s *Session) SetContext(ctx string) {
	s.history.Context = ctx
}

// Chat sends a user message through the model and returns the response,
// tool call records, token info, and any error. It sanitizes history to
// remove stale tool call metadata before calling the API, retries on
// transient errors with exponential backoff, and falls back to alternative
// models if the primary is exhausted.
func (s *Session) Chat(ctx context.Context, userMessage string) (string, []ToolCallRecord, TokenInfo, error) {
	s.history.Messages = append(s.history.Messages, models.AIMessage{
		Role:    models.User,
		Message: userMessage,
	})

	// --- Sanitize history: strip stale tool call metadata ---
	sanitizeHistory(&s.history)

	// --- Try primary model with retries ---
	resp, err := chatWithRetry(ctx, s.kai, &s.history, 3)
	if err == nil {
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

		fbKai := buildKarmaAI(fbCfg, s.tools)

		// Re-sanitize before each fallback attempt
		sanitizeHistory(&s.history)

		resp, err = chatWithRetry(ctx, fbKai, &s.history, 2)
		if err == nil {
			log.Printf("[karmahelper] fallback model %s/%s succeeded", fb.Provider, fb.Model)
			return s.processResponse(resp)
		}

		log.Printf("[karmahelper] fallback model %s/%s failed: %v", fb.Provider, fb.Model, err)
	}

	return "", nil, TokenInfo{}, fmt.Errorf("all models failed, primary error: %w", primaryErr)
}

// processResponse extracts the AI response text, tool call records, and token
// info from a successful AIChatResponse.
func (s *Session) processResponse(resp *models.AIChatResponse) (string, []ToolCallRecord, TokenInfo, error) {
	tokens := TokenInfo{
		InputTokens:  resp.InputTokens,
		OutputTokens: resp.OutputTokens,
		TotalTokens:  resp.Tokens,
	}
	s.LastTokens = tokens

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

	return resp.AIResponse, records, tokens, nil
}

// sanitizeHistory removes stale tool call metadata from history messages.
// This prevents "input item ID does not belong to this connection" errors
// from Anthropic and similar providers when persisted history contains
// tool call IDs from a previous API connection.
// Tool-role messages are converted to assistant messages with a prefix to
// preserve conversation context while removing stale API-specific IDs.
func sanitizeHistory(history *models.AIChatHistory) {
	cleaned := make([]models.AIMessage, 0, len(history.Messages))
	for _, msg := range history.Messages {
		// Convert tool-role messages to assistant messages to preserve context
		if msg.Role == models.Tool {
			cleaned = append(cleaned, models.AIMessage{
				Role:    models.Assistant,
				Message: "[Tool Result] " + msg.Message,
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

// isRetryableError determines whether the error is transient and worth retrying.
func isRetryableError(err error) bool {
	if err == nil {
		return false
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
	}
	for _, p := range retryablePatterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
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
			// Check for empty response
			if resp != nil && strings.TrimSpace(resp.AIResponse) == "" {
				log.Printf("[karmahelper] WARNING: model returned empty response (input_tokens=%d, output_tokens=%d, history_len=%d)",
					resp.InputTokens, resp.OutputTokens, len(history.Messages))
				// Return a fallback response instead of empty
				resp.AIResponse = "I processed your message but was unable to generate a meaningful response. Please try rephrasing or providing more context."
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
func buildKarmaAI(cfg SessionConfig, agentTools []tools.Tool) *ai.KarmaAI {
	provider := resolveProvider(cfg.Provider)
	model := resolveModel(cfg.Model)

	options := []ai.Option{
		ai.WithSystemMessage(cfg.SystemPrompt),
		ai.WithMaxTokens(cfg.MaxTokens),
	}

	if cfg.Temperature > 0 {
		options = append(options, ai.WithTemperature(cfg.Temperature))
	}

	if len(agentTools) > 0 {
		options = append(options, ai.WithToolsEnabled())
		options = append(options, ai.WithDirectToolCalls())
		options = append(options, ai.WithMaxToolPasses(8))

		for _, t := range agentTools {
			goTool := karmaxToolToGoFunctionTool(t)
			options = append(options, ai.AddGoFunctionTool(goTool))
		}
	}

	return ai.NewKarmaAI(model, provider, options...)
}

func karmaxToolToGoFunctionTool(t tools.Tool) ai.GoFunctionTool {
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

	return ai.NewGoFunctionTool(
		manifest.Name,
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
			if err != nil {
				return "", err
			}
			if result.IsError {
				return fmt.Sprintf("Error: %s", result.Error), nil
			}

			output, err := json.Marshal(result.Output)
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
	case "anthropic":
		return ai.Anthropic
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
		// No dedicated thinking constant in karma; use Claude4Opus as base
		// and the provider mapping will resolve to the correct API model ID.
		// The proxy at ANTHROPIC_BASE_URL handles thinking model routing.
		return ai.Claude4Opus
	default:
		return ai.GPT4o
	}
}
