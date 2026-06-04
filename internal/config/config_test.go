package config

import "testing"

func TestApplyDefaultsAddsGeminiFallbacks(t *testing.T) {
	cfg := &KarmaxConfig{
		AI: AIConfig{
			DefaultProvider: "anthropic",
			DefaultModel:    "claude-4-sonnet",
		},
		Agents: []AgentDefConfig{{ID: "agent-main"}},
	}

	applyDefaults(cfg)

	fallbacks := cfg.Agents[0].FallbackModels
	if len(fallbacks) != 2 {
		t.Fatalf("expected two default fallback models, got %d", len(fallbacks))
	}
	if fallbacks[0].Provider != "google" || fallbacks[0].Model != "gemini-3.1-pro-high" {
		t.Fatalf("unexpected first fallback: %+v", fallbacks[0])
	}
	if fallbacks[1].Provider != "google" || fallbacks[1].Model != "gemini-3.1-flash-lite" {
		t.Fatalf("unexpected second fallback: %+v", fallbacks[1])
	}
}

func TestApplyDefaultsPreservesAndAppendsFallbacks(t *testing.T) {
	cfg := &KarmaxConfig{
		Agents: []AgentDefConfig{{
			ID: "agent-main",
			FallbackModels: []FallbackModelConfig{{
				Provider: "openai",
				Model:    "gpt-4o-mini",
			}},
		}},
	}

	applyDefaults(cfg)

	fallbacks := cfg.Agents[0].FallbackModels
	if len(fallbacks) != 3 {
		t.Fatalf("expected custom fallback plus Gemini defaults, got %d", len(fallbacks))
	}
	if fallbacks[0].Provider != "openai" || fallbacks[0].Model != "gpt-4o-mini" {
		t.Fatalf("custom fallback was not preserved first: %+v", fallbacks[0])
	}
}
