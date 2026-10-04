package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/runtime"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/spf13/cobra"
)

func callCmd() *cobra.Command {
	var to, brief string
	cmd := &cobra.Command{
		Use:   "call",
		Short: "Place a voice call that KARMAX's brain conducts",
		Example: "  karmax call --to operator --brief 'ask whether the deploy can go out today'\n" +
			"  karmax call --to 919876543210@s.whatsapp.net --brief '...'",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := config.Load(findConfig())
			if err != nil {
				return err
			}
			brain := runtime.BrainURL(cfg)
			if brain == "" {
				return fmt.Errorf("voice is off (KARMAX_VOICE or webhooks disabled)")
			}
			jid, err := resolveCallTarget(to, cfg)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
			defer cancel()
			if err := placeBrainCall(ctx, hostpaths.WacliAPIURL(), jid, brief, brain); err != nil {
				return err
			}
			fmt.Println("calling.")
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "a WhatsApp jid, or \"operator\"")
	cmd.Flags().StringVar(&brief, "brief", "", "what the call is for")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// resolveCallTarget turns "operator" into the first operator chat.
func resolveCallTarget(to string, cfg *config.KarmaxConfig) (string, error) {
	to = strings.TrimSpace(to)
	if !strings.EqualFold(to, "operator") {
		return to, nil
	}
	if chats := builtin.OperatorChats(); len(chats) > 0 {
		return chats[0], nil
	}
	for _, ch := range cfg.Comms.Channels {
		if strings.EqualFold(ch.Type, "whatsapp") && ch.Settings["target_chat"] != "" {
			return ch.Settings["target_chat"], nil
		}
	}
	return "", fmt.Errorf("no operator chat configured (WHATSAPP_OPERATOR_CHATS)")
}

// placeBrainCall asks wacli to ring jid and hold the call against KARMAX's brain.
func placeBrainCall(ctx context.Context, apiURL, jid, brief, brainURL string) error {
	body, err := json.Marshal(map[string]any{"to": jid, "brain": true, "brief": brief, "brain_url": brainURL})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiURL, "/")+"/calls", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("wacli refused (%s): %.300s", resp.Status, raw)
	}
	return nil
}
