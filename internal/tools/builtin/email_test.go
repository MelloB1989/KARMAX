package builtin

import (
	"context"
	"errors"
	"net/smtp"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

// An email KARMAX sends as the operator is recorded through the recorder every
// acting tool shares, so it is in the turn's "Sent as you" report without the
// model declaring it. One that fails is not.
func TestASentEmailIsRecordedAsSentAsTheOperator(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.example.test")
	t.Setenv("SMTP_FROM", "operator@example.test")
	var got []tools.SentAs
	fail := false
	tool := &EmailTool{send: func(string, smtp.Auth, string, []string, []byte) error {
		if fail {
			return errors.New("550 mailbox unavailable")
		}
		return nil
	}}
	tool.RecordSentWith(func(s tools.SentAs) { got = append(got, s) })

	res, err := tool.Execute(context.Background(), map[string]any{"to": "dana@example.com", "subject": "Release notes", "body": "Attached."})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}
	want := tools.SentAs{Service: "email", Account: "operator@example.test", To: "dana@example.com", Action: "emailed", Text: "Release notes"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("recorded %+v, want %+v", got, want)
	}

	fail = true
	if res, _ := tool.Execute(context.Background(), map[string]any{"to": "dana@example.com", "subject": "x", "body": "y"}); !res.IsError {
		t.Fatal("a refused email was reported as sent")
	}
	if len(got) != 1 {
		t.Errorf("a failed email was recorded: %+v", got)
	}
}
