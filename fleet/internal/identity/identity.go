// Package identity says which agent a relay request is from. Every agent
// holds the one relay token, so the token cannot: each container also gets
// a key of its own, Key(full token, its agent), in its env file
// (FLEET_AGENT_KEY), and fleetctl sends it as Header. Only fleetd and the
// operator hold the full token, so no agent can make another's key, and
// fleetd checks one without storing any.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Header carries Value(worker, key) on a request.
const Header = "X-Fleet-Agent"

// Key is an agent's key: the HMAC of its name under the full token. Empty
// without a full token.
func Key(full, agent string) string {
	if full == "" {
		return ""
	}
	m := hmac.New(sha256.New, []byte(full))
	m.Write([]byte("agent:" + agent))
	return hex.EncodeToString(m.Sum(nil))
}

// Value is the header's value: the worker's name and its container's key.
func Value(name, key string) string { return name + ":" + key }

// slot is a worker's suffix after its container's agent: agent-03-s2.
var slot = regexp.MustCompile(`^(agent-\d+)-s\d+$`)

// Base is the container agent a worker runs in: agent-03 for agent-03,
// agent-03:2 and agent-03-s2. One container, one key, for all its workers.
func Base(name string) string {
	if b, _, ok := strings.Cut(name, ":"); ok {
		return b
	}
	if m := slot.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// Verify reads a header's value and returns the worker it names when the
// key is that worker's container's.
func Verify(full, value string) (string, bool) {
	i := strings.LastIndex(value, ":")
	if full == "" || i < 1 || i == len(value)-1 {
		return "", false
	}
	name, key := value[:i], value[i+1:]
	if !hmac.Equal([]byte(key), []byte(Key(full, Base(name)))) {
		return "", false
	}
	return name, true
}
