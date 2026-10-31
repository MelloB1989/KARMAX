package config

import "testing"

func TestApplyDefaultsAddsFallbacks(t *testing.T) {
	cfg := &KarmaxConfig{
		AI: AIConfig{
			DefaultProvider: "anthropic",
			DefaultModel:    "claude-opus-4.6",
		},
		Agents: []AgentDefConfig{{ID: "agent-main"}},
	}

	applyDefaults(cfg)

	fallbacks := cfg.Agents[0].FallbackModels
	if len(fallbacks) != 2 {
		t.Fatalf("expected two default fallback models, got %d", len(fallbacks))
	}
	if fallbacks[0].Provider != "anthropic" || fallbacks[0].Model != "claude-sonnet-4.6" {
		t.Fatalf("unexpected first fallback: %+v", fallbacks[0])
	}
	if fallbacks[1].Provider != "anthropic" || fallbacks[1].Model != "deepseek-3.2" {
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
		t.Fatalf("expected custom fallback plus default fallbacks, got %d", len(fallbacks))
	}
	if fallbacks[0].Provider != "openai" || fallbacks[0].Model != "gpt-4o-mini" {
		t.Fatalf("custom fallback was not preserved first: %+v", fallbacks[0])
	}
}

func TestNormalizeLoops(t *testing.T) {
	cfg := &KarmaxConfig{
		Agents: []AgentDefConfig{{ID: "nexus"}},
		Loops: []LoopConfig{
			{Name: "sync", Every: "30m", Prompt: "do it"},
			{Name: "news", Cron: "0 0 9 * * *", Prompt: "news", Agent: "nexus"},
		},
	}

	applyDefaults(cfg)

	if cfg.Loops[0].Cron != "@every 30m" {
		t.Fatalf("every not translated to cron: %q", cfg.Loops[0].Cron)
	}
	if cfg.Loops[0].Agent != "nexus" {
		t.Fatalf("default agent not applied: %q", cfg.Loops[0].Agent)
	}
	if cfg.Loops[0].Enabled == nil || !*cfg.Loops[0].Enabled {
		t.Fatal("enabled should default to true")
	}
	if err := validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateLoopRejectsMissingPrompt(t *testing.T) {
	cfg := &KarmaxConfig{
		Agents: []AgentDefConfig{{ID: "nexus"}},
		Loops:  []LoopConfig{{Name: "bad", Every: "1h"}},
	}
	applyDefaults(cfg)
	if err := validate(cfg); err == nil {
		t.Fatal("expected validation error for loop missing prompt")
	}
}

func TestChannelRoutedAgentIsStartedAndUnknownRouteRejected(t *testing.T) {
	cfg := &KarmaxConfig{
		Agents: []AgentDefConfig{{ID: "chat"}, {ID: "idle"}, {ID: "up", Triggers: AgentTriggersConfig{RunOnStart: true}}},
		Comms:  CommsConfig{Channels: []ChannelConfig{{ID: "c", Type: "http", AgentID: "chat"}, {ID: "d", Type: "http", AgentID: "up"}}},
	}
	if err := validate(cfg); err != nil {
		t.Fatal(err)
	}
	routeAgents(cfg)
	if !cfg.Agents[0].Triggers.RunOnStart || cfg.Agents[1].Triggers.RunOnStart {
		t.Fatalf("run_on_start = %v/%v, want routed true, idle false", cfg.Agents[0].Triggers.RunOnStart, cfg.Agents[1].Triggers.RunOnStart)
	}
	if len(cfg.AutoStarted) != 1 || cfg.AutoStarted[0] != "chat" {
		t.Fatalf("AutoStarted = %v", cfg.AutoStarted)
	}
	cfg.Comms.Channels[0].AgentID = "ghost"
	if err := validate(cfg); err == nil {
		t.Fatal("a channel routed to an unknown agent was accepted")
	}
}

func TestHarnessMCPServersBecomeOneMCPConfig(t *testing.T) {
	var h HarnessConfig
	if got, err := h.MCPConfigJSON(); got != "" || err != nil {
		t.Fatalf("unset: got %q, %v; want the CLI's own servers", got, err)
	}
	h.MCPServers = map[string]map[string]any{
		"fleet": {"command": "fleetctl", "args": []any{"mcp"}},
	}
	got, err := h.MCPConfigJSON()
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"mcpServers":{"fleet":{"args":["mcp"],"command":"fleetctl"}}}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
