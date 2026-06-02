package karmahelper

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MelloB1989/karma/ai"
	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
)

type SessionConfig struct {
	Provider    string
	Model       string
	SystemPrompt string
	Temperature float32
	MaxTokens   int
}

type Session struct {
	cfg     SessionConfig
	tools   []tools.Tool
	history models.AIChatHistory
	kai     *ai.KarmaAI
}

type ToolCallRecord struct {
	ID     string
	Name   string
	Input  map[string]any
	Result tools.ToolResult
	Error  error
}

func NewSession(cfg SessionConfig, agentTools []tools.Tool) *Session {
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

	kai := ai.NewKarmaAI(model, provider, options...)

	return &Session{
		cfg:   cfg,
		tools: agentTools,
		kai:   kai,
		history: models.AIChatHistory{
			Messages: []models.AIMessage{},
		},
	}
}

func (s *Session) Chat(ctx context.Context, userMessage string) (string, []ToolCallRecord, error) {
	s.history.Messages = append(s.history.Messages, models.AIMessage{
		Role:    models.User,
		Message: userMessage,
	})

	resp, err := s.kai.ChatCompletionManaged(&s.history)
	if err != nil {
		return "", nil, fmt.Errorf("karma chat: %w", err)
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

	return resp.AIResponse, records, nil
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
	case "grok-4":
		return ai.Grok4
	case "grok-3":
		return ai.Grok3
	case "llama-3.3-70b":
		return ai.Llama33_70B
	case "claude-opus-4-6-thinking":
		return ai.BaseModel("claude-opus-4-6-thinking")
	default:
		return ai.GPT4o
	}
}
