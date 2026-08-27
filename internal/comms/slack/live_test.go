package slack

import (
	"context"
	"os"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestLiveSocketConnects is the one test that actually dials Slack. It only
// runs when real credentials are exported, so `go test ./...` stays green on
// every machine that doesn't have them — which is every machine except
// whoever is deliberately checking Socket Mode against a live workspace.
func TestLiveSocketConnects(t *testing.T) {
	appToken := os.Getenv("SLACK_LIVE_TEST_APP_TOKEN")
	botToken := os.Getenv("SLACK_LIVE_TEST_BOT_TOKEN")
	if appToken == "" || botToken == "" {
		t.Skip("SLACK_LIVE_TEST_APP_TOKEN / SLACK_LIVE_TEST_BOT_TOKEN not set; skipping the live Socket Mode test")
	}

	c := New("live-test", appToken, botToken, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	defer c.Stop()
}
