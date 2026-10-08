package karmahelper

import "testing"

func TestBedrockKeyAppliedToBedrockOnly(t *testing.T) {
	cfg := SessionConfig{Provider: "bedrock", Model: "qwen.qwen3-next-80b-a3b", BedrockAPIKey: "k-test", BedrockRegion: "us-east-1"}
	kai := buildKarmaAI(cfg, nil, nil, nil)
	if kai.BedrockAPIKey != "k-test" || kai.BedrockRegion != "us-east-1" {
		t.Fatalf("bedrock session lost its key/region: %q %q", kai.BedrockAPIKey, kai.BedrockRegion)
	}
	cfg.Provider = "openai"
	if kai := buildKarmaAI(cfg, nil, nil, nil); kai.BedrockAPIKey != "" {
		t.Fatal("the bearer key leaked onto a non-bedrock provider")
	}
	// A fallback built from the same config carries the key too.
	fb := SessionConfig{Provider: "bedrock", Model: "mistral.ministral-3-14b-instruct", BedrockAPIKey: "k-test"}
	if kai := buildKarmaAI(fb, nil, nil, nil); kai.BedrockAPIKey != "k-test" {
		t.Fatal("fallback model lost the key")
	}
}

func TestSessionUsageHookSeesModel(t *testing.T) {
	var got Usage
	cfg := SessionConfig{OnUsage: func(u Usage) { got = u }}
	reportUsage(cfg, "bedrock", "m", TokenInfo{InputTokens: 5, OutputTokens: 2})
	if got.Model != "m" || got.InputTokens != 5 {
		t.Fatalf("hook got %+v", got)
	}
}
