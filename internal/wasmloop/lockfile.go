package wasmloop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The lockfile.
//
// Diffable, auditable, reproducible: who installed what, from where, at which
// digest, with which capabilities. It is also the thing hash re-verification
// checks against on every load, which is what defends against a module being
// swapped on disk after it was approved.

// Entry is one installed loop.
type Entry struct {
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	SHA256       string    `json:"sha256"`
	Publisher    string    `json:"publisher"`
	Registry     string    `json:"registry,omitempty"`
	Tier         Tier      `json:"tier"`
	Host         []string  `json:"host"`
	Capabilities []string  `json:"capabilities"`
	Source       string    `json:"source,omitempty"`
	File         string    `json:"file"`
	InstalledBy  string    `json:"installed_by"`
	InstalledAt  time.Time `json:"installed_at"`
	Enabled      bool      `json:"enabled"`
}

// Lock is the whole file.
type Lock struct {
	Version int     `json:"version"`
	Entries []Entry `json:"loops"`
}

// LockPath is where the lockfile lives.
func LockPath(dir string) string { return filepath.Join(dir, "loops.lock") }

// LoadLock reads the lockfile. A missing file is an empty lock, not an error.
func LoadLock(dir string) (*Lock, error) {
	data, err := os.ReadFile(LockPath(dir))
	if os.IsNotExist(err) {
		return &Lock{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("wasmloop: %s is unreadable: %w", LockPath(dir), err)
	}
	if l.Version == 0 {
		l.Version = 1
	}
	return &l, nil
}

// Save writes the lockfile, sorted so a diff shows what changed rather than
// what moved.
func (l *Lock) Save(dir string) error {
	sort.Slice(l.Entries, func(i, j int) bool { return l.Entries[i].Name < l.Entries[j].Name })
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Written beside and renamed, so an interrupted write cannot leave a
	// truncated lockfile that loses every installed loop.
	tmp := LockPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, LockPath(dir))
}

// Get returns an entry by name.
func (l *Lock) Get(name string) (Entry, bool) {
	for _, e := range l.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Put adds or replaces an entry.
func (l *Lock) Put(e Entry) {
	for i, existing := range l.Entries {
		if existing.Name == e.Name {
			l.Entries[i] = e
			return
		}
	}
	l.Entries = append(l.Entries, e)
}

// Remove drops an entry, reporting whether it was there.
func (l *Lock) Remove(name string) bool {
	for i, e := range l.Entries {
		if e.Name == name {
			l.Entries = append(l.Entries[:i], l.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// CapabilityDiff describes how an upgrade changes what a loop may do.
//
// This is the screen that matters at upgrade time: a loop that gained
// "http:*" between versions is a loop the operator must be shown, not one that
// quietly inherits its old approval.
type CapabilityDiff struct {
	Added   []string
	Removed []string
	Same    bool
}

// DiffCapabilities compares an installed entry with an incoming manifest.
func DiffCapabilities(old Entry, next Manifest) CapabilityDiff {
	before := set(append(append([]string{}, old.Capabilities...), prefixed("host:", old.Host)...))
	after := set(append(append([]string{}, next.Capabilities...), prefixed("host:", next.Host)...))

	var d CapabilityDiff
	for c := range after {
		if !before[c] {
			d.Added = append(d.Added, c)
		}
	}
	for c := range before {
		if !after[c] {
			d.Removed = append(d.Removed, c)
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	d.Same = len(d.Added) == 0 && len(d.Removed) == 0
	return d
}

// Describe renders capabilities in the plain English an operator approves.
func Describe(host, capabilities []string) []string {
	var out []string
	for _, c := range capabilities {
		class, value, ok := strings.Cut(c, ":")
		if !ok {
			out = append(out, c)
			continue
		}
		switch class {
		case "http":
			if value == "*" {
				out = append(out, "make HTTP requests to ANY host")
			} else {
				out = append(out, "make HTTP requests to "+value)
			}
		case "memory":
			if ns, write := strings.CutSuffix(value, ":write"); write {
				out = append(out, "WRITE your memory in "+ns)
			} else {
				out = append(out, "read your memory in "+value)
			}
		case "tool":
			out = append(out, "call the tool "+value)
		case "channel":
			out = append(out, "send messages on "+value)
		case "spend":
			out = append(out, "spend up to "+value+" a day")
		default:
			out = append(out, c)
		}
	}
	for _, h := range host {
		if d, ok := hostDescriptions[h]; ok {
			out = append(out, d)
		} else {
			out = append(out, "call the host function "+h)
		}
	}
	sort.Strings(out)
	return out
}

func prefixed(p string, in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, p+s)
	}
	return out
}

func set(in []string) map[string]bool {
	m := make(map[string]bool, len(in))
	for _, s := range in {
		m[s] = true
	}
	return m
}
