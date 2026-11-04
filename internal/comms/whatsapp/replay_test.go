package whatsapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// The incident: after a restart a group message reached the monitor without
// its group flag, and the monitor answered it as a direct chat — in the group.
// Whether a chat is a group follows from its address, so a missing flag cannot
// make one a direct chat.
func TestAGroupAddressIsAGroupWhateverTheEventSays(t *testing.T) {
	w := channelFor(t)
	m := route(t, w, `{"event":"incoming_message","payload":{
		"chat":{"jid":"120363000000000009@g.us"},
		"message":{"id":"g1","chat_jid":"120363000000000009@g.us","sender_jid":"555000111222:7@lid","content":"- brush ka thing"},
		"source":"whatsapp_event"}}`)

	if m.Metadata["is_group"] != true {
		t.Errorf("is_group = %v for a g.us chat; a group message must never read as a direct chat", m.Metadata["is_group"])
	}
	if m.SenderID != "555000111222" {
		t.Errorf("SenderID = %q, want the member who wrote it, not the group", m.SenderID)
	}
	for _, jid := range []string{"15550001111@s.whatsapp.net", "123456789@lid", "status@broadcast", "120363000000000001@newsletter"} {
		if IsGroupJID(jid) {
			t.Errorf("%s is not a group", jid)
		}
	}
}

// wacli retries a delivery it is unsure of, and a missed one is redelivered
// after a restart: either brings the same message again. It arrives under the
// same id, which is how the store recognises the repeat and the agent hears it
// once (comms.TestAMessageThatArrivesAgainIsNotActedOnAgain).
func TestAMessageKeepsItsIdWhenItArrivesAgain(t *testing.T) {
	w := channelFor(t)
	body := `{"event":"incoming_message","payload":{
		"chat":{"jid":"15550003333@s.whatsapp.net"},
		"message":{"id":"dup-1","chat_jid":"15550003333@s.whatsapp.net","sender_jid":"15550003333@s.whatsapp.net","content":"are we on for 5?"},
		"source":"whatsapp_event"}}`
	first, again := route(t, w, body), route(t, w, body)
	if first.ID != again.ID {
		t.Errorf("the same message arrived as %q and then %q; a repeat must be recognisable", first.ID, again.ID)
	}
	if other := route(t, w, strings.Replace(body, `"dup-1"`, `"dup-2"`, 1)); other.ID == first.ID {
		t.Errorf("two different messages share the id %q", other.ID)
	}
	// The same message id in another chat is another message.
	if elsewhere := route(t, w, strings.ReplaceAll(body, "15550003333", "15550004444")); elsewhere.ID == first.ID {
		t.Errorf("messages in two chats share the id %q", elsewhere.ID)
	}
	// Without an id there is nothing to recognise, and nothing is merged.
	noID := strings.Replace(body, `"id":"dup-1",`, "", 1)
	if a, b := route(t, w, noID), route(t, w, noID); a.ID == b.ID {
		t.Errorf("two messages without ids were given one id %q", a.ID)
	}
}

// A restart's missed messages are the deliveries wacli could not make to this
// webhook: those, and only those, are sent again. A message a later attempt
// delivered, another webhook's traffic, a call long over and anything older
// than the window are left alone, and each missed message goes once.
func TestOnlyWhatTheWebhookMissedIsRedelivered(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	rec := func(id int, webhook int, url, event, msg, status, created string) map[string]any {
		return map[string]any{"id": id, "webhook_id": webhook, "url": url, "event": event,
			"message_id": msg, "status": status, "created_at": created}
	}
	here, elsewhere := "http://127.0.0.1:9090"+WebhookPath, "http://127.0.0.1:7000/hooks/other"
	failed := []map[string]any{
		rec(1, 35, here, "incoming_message", "missed", "failed", at(time.Minute)),
		rec(2, 35, here, "incoming_message", "missed", "failed", at(2*time.Minute)), // the same message, failed twice
		rec(3, 35, here, "incoming_message", "retried-ok", "failed", at(time.Minute)),
		rec(4, 9, elsewhere, "incoming_message", "not-ours", "failed", at(time.Minute)),
		rec(5, 35, here, "call.incoming", "a-call", "failed", at(time.Minute)),
		rec(6, 35, here, "incoming_message", "stale", "failed", at(13*time.Hour)),
		rec(7, 35, here, "outgoing_message", "own-send", "failed", at(time.Minute)),
	}
	byMessage := map[string][]map[string]any{
		"missed":     {failed[0], failed[1]},
		"retried-ok": {failed[2], rec(8, 35, here, "incoming_message", "retried-ok", "delivered", at(time.Minute))},
		"own-send":   {failed[6]},
	}
	write := func(name string, v any) {
		b, _ := json.Marshal(map[string]any{"webhook_deliveries": v})
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("failed.json", failed)
	for msg, recs := range byMessage {
		write("q-"+msg+".json", recs)
	}
	// A fake wacli: lists from the files above, and records what it was asked
	// to redeliver.
	script := `#!/bin/sh
d=` + dir + `
case "$1 $2" in
"webhooks deliveries")
  case "$3" in
  --status) cat "$d/failed.json" ;;
  --query) if [ -f "$d/q-$4.json" ]; then cat "$d/q-$4.json"; else echo '{"webhook_deliveries":[]}'; fi ;;
  esac ;;
"webhooks replay") echo "$4" >> "$d/replayed"; echo '{"ok":true,"delivery":{"status":"delivered"}}' ;;
*) echo "unexpected: $*" >&2; exit 2 ;;
esac
`
	fake := filepath.Join(dir, "wacli")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	w := New("wa", fake, "", "", zap.NewNop())
	w.sweepMissed(context.Background())

	raw, _ := os.ReadFile(filepath.Join(dir, "replayed"))
	got := strings.Fields(string(raw))
	sort.Strings(got)
	if want := []string{"1", "7"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("redelivered deliveries %v, want %v: the missed message once, and the account's own missed send", got, want)
	}
}
