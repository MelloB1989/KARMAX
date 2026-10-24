package server

// The test side of identity_stub.go, with the v2-memory branch's helper
// names: at merge, delete this file with it.

import (
	"net/http"
	"strings"
)

func init() {
	stubVerified = func(r *http.Request) (string, bool) {
		name, ok := strings.CutSuffix(r.Header.Get("X-Fleet-Agent"), ":stub-key")
		return name, ok && name != ""
	}
}

// asAgent makes r a request from the named agent, proved.
func asAgent(r *http.Request, name string) *http.Request {
	r.Header.Set("X-Fleet-Agent", name+":stub-key")
	return r
}

// OperatorAssigner: a member's login is not the operator's side.
func (f *fake) OperatorAssigner(assigner string) bool {
	return assigner != "" && !strings.Contains(assigner, "@")
}
