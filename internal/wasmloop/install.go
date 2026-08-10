package wasmloop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/store"
)

// Install as a transaction: fetch, verify, record, activate.
//
// The old path did `go get` and rebuilt the daemon, which meant the decision to
// trust a loop and the act of running it were the same moment. Here the bytes
// sit inert until every check has passed and the operator has seen what the
// module is asking for.

// Store is what install needs to record grants. An interface so the steps can
// be tested without a database.
type Store interface {
	SaveGrant(store.Grant) error
	RevokeSubject(string) (int64, error)
}

// Installer performs installs against a directory.
type Installer struct {
	Dir    string // ~/.karmax/loops
	Broker Store
	Trust  Trust
	// Actor is recorded in the lockfile, so "who installed this" has an answer.
	Actor string
}

// Preview is what the operator is shown before anything is written.
type Preview struct {
	Manifest Manifest
	Verdict  *Verdict
	// Grants is the plain-English list of what it will be allowed to do.
	Grants []string
	// Diff is set when this replaces an installed version.
	Diff *CapabilityDiff
	// Upgrade is the version being replaced, if any.
	Upgrade string
}

// Inspect verifies an artifact and reports what installing it would mean,
// without writing anything.
func (in *Installer) Inspect(data []byte) (*Preview, error) {
	a, v, err := VerifyBytes(data, in.Trust)
	if err != nil {
		return nil, err
	}
	p := &Preview{
		Manifest: a.Manifest,
		Verdict:  v,
		Grants:   Describe(a.Manifest.Host, a.Manifest.Capabilities),
	}

	lock, err := LoadLock(in.Dir)
	if err != nil {
		return nil, err
	}
	if old, ok := lock.Get(a.Manifest.Name); ok {
		d := DiffCapabilities(old, a.Manifest)
		p.Diff = &d
		p.Upgrade = old.Version
	}
	return p, nil
}

// Install writes the artifact, records it, and grants exactly what its manifest
// declared.
//
// Order matters: the file is written before the lockfile so a lockfile entry
// never names a file that is not there, and grants are replaced wholesale so an
// upgrade that dropped a capability actually loses it.
func (in *Installer) Install(data []byte) (*Preview, error) {
	p, err := in.Inspect(data)
	if err != nil {
		return nil, err
	}
	a, err := Unpack(data)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(in.Dir, 0o755); err != nil {
		return nil, err
	}
	file := filepath.Join(in.Dir, a.Manifest.Name+".kloop")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, file); err != nil {
		return nil, err
	}

	subject := broker.LoopSubject(a.Manifest.Name)
	// Replaced rather than merged: an upgrade that no longer asks for a
	// capability must not keep it because a previous version did.
	if _, err := in.Broker.RevokeSubject(subject); err != nil {
		return nil, fmt.Errorf("wasmloop: could not clear previous grants: %w", err)
	}
	for _, c := range a.Manifest.Capabilities {
		class, value, ok := strings.Cut(c, ":")
		if !ok {
			return nil, fmt.Errorf("wasmloop: %q is not <capability>:<value>", c)
		}
		if err := in.Broker.SaveGrant(store.Grant{
			Subject: subject, Capability: class, Value: value,
			GrantedBy: "loop-manifest:" + a.Manifest.Version,
		}); err != nil {
			return nil, err
		}
	}
	// No auto-grants for host functions. A manifest that asks for `notify` must
	// also ask for `tool:app.push`, so the list the operator approves is the
	// whole list — not a visible half plus an inferred remainder.

	lock, err := LoadLock(in.Dir)
	if err != nil {
		return nil, err
	}
	lock.Put(Entry{
		Name: a.Manifest.Name, Version: a.Manifest.Version, SHA256: a.Manifest.SHA256,
		Publisher: p.Verdict.Publisher, Registry: p.Verdict.Registry, Tier: p.Verdict.Tier,
		Host: a.Manifest.Host, Capabilities: a.Manifest.Capabilities,
		Source: a.Manifest.SourceURL, File: filepath.Base(file),
		InstalledBy: in.Actor, InstalledAt: time.Now(), Enabled: true,
	})
	if err := lock.Save(in.Dir); err != nil {
		return nil, err
	}
	return p, nil
}

// Remove uninstalls a loop and takes its capabilities with it.
func (in *Installer) Remove(name string) error {
	lock, err := LoadLock(in.Dir)
	if err != nil {
		return err
	}
	entry, ok := lock.Get(name)
	if !ok {
		return fmt.Errorf("wasmloop: %q is not installed", name)
	}
	if _, err := in.Broker.RevokeSubject(broker.LoopSubject(name)); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(in.Dir, entry.File))
	lock.Remove(name)
	return lock.Save(in.Dir)
}

// Load reads an installed loop back, re-verifying it.
//
// The digest is checked against the LOCKFILE, not just against the manifest
// inside the file — otherwise someone who replaced the artifact wholesale, with
// a matching self-consistent manifest, would pass. What was approved is what is
// recorded, and that is what has to still be true.
func (in *Installer) Load(name string) (*Artifact, error) {
	lock, err := LoadLock(in.Dir)
	if err != nil {
		return nil, err
	}
	entry, ok := lock.Get(name)
	if !ok {
		return nil, fmt.Errorf("wasmloop: %q is not installed", name)
	}
	data, err := os.ReadFile(filepath.Join(in.Dir, entry.File))
	if err != nil {
		return nil, fmt.Errorf("wasmloop: %s is in the lockfile but its file is missing: %w", name, err)
	}
	a, v, err := VerifyBytes(data, in.Trust)
	if err != nil {
		return nil, err
	}
	if a.Digest() != strings.ToLower(entry.SHA256) {
		return nil, fmt.Errorf("wasmloop: %s on disk is not what the lockfile recorded\n"+
			"  approved %s\n  found    %s\nit was replaced after installation; refusing to run it",
			name, entry.SHA256, a.Digest())
	}
	if v.Tier != entry.Tier {
		return nil, fmt.Errorf("wasmloop: %s is now %s but was installed as %s", name, v.Tier, entry.Tier)
	}
	return a, nil
}

// Installed lists what is in the lockfile.
func (in *Installer) Installed() ([]Entry, error) {
	lock, err := LoadLock(in.Dir)
	if err != nil {
		return nil, err
	}
	return lock.Entries, nil
}

// Dir is where artifacts and the lockfile live.
func Dir() string {
	if d := strings.TrimSpace(os.Getenv("KARMAX_LOOPS_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "loops"
	}
	return filepath.Join(home, ".karmax", "loops")
}
