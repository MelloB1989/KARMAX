package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/identity"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func tempStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "k.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The directory is built from configuration: the operator's name from the
// environment, their accounts from the operator chats, and what it has learned
// before from the store.
func TestTheDirectoryIsBuiltFromConfigurationAndRemembersWhatItLearned(t *testing.T) {
	s := tempStore(t)
	env := map[string]string{"KARMAX_OPERATOR_NAME": "Priya"}
	getenv := func(k string) string { return env[k] }

	d := newIdentity(getenv, []string{"15550001111"}, s)
	if d.Name() != "Priya" || !d.IsOperator("15550001111@s.whatsapp.net") {
		t.Fatalf("directory not built from configuration: name=%q", d.Name())
	}
	if !d.LearnAlias("15550001111@s.whatsapp.net", "123456789@lid") {
		t.Fatal("did not learn")
	}

	again := newIdentity(getenv, []string{"15550001111"}, s)
	if !again.IsOperator("123456789:3@lid") {
		t.Error("a learned alias did not survive a restart")
	}
}

// The labeller tells KARMAX's own sends from the operator typing on the same
// account, and names everybody else from their own contact record.
func TestTheThreadLabellerTellsKarmaxFromTheOperator(t *testing.T) {
	d := identity.New("Priya", []string{"123456789@lid"})
	d.SetLinked("15550002222@s.whatsapp.net")
	sent := func(chat, text string, _ time.Time) bool { return text == "sent by KARMAX" }
	names := func(_ context.Context, jid string) string {
		if identity.Bare(jid) == "555000444" {
			return "Omar"
		}
		return ""
	}
	label := speakerLabeller(d, sent, names)

	for _, c := range []struct {
		msg  map[string]any
		from string
	}{
		{map[string]any{"sender_jid": "15550002222:63@s.whatsapp.net", "is_from_me": true, "content": "sent by KARMAX",
			"chat_jid": "120363000000000001@g.us", "timestamp": "2026-10-28T12:58:57+05:30"}, "KARMAX (on the linked account)"},
		{map[string]any{"sender_jid": "15550002222:63@s.whatsapp.net", "is_from_me": true, "content": "typed by hand",
			"chat_jid": "120363000000000001@g.us", "timestamp": "2026-10-28T12:58:57+05:30"}, "Priya (your operator, typing on the linked account)"},
		{map[string]any{"sender_jid": "123456789:4@lid", "is_from_me": false, "content": "x"}, "Priya (your operator)"},
		{map[string]any{"sender_jid": "555000444@lid", "is_from_me": false, "content": "x"}, "Omar (not your operator)"},
	} {
		label(context.Background(), c.msg)
		if c.msg["from"] != c.from {
			t.Errorf("from = %v, want %q", c.msg["from"], c.from)
		}
	}
}

// The linked account is whatever the bridge says it is linked to.
func TestTheLinkedAccountIsAskedOfTheBridge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"connected":true,"user_jid":"15550002222:63@s.whatsapp.net","user_lid":"987654321:63@lid"}`))
	}))
	defer srv.Close()

	ids, err := linkedAccount(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := identity.New("Priya", nil)
	d.SetLinked(ids...)
	if d.RoleOf("987654321@lid") != identity.RoleLinked || d.RoleOf("15550002222") != identity.RoleLinked {
		t.Errorf("linked ids not taken from the bridge: %v", ids)
	}
	if !strings.Contains(d.Brief(), "987654321@lid") {
		t.Error("the brief does not name the linked LID")
	}
}
