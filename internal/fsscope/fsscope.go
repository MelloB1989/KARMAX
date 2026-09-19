// Package fsscope is what the assistant may touch on this machine.
//
// KARMAX runs a coding harness with real file and shell tools on somebody's
// laptop. Until now that meant all of it: every repository, every folder of
// scanned documents, the SSH keys, the browser profile holding a cookie for
// every account they had signed into. The person who asked it to tidy up a
// project had also, without being asked, handed it their home directory.
//
// So there is a policy: a denylist that always wins, and a set of directories
// the assistant is pointed at. The denylist is the part with teeth, and the
// part worth having even when somebody opens up everything else — a credential
// store is never what they meant, and "read every file under my home" said out
// loud is not the same as "read my private key".
//
// Enforcement is Claude Code's own permission system, which is the only thing
// on this machine that can actually stop a read — a rule written here blocks
// `cat` through the Bash tool, not merely the Read tool. What KARMAX does is
// decide the rules and refuse to pass the flag that would ignore them.
package fsscope

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Grant is a directory the assistant may use.
type Grant struct {
	Path string `json:"path"`
	// Write is whether it may change what is there, as opposed to reading it.
	Write bool `json:"write"`
}

// Policy is the whole answer to "what may it touch".
type Policy struct {
	// Enforced is whether the rules are applied at all.
	//
	// False on an instance nobody has answered the question for, which keeps
	// the harness working exactly as it did before this existed. Setting any
	// policy turns it on; that is the moment the operator has said something,
	// and acting on a policy they never stated would be a worse surprise than
	// not having one.
	Enforced bool `json:"enforced"`

	// Grants are the directories it WORKS in — handed to the harness as its
	// working set. A grant is an instruction, not a fence: the only half of it
	// that is enforced is Write, because a read-only grant is expressed as a
	// denial of edits there. See rules.go for why "only these folders" is not
	// something this can honestly promise.
	Grants []Grant `json:"grants"`

	// Deny are paths it may never reach, and this half is a real block — it
	// stops `cat` through the shell, not only the Read tool. Extra ones the
	// operator adds are kept here; the standard list is not stored, so it
	// improves without anybody editing a file.
	Deny []string `json:"deny"`
}

// Load reads the policy for a data directory. A missing file is the default.
func Load(dataDir string) Policy {
	b, err := os.ReadFile(Path(dataDir))
	if err != nil {
		return Policy{}
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return Policy{}
	}
	return p
}

// Save writes the policy back.
func Save(dataDir string, p Policy) error {
	path := Path(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file names the directories somebody has decided are sensitive,
	// which is itself worth not publishing.
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Path is where the policy lives, for a caller that wants to say so.
func Path(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dataDir = filepath.Join(home, ".karmax")
	}
	return filepath.Join(dataDir, "access.json")
}

// Allow adds or updates a grant. The path is made absolute and must exist:
// granting a directory that is not there is almost always a typo, and finding
// out at the moment the assistant needed it is too late to be useful.
func (p *Policy) Allow(path string, write bool) error {
	abs, err := clean(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("%s: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a file; grant the folder it is in", abs)
	}
	for i := range p.Grants {
		if same(p.Grants[i].Path, abs) {
			p.Grants[i].Write = write
			return nil
		}
	}
	p.Grants = append(p.Grants, Grant{Path: abs, Write: write})
	sort.Slice(p.Grants, func(i, j int) bool { return p.Grants[i].Path < p.Grants[j].Path })
	p.Enforced = true
	return nil
}

// Revoke removes a grant.
func (p *Policy) Revoke(path string) error {
	abs, err := clean(path)
	if err != nil {
		return err
	}
	kept := p.Grants[:0]
	found := false
	for _, g := range p.Grants {
		if same(g.Path, abs) {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	p.Grants = kept
	if !found {
		return fmt.Errorf("%s was not granted", abs)
	}
	p.Enforced = true
	return nil
}

// Forbid adds a path the assistant may never reach.
func (p *Policy) Forbid(path string) error {
	abs, err := clean(path)
	if err != nil {
		return err
	}
	for _, d := range p.Deny {
		if same(d, abs) {
			return nil
		}
	}
	p.Deny = append(p.Deny, abs)
	sort.Strings(p.Deny)
	p.Enforced = true
	return nil
}

// Unforbid removes one of the operator's own denials. The standard list cannot
// be removed this way, because it is not stored — see StandardDenies.
func (p *Policy) Unforbid(path string) error {
	abs, err := clean(path)
	if err != nil {
		return err
	}
	kept := p.Deny[:0]
	found := false
	for _, d := range p.Deny {
		if same(d, abs) {
			found = true
			continue
		}
		kept = append(kept, d)
	}
	p.Deny = kept
	if !found {
		return errors.New("that was not one of your own denials; the standard ones cannot be removed")
	}
	return nil
}

// Dirs are the directories to hand a harness as its working set.
func (p Policy) Dirs() []string {
	out := make([]string, 0, len(p.Grants))
	for _, g := range p.Grants {
		out = append(out, g.Path)
	}
	return out
}

func clean(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("no path given")
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Symlinks are resolved so a grant cannot be widened by pointing a link at
	// somewhere else afterwards — and so /tmp and /private/tmp on macOS are one
	// answer rather than two.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// same compares two paths, case-insensitively where the filesystem is.
func same(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
