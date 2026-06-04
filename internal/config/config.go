package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*KarmaxConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	expanded := expandEnvVars(string(data))

	var cfg KarmaxConfig
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

func expandEnvVars(s string) string {
	return os.Expand(s, func(key string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return ""
	})
}

func applyDefaults(cfg *KarmaxConfig) {
	if cfg.Karmax.Version == "" {
		cfg.Karmax.Version = "1"
	}
	if cfg.Karmax.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.Karmax.DataDir = home + "/.karmax"
	}
	if strings.HasPrefix(cfg.Karmax.DataDir, "~/") {
		home, _ := os.UserHomeDir()
		cfg.Karmax.DataDir = home + cfg.Karmax.DataDir[1:]
	}
	if cfg.Karmax.LogLevel == "" {
		cfg.Karmax.LogLevel = "info"
	}
	if cfg.Karmax.LogFormat == "" {
		cfg.Karmax.LogFormat = "pretty"
	}
	if cfg.Dashboard.Port == 0 {
		cfg.Dashboard.Port = 8080
	}
	if cfg.Dashboard.Host == "" {
		cfg.Dashboard.Host = "127.0.0.1"
	}
	if cfg.Webhooks.Port == 0 {
		cfg.Webhooks.Port = 9090
	}
	if cfg.Webhooks.Host == "" {
		cfg.Webhooks.Host = "0.0.0.0"
	}

	for i := range cfg.Agents {
		if cfg.Agents[i].Provider == "" {
			cfg.Agents[i].Provider = cfg.AI.DefaultProvider
		}
		if cfg.Agents[i].Model == "" {
			cfg.Agents[i].Model = cfg.AI.DefaultModel
		}
		if cfg.Agents[i].Temperature == 0 {
			cfg.Agents[i].Temperature = 0.7
		}
		if cfg.Agents[i].MaxTokens == 0 {
			cfg.Agents[i].MaxTokens = 4096
		}
		if cfg.Agents[i].RestartPolicy == "" {
			cfg.Agents[i].RestartPolicy = "on-failure"
		}
		if cfg.Agents[i].Memory.Namespace == "" {
			cfg.Agents[i].Memory.Namespace = cfg.Agents[i].ID
		}
		if cfg.Agents[i].HealthCheck.IntervalSeconds == 0 {
			cfg.Agents[i].HealthCheck.IntervalSeconds = 30
		}
		cfg.Agents[i].FallbackModels = ensureDefaultGeminiFallbacks(cfg.Agents[i].FallbackModels)
	}
}

func ensureDefaultGeminiFallbacks(fallbacks []FallbackModelConfig) []FallbackModelConfig {
	defaults := []FallbackModelConfig{
		{Provider: "google", Model: "gemini-3.1-pro-high"},
		{Provider: "google", Model: "gemini-3.1-flash-lite"},
	}
	if len(fallbacks) == 0 {
		return defaults
	}

	out := append([]FallbackModelConfig(nil), fallbacks...)
	for _, def := range defaults {
		exists := false
		for _, fb := range out {
			if strings.EqualFold(fb.Provider, def.Provider) && strings.EqualFold(fb.Model, def.Model) {
				exists = true
				break
			}
		}
		if !exists {
			out = append(out, def)
		}
	}
	return out
}

func validate(cfg *KarmaxConfig) error {
	seen := make(map[string]bool)
	for _, a := range cfg.Agents {
		if a.ID == "" {
			return fmt.Errorf("agent missing id")
		}
		if seen[a.ID] {
			return fmt.Errorf("duplicate agent id: %s", a.ID)
		}
		seen[a.ID] = true
	}
	return nil
}

func SaveDefault(path string) error {
	cfg := defaultConfig()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func defaultConfig() KarmaxConfig {
	return KarmaxConfig{
		Karmax: KarmaxCoreConfig{
			Version:   "1",
			DataDir:   "~/.karmax",
			LogLevel:  "info",
			LogFormat: "pretty",
		},
		Dashboard: DashboardConfig{
			Enabled: true,
			Port:    8080,
			Host:    "127.0.0.1",
		},
		Webhooks: WebhooksConfig{
			Enabled: true,
			Port:    9090,
			Host:    "0.0.0.0",
		},
		AI: AIConfig{
			DefaultProvider: "openai",
			DefaultModel:    "gpt-4o",
			Providers: map[string]ProviderConfig{
				"openai": {APIKey: "${OPENAI_API_KEY}"},
				"google": {APIKey: "${GOOGLE_API_KEY}"},
			},
		},
		Agents: []AgentDefConfig{
			{
				ID:           "hello-world",
				Name:         "Hello World Agent",
				Description:  "A sample agent that responds to greetings",
				SystemPrompt: "You are a friendly assistant. Respond helpfully and concisely.",
				Model:        "gpt-4o",
				Provider:     "openai",
				Temperature:  0.7,
				MaxTokens:    1024,
				Memory: AgentMemoryConfig{
					Enabled:    true,
					Namespace:  "hello-world",
					MaxEntries: 50,
				},
				RestartPolicy:  "on-failure",
				FallbackModels: ensureDefaultGeminiFallbacks(nil),
				Triggers: AgentTriggersConfig{
					RunOnStart: true,
				},
			},
		},
	}
}
