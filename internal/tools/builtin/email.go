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

type EmailTool struct{}

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

	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, fromAddr, recipients, []byte(msg))
	if err != nil {
		return tools.ErrorResult(err), nil
	}

	return tools.SuccessResult(map[string]any{
		"sent_to": to,
		"subject": subject,
	}), nil
}
