package github

import (
	"path"
	"strings"
)

// Scope limits which repositories an account's notifications become events
// for. A login is often a member of organisations whose traffic is none of the
// operator's business, and every event is something the agent may act on.
//
// Patterns are kept per login, and "*" holds those for every login. A pattern
// limits only the owner it names: with "acme/eval-*", acme's other
// repositories are out, and every other owner's are untouched.
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
// The login's own patterns decide first; then those for every login; an owner
// no pattern names is not limited.
func (sc Scope) Allows(account, repo string) bool {
	repo = strings.ToLower(repo)
	owner, _, _ := strings.Cut(repo, "/")
	for _, key := range []string{strings.ToLower(account), "*"} {
		limited := false
		for _, g := range sc[key] {
			gOwner, _, _ := strings.Cut(g, "/")
			if ok, _ := path.Match(gOwner, owner); !ok {
				continue
			}
			limited = true
			if ok, _ := path.Match(g, repo); ok {
				return true
			}
		}
		if limited {
			return false
		}
	}
	return true
}
