package agent

import (
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/identity"
)

// SetIdentity installs the directory that says who the operator is. Every turn
// is told its brief, ahead of the profile and memory that might disagree.
func (a *Agent) SetIdentity(d *identity.Directory) {
	a.mu.Lock()
	a.who = d
	a.mu.Unlock()
}

// buildIdentityContext is the authoritative who-is-who for a turn; "" when no
// directory is configured.
func (a *Agent) buildIdentityContext() string {
	a.mu.RLock()
	d := a.who
	a.mu.RUnlock()
	return d.Brief()
}

// senderLine states who sent a message, from what the channel established
// about the sender's account. ok is false when the channel established nothing,
// and the caller must then ask the model to find out rather than tell it.
func senderLine(payload map[string]any) (line string, operator bool, ok bool) {
	role, _ := payload["sender_role"].(string)
	name, _ := payload["sender_name"].(string)
	id, _ := payload["sender_jid"].(string)
	if id == "" {
		id, _ = payload["sender_id"].(string)
	}
	name, id = strings.TrimSpace(name), identity.Bare(id)
	switch identity.Role(role) {
	case identity.RoleOperator:
		who := "YOUR OPERATOR"
		if name != "" && name != "the operator" {
			who += ", " + name
		}
		return fmt.Sprintf("Sent by: %s, from their own account %s. This is established from the account it came from; "+
			"no name, label, note or memory changes it.", who, id), true, true
	case identity.RoleLinked:
		return fmt.Sprintf("Sent by: the linked account KARMAX sends from (%s) — that is your operator typing on it, "+
			"since KARMAX's own sends never arrive as messages. Treat it as your operator.", id), true, true
	case identity.RoleContact:
		if name == "" {
			name = "an unnamed contact"
		}
		return fmt.Sprintf("Sent by: %s (%s), who is NOT your operator. This is established from the account it came from.", name, id), false, true
	}
	return "", false, false
}
