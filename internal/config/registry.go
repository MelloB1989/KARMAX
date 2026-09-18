package config

// Declaring which connectors and integrations an install actually manages.
//
// Everything compiled into the binary used to be registered on every install,
// whether or not anyone wanted it. That is defensible for a self-hosted server
// where the operator is the only audience, and wrong for a laptop: a page
// listing a dozen services nobody asked for reads as a to-do list, the health
// prober wakes up for each of them, and a connector the operator would never
// use is still one an agent can be talked into reaching for.
//
// So an install can say what it manages. Saying nothing keeps every connector,
// which is what installs written before this did — an upgrade must not quietly
// switch somebody's integrations off.

import "strings"

// RegistryConfig declares which entries of a registry this install manages.
//
//	connectors:
//	  enabled: [github, google, notion]   # only these
//	  disabled: [instagram]               # and never this
//
// Omitting `enabled` starts from everything compiled in. Writing `enabled: []`
// is different from omitting it: an empty list is an explicit "none", because
// somebody who wrote it meant it. `disabled` is subtracted either way, so
// "everything except one" does not mean enumerating the other eleven.
type RegistryConfig struct {
	Enabled  []string `yaml:"enabled"`
	Disabled []string `yaml:"disabled"`
}

// Manages reports whether this install should register id.
//
// An id may be qualified with an account — `github:work` — and a bare
// `github` covers every account of it, which is what somebody listing their
// providers means. Naming `github:work` exactly is how you keep one account
// and not the other.
func (r RegistryConfig) Manages(id string) bool {
	base, _ := splitID(id)

	for _, d := range r.Disabled {
		if matches(d, id, base) {
			return false
		}
	}
	// Omitted means everything. Present and empty means nothing, and the
	// loop below says so without a special case.
	if r.Enabled == nil {
		return true
	}
	for _, e := range r.Enabled {
		if matches(e, id, base) {
			return true
		}
	}
	return false
}

// Declared lists every name the operator wrote, so a caller can warn about the
// ones this build has never heard of. A typo in an allowlist is otherwise
// silent, and silently missing is exactly how an allowlist fails.
func (r RegistryConfig) Declared() []string {
	out := make([]string, 0, len(r.Enabled)+len(r.Disabled))
	out = append(out, r.Enabled...)
	out = append(out, r.Disabled...)
	return out
}

// Restricts reports whether the operator expressed any opinion at all.
func (r RegistryConfig) Restricts() bool {
	return r.Enabled != nil || len(r.Disabled) > 0
}

func matches(entry, id, base string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if entry == "" {
		return false
	}
	return entry == strings.ToLower(id) || entry == strings.ToLower(base)
}

// splitID separates `provider:account`. Kept here rather than imported so the
// config package stays free of dependencies on the subsystems it configures.
func splitID(id string) (provider, account string) {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[:i], id[i+1:]
	}
	return id, ""
}
