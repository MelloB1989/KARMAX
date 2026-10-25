package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// SandboxKey is a sandbox agent's one credential: the HMAC of its name under
// the full token, in a namespace of its own, so it is never an agent's Key
// (which a relay request pairs with the relay token). fleetd takes it as a
// bearer token that means exactly "sandbox agent X". Empty without a full
// token.
func SandboxKey(full, agent string) string {
	if full == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(full))
	m.Write([]byte("sandbox:" + agent))
	return hex.EncodeToString(m.Sum(nil))
}

// SandboxOf is the sandbox agent, among agents, whose key bearer is.
func SandboxOf(full, bearer string, agents []string) (string, bool) {
	if full == "" || bearer == "" {
		return "", false
	}
	for _, a := range agents {
		if hmac.Equal([]byte(bearer), []byte(SandboxKey(full, a))) {
			return a, true
		}
	}
	return "", false
}
