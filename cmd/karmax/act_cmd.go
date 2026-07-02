package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// These commands are the CLI's ACTION surface: everything the harness agent
// can do is also reachable here (via GET/POST /api/tools on the running
// daemon), so delegated harnesses (Claude Code) and shell scripts have full
// parity with the orchestrator — retrieve memory, notify the operator's app,
// send messages, schedule work, or call ANY registered tool. Nothing is
// hardcoded: the tool list is read live from the daemon.

// newAskCmd sends a prompt to the orchestrator agent (full toolset + memory)
// and prints its reply.
func newAskCmd() *cobra.Command {
	var agentID string
	cmd := &cobra.Command{
		Use:   "ask <prompt>",
		Short: "Ask the orchestrator agent (full context, memory, and tools)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			out, err := apiPOSTJSON("/api/chat", map[string]any{
				"message": strings.Join(args, " "),
				"agent":   agentID,
			}, 4*time.Minute)
			if err != nil {
				return err
			}
			fmt.Println(asStr(out["reply"]))
			return nil
		},
	}
	cmd.Flags().StringVar(&agentID, "agent", "", "agent to ask (default: the first agent)")
	return cmd
}

// newNotifyCmd pushes a notification to the operator's phone app (feed + push).
func newNotifyCmd() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "notify <title> <body...>",
		Short: "Notify the operator via the phone app (feed + push)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return callTool("app.push", map[string]any{
				"title": args[0],
				"body":  strings.Join(args[1:], " "),
				"kind":  kind,
			}, 30*time.Second)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "update", "category: briefing, reminder, alert, update")
	return cmd
}

// newSendCmd sends a message through a comms channel (WhatsApp by default).
func newSendCmd() *cobra.Command {
	var channelID string
	cmd := &cobra.Command{
		Use:   "send <target> <message...>",
		Short: "Send a message via a comms channel (phone number, contact, or chat JID)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			input := map[string]any{
				"target":  args[0],
				"content": strings.Join(args[1:], " "),
			}
			if channelID != "" {
				input["channel_id"] = channelID
			}
			return callTool("comms.send", input, time.Minute)
		},
	}
	cmd.Flags().StringVar(&channelID, "channel", "", "KARMAX channel id (default: the agent's primary channel)")
	return cmd
}

// callTool invokes a harness tool via the daemon API and prints the result.
func callTool(name string, input map[string]any, timeout time.Duration) error {
	out, err := apiPOSTJSON("/api/tools/"+url.PathEscape(name), input, timeout)
	if err != nil {
		return err
	}
	if ok, _ := out["ok"].(bool); !ok {
		return fmt.Errorf("%s: %s", name, asStr(out["error"]))
	}
	printToolOutput(out["output"])
	return nil
}

func printToolOutput(v any) {
	switch t := v.(type) {
	case nil:
		fmt.Println("ok")
	case string:
		fmt.Println(t)
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Printf("%v\n", v)
			return
		}
		fmt.Println(string(b))
	}
}

// parseKVArgs turns trailing key=value args into a tool input map. Values that
// parse as JSON (numbers, booleans, objects, arrays) are passed typed;
// everything else is a string.
func parseKVArgs(args []string) (map[string]any, error) {
	input := map[string]any{}
	for _, a := range args {
		k, v, found := strings.Cut(a, "=")
		if !found || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", a)
		}
		var typed any
		if err := json.Unmarshal([]byte(v), &typed); err == nil {
			input[k] = typed
		} else {
			input[k] = v
		}
	}
	return input, nil
}
