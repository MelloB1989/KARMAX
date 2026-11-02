package github

import (
	"path"
	"strings"
)

// Scope limits which repositories an account's notifications become events
// for. A login is often a member of organisations whose traffic is none of the
// operator's business, and every event is something the agent may act on. An
// account the scope does not name is not limited.
type Scope map[string][]string

// ParseScope reads "account=glob,glob;account=glob". Globs match owner/repo,
// case-insensitively, with path.Match: "acme/eval-*" or "acme/*".
func ParseScope(s string) Scope {
	sc := Scope{}
	for _, part := range strings.Split(s, ";") {
		account, globs, ok := strings.Cut(part, "=")
		account = strings.ToLower(strings.TrimSpace(account))
		if !ok || account == "" {
			continue
		}
		for _, g := range strings.Split(globs, ",") {
			if g = strings.ToLower(strings.TrimSpace(g)); g != "" {
				sc[account] = append(sc[account], g)
			}
		}
	}
	return sc
}

// Allows reports whether account's events from repo (owner/name) are wanted.
func (sc Scope) Allows(account, repo string) bool {
	globs, limited := sc[strings.ToLower(account)]
	if !limited {
		return true
	}
	repo = strings.ToLower(repo)
	for _, g := range globs {
		if ok, _ := path.Match(g, repo); ok {
			return true
		}
	}
	return false
}
