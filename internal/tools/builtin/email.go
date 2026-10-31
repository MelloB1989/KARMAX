package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/smtp"
	"os"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
)

// EmailTool sends mail from the operator's configured SMTP account, and
// records each sent message as sent as the operator.
type EmailTool struct {
	sent tools.SentRecorder
	// send delivers a message; smtp.SendMail when nil.
	send func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

// RecordSentWith gives the tool the recorder every acting tool shares.
func (t *EmailTool) RecordSentWith(r tools.SentRecorder) { t.sent = r }

func (t *EmailTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name:        "email.send",
		Description: "Send an email via SMTP",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {"type": "string"},
				"subject": {"type": "string"},
				"body": {"type": "string"},
				"html": {"type": "boolean"}
			},
			"required": ["to", "subject", "body"]
		}`),
	}
}

func (t *EmailTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	to, _ := input["to"].(string)
	subject, _ := input["subject"].(string)
	body, _ := input["body"].(string)

	if to == "" || subject == "" {
		return tools.ErrorResult(fmt.Errorf("to and subject are required")), nil
	}

	smtpHost := os.Getenv("SMTP_HOST")
	smtpPort := os.Getenv("SMTP_PORT")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	fromAddr := os.Getenv("SMTP_FROM")

	if smtpHost == "" {
		return tools.ErrorResult(fmt.Errorf("SMTP_HOST not configured")), nil
	}
	if smtpPort == "" {
		smtpPort = "587"
	}
	if fromAddr == "" {
		fromAddr = smtpUser
	}

	contentType := "text/plain"
	if isHTML, _ := input["html"].(bool); isHTML {
		contentType = "text/html"
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: %s; charset=UTF-8\r\n\r\n%s",
		fromAddr, to, subject, contentType, body)

	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	recipients := strings.Split(to, ",")
	for i, r := range recipients {
		recipients[i] = strings.TrimSpace(r)
	}

	send := t.send
	if send == nil {
		send = smtp.SendMail
	}
	if err := send(smtpHost+":"+smtpPort, auth, fromAddr, recipients, []byte(msg)); err != nil {
		return tools.ErrorResult(err), nil
	}
	t.sent.Record(tools.SentAs{Service: "email", Account: fromAddr, To: to, Action: "emailed", Text: subject})

	return tools.SuccessResult(map[string]any{
		"sent_to": to,
		"subject": subject,
	}), nil
}
