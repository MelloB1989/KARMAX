package server

import "net/http"

// verifiedAgent is the agent a relay-scope request is from, when its
// identity checks out. A stand-in until agent identity lands: it verifies
// nobody, so no agent can read a secret yet. Replaced at merge.
func verifiedAgent(r *http.Request) (name string, ok bool) { return "", false }
