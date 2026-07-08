package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// WhatsAppSendMediaTool sends a local file (image, PDF, spreadsheet, etc.) as a
// WhatsApp media message via wacli. Pair it with claude_code.call to generate a
// file (a report, a chart, an exported sheet) and then deliver it.
type WhatsAppSendMediaTool struct {
	WacliPath string
}

func (t *WhatsAppSendMediaTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "whatsapp.send_media",
		Description: "Send a file (image, PDF, Excel/CSV, doc, video, audio) as a WhatsApp media message to a chat. " +
			"Provide a LOCAL file path (e.g. one you created via claude_code.call, or downloaded). Optionally include a caption. " +
			"Use this to deliver documents, screenshots, reports, or generated files to the operator or a contact.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {"type": "string", "description": "Recipient chat: JID, phone number, or contact/group name."},
				"path": {"type": "string", "description": "Absolute local path of the file to send."},
				"caption": {"type": "string", "description": "Optional text caption sent with the media."}
			},
			"required": ["to", "path"]
		}`),
	}
}

func (t *WhatsAppSendMediaTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	to, _ := input["to"].(string)
	path, _ := input["path"].(string)
	caption, _ := input["caption"].(string)
	if strings.TrimSpace(to) == "" || strings.TrimSpace(path) == "" {
		return tools.ErrorResult(fmt.Errorf("to and path are required")), nil
	}
	wacli := t.WacliPath
	if wacli == "" {
		wacli = defaultWacliPath()
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	args := []string{"send", "--to", to, "--media", path}
	if strings.TrimSpace(caption) != "" {
		args = append(args, "--text", caption)
	}
	out, err := exec.CommandContext(cctx, wacli, args...).CombinedOutput()
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("wacli send media: %w (%s)", err, strings.TrimSpace(string(out)))), nil
	}
	return tools.SuccessResult(map[string]any{
		"status": "sent", "to": to, "path": path, "output": strings.TrimSpace(string(out)),
	}), nil
}

// WhatsAppViewMediaTool downloads a received WhatsApp media message and reads
// its content — images (described/OCR'd), PDFs, and spreadsheets (extracted) —
// by delegating the actual understanding to the Claude Code harness, which can
// view and parse all of them. Incoming media messages arrive with a marker
// telling the agent the chat + message_id to pass here.
type WhatsAppViewMediaTool struct {
	WacliPath string
	Store     *store.Store
	AgentID   string
	Namespace string
}

func (t *WhatsAppViewMediaTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "whatsapp.view_media",
		Description: "See/understand a media file (image, PDF, Excel/CSV, document) that was RECEIVED in a WhatsApp chat. " +
			"Incoming media messages appear in your context with a marker like '[received an image — chat_id=… message_id=…]'. " +
			"Pass that chat_id and message_id here; it downloads the file and returns a description of the image or the extracted text/data from the document.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"chat": {"type": "string", "description": "The chat_id/JID the media message is in."},
				"message_id": {"type": "string", "description": "The message_id of the media message."},
				"question": {"type": "string", "description": "Optional: a specific thing to look for or answer about the media."}
			},
			"required": ["chat", "message_id"]
		}`),
	}
}

func (t *WhatsAppViewMediaTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	chat, _ := input["chat"].(string)
	messageID, _ := input["message_id"].(string)
	question, _ := input["question"].(string)
	if strings.TrimSpace(chat) == "" || strings.TrimSpace(messageID) == "" {
		return tools.ErrorResult(fmt.Errorf("chat and message_id are required")), nil
	}
	wacli := t.WacliPath
	if wacli == "" {
		wacli = defaultWacliPath()
	}

	// 1) Download the media via wacli → local path.
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	out, err := exec.CommandContext(dctx, wacli, "media", "download", "--chat", chat, "--message-id", messageID).CombinedOutput()
	cancel()
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("wacli media download: %w (%s)", err, strings.TrimSpace(string(out)))), nil
	}
	var dl struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
	}
	_ = json.Unmarshal(out, &dl)
	if !dl.OK || dl.Path == "" {
		return tools.ErrorResult(fmt.Errorf("media download returned no path: %s", oneLineTrunc(string(out), 200))), nil
	}

	// 2) Understand it via the Claude harness (vision for images; text/table
	//    extraction for PDFs and spreadsheets). Ephemeral one-off.
	ask := "The operator received this file in a WhatsApp chat. Open it and tell me what it contains."
	if strings.TrimSpace(question) != "" {
		ask = "The operator received this file in a WhatsApp chat. " + strings.TrimSpace(question)
	}
	prompt := ask + "\n\nFile: " + dl.Path + "\n\n" +
		"If it's an image: describe it and read any text in it. If it's a PDF or document: extract the key text/points. " +
		"If it's a spreadsheet (xlsx/csv): summarize the sheets and pull out the important rows/figures. " +
		"Be accurate and concise; end with a one-line summary."
	cc := &ClaudeCodeTool{Store: t.Store, AgentID: t.AgentID, Namespace: t.Namespace}
	res, cerr := cc.Execute(ctx, map[string]any{"prompt": prompt, "ephemeral": true})
	content := ""
	if cerr == nil && !res.IsError {
		if m, ok := res.Output.(map[string]any); ok {
			content, _ = m["output"].(string)
		}
	}
	if strings.TrimSpace(content) == "" {
		// Downloaded but couldn't extract — still return the path so the agent
		// can send/forward it or ask the operator.
		return tools.SuccessResult(map[string]any{
			"path": dl.Path, "content": "", "note": "downloaded but could not extract content automatically",
		}), nil
	}
	return tools.SuccessResult(map[string]any{"path": dl.Path, "content": content}), nil
}

func oneLineTrunc(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
