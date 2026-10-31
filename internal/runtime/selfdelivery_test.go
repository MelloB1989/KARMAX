package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/store"
)

func bash(cmd string, failed bool) harness.ToolCall {
	in, _ := json.Marshal(map[string]any{"command": cmd})
	return harness.ToolCall{Name: "Bash", Input: in, Failed: failed}
}

// A task session told not to send its result itself may do it anyway, with
// a raw `wacli send` — around comms, where nothing recorded it. What it ran
// is in the round's own tool calls, and that is how the runner knows: the
// text or file it already sent to the requester is not sent again.
func TestWhatATaskSessionSentItselfIsRecognised(t *testing.T) {
	deck := "/srv/out/northwind-deck.pdf"
	for _, tc := range []struct {
		name      string
		calls     []harness.ToolCall
		text      bool
		sentFiles []string
	}{
		{"wacli, text and file", []harness.ToolCall{bash(`wacli send --to 120363000000000001@g.us --text "Here is the deck" --media `+deck, false)}, true, []string{deck}},
		{"a full path to wacli, text only", []harness.ToolCall{bash(`/opt/bin/wacli send --to 120363000000000001 --text "done"`, false)}, true, nil},
		{"comms.send through karmax", []harness.ToolCall{bash(`karmax tool call comms.send channel_id=whatsapp-main target=120363000000000001@g.us content="done"`, false)}, true, nil},
		{"a file only", []harness.ToolCall{bash(`wacli send --to 120363000000000001@g.us --media `+deck, false)}, false, []string{deck}},
		{"a send that failed", []harness.ToolCall{bash(`wacli send --to 120363000000000001@g.us --text "x"`, true)}, false, nil},
		{"another chat", []harness.ToolCall{bash(`wacli send --to 15555550199 --text "x"`, false)}, false, nil},
		{"reading the chat is not sending", []harness.ToolCall{bash(`wacli messages --chat 120363000000000001@g.us --limit 15`, false)}, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, files := sessionDelivered(tc.calls, "120363000000000001@g.us", []string{deck})
			if text != tc.text {
				t.Errorf("text sent = %v, want %v", text, tc.text)
			}
			if len(files) != len(tc.sentFiles) {
				t.Errorf("files sent = %v, want %v", files, tc.sentFiles)
			}
			for _, f := range tc.sentFiles {
				if !files[f] {
					t.Errorf("%s not recognised as sent", f)
				}
			}
		})
	}
}

// longWorkSelfSendCLI is longWorkCLI whose task round delivers the result
// itself with a raw `wacli send`, against its brief.
func longWorkSelfSendCLI(t *testing.T, deck, chat string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude.sh")
	body := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"    *'Continue this task'*)\n" +
		"      echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t3\",\"name\":\"Bash\",\"input\":{\"command\":\"wacli send --to " + chat + " --text \\\"Here you go\\\" --media " + deck + "\"}}]}}'\n" +
		"      echo '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"Sent the deck to the group.\\nREPORT: Here is the onboarding deck for Northwind.\\nFILE: " + deck + "\\nSTATUS: done\",\"usage\":{}}'\n" +
		"      ;;\n" +
		"    *)\n" +
		"      echo '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Bash\",\"input\":{\"command\":\"git log --oneline\"}}]}}'\n" +
		"      sleep 30\n" +
		"      ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole way: the task session sends the result to the requester itself.
// The requester gets the acknowledgement and that one delivery — the runner
// does not post the result or the file a second time — and the task is done.
func TestATaskThatDeliveredItselfIsNotDeliveredAgain(t *testing.T) {
	f := newLongWorkFlowWith(t, "", "", func(t *testing.T, deck string) string {
		return longWorkSelfSendCLI(t, deck, opChat)
	})

	evt := operatorMessage("make an onboarding deck for Northwind from the work we did, no filler")
	if err := f.rt.routeToAgent(evt); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		task, found, _ := f.db.GetTaskForRequest(evt.ID)
		if found && task.Status == store.TaskDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the task never finished: %+v", task)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	got := f.sentTo(opChat)
	if len(got) != 1 || !strings.Contains(got[0].text, "On it") {
		t.Errorf("the requester got %+v from KARMAX, want only the acknowledgement — the session delivered the rest itself", got)
	}
	if owed, _ := f.db.OwedDeliveries("task:"); len(owed) != 0 {
		t.Errorf("results still owed: %+v", owed)
	}
}
