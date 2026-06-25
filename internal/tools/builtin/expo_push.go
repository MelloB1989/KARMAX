package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
)

const expoPushURL = "https://exp.host/--/api/v2/push/send"

// SendExpoPush delivers a notification to every registered device via the Expo
// push service. Returns the device count and the raw Expo response. Shared by
// AppPushTool and the proposals subsystem.
func SendExpoPush(s *store.Store, title, body, priority string, data map[string]any) (int, any, error) {
	tokens, err := s.ListPushTokens()
	if err != nil {
		return 0, nil, err
	}
	if len(tokens) == 0 {
		return 0, nil, nil
	}
	if priority != "default" && priority != "high" {
		priority = "high"
	}

	messages := make([]map[string]any, 0, len(tokens))
	for _, tk := range tokens {
		msg := map[string]any{"to": tk.Token, "body": body, "sound": "default", "priority": priority}
		if title != "" {
			msg["title"] = title
		}
		if data != nil {
			msg["data"] = data
		}
		messages = append(messages, msg)
	}

	payload, _ := json.Marshal(messages)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, expoPushURL, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	var result any
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode >= 300 {
		return len(tokens), result, fmt.Errorf("expo push returned %d", resp.StatusCode)
	}
	return len(tokens), result, nil
}
