package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
	"go.uber.org/zap"
)

// The incident: the gateway was lent the full WhatsApp CLI. Answering a group
// message, it messaged the sender directly itself, then returned "I sent the
// sender a short reply…" as its reply, which the loop sent into the chat — the
// account of what it did, meant for the operator, delivered to the third
// party. A gateway call decides and drafts; it never sends.
func TestTheGatewayIsNeverLentAWayToSend(t *testing.T) {
	// Never the real binary: if the guard regressed, these calls would post,
	// message and ring from the operator's account. The fake records what it
	// was asked to do.
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "wacli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := lentWacli
	lentWacli = func() string { return fake }
	t.Cleanup(func() { lentWacli = real })

	rt := &KarmaxRuntime{tools: tools.NewRegistry(), log: zap.NewNop()}

	wacli, ok := rt.lendableTool("wacli")
	if !ok {
		t.Fatal("wacli is not lendable at all; reading chats is what the gateway needs it for")
	}
	for _, args := range [][]any{
		{"send", "--to", "15550001111", "--text", "hi"},
		{"bulk-send", "--file", "x.json"},
		{"edit", "--chat", "c", "--id", "m", "--text", "x"},
		{"delete", "--chat", "c", "--id", "m"},
		{"story", "--text", "x"},
		{"call", "--to", "15550001111"},
		{"contacts", "update", "--jid", "j"},
		{"access", "unlock", "c"},
		{"api", "POST", "/send"},
		{"--json", "send"},
	} {
		out, err := wacli.Run(context.Background(), map[string]any{"args": args})
		if err == nil || !strings.Contains(err.Error(), "only reads") {
			t.Errorf("wacli %v was allowed (out %q, err %v); the gateway must not be able to act", args, out, err)
		}
	}

	if ran, _ := os.ReadFile(calls); len(ran) > 0 {
		t.Errorf("the guard let these reach wacli:\n%s", ran)
	}
	if _, err := wacli.Run(context.Background(), map[string]any{"args": []any{"messages", "--chat", "c"}}); err != nil {
		t.Errorf("a read was refused: %v", err)
	}
	if ran, _ := os.ReadFile(calls); !strings.Contains(string(ran), "messages --chat c") {
		t.Errorf("the read did not reach wacli: %q", ran)
	}

	for _, args := range [][]string{
		{"messages", "--chat", "c", "--limit", "15"},
		{"resolve", "Omar"},
		{"chats", "--query", "x"},
		{"contacts", "lookup", "Omar"},
		{"media", "download", "--chat", "c", "--id", "m"},
		{"receipts", "--id", "m"},
	} {
		if !wacliReads(args) {
			t.Errorf("wacli %v only reads and should be allowed", args)
		}
	}

	for _, name := range []string{"whatsapp_send_message", "whatsapp.send_message", "comms_send", "gmail_send"} {
		if _, ok := rt.lendableTool(name); ok {
			t.Errorf("%s was lent to the gateway; a send goes through the loop's gated path", name)
		}
	}
}
