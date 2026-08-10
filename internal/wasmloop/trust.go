package wasmloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Where trust is configured.
//
// One file, read by both the CLI and the daemon. They used to disagree: the
// daemon took its trusted keys from a systemd Environment= line and the CLI
// from the shell, so `wloop verify` reported failures for loops the daemon was
// happily running. A permission model whose answer depends on which process you
// ask is not one anybody can reason about.
//
// The environment still adds to it, for a container that would rather not
// mount a file.

// trustFile is the on-disk form.
type trustFile struct {
	Registries     []string `json:"registries"`
	Revoked        []string `json:"revoked"`
	AllowCommunity bool     `json:"allow_community"`
}

// TrustPath is where the configuration lives.
func TrustPath(dir string) string { return filepath.Join(dir, "trust.json") }

// LoadTrust reads the trust configuration, merging the environment on top.
func LoadTrust(dir string) Trust {
	var f trustFile
	if data, err := os.ReadFile(TrustPath(dir)); err == nil {
		_ = json.Unmarshal(data, &f)
	}
	t := Trust{
		Registries:     f.Registries,
		Revoked:        f.Revoked,
		AllowCommunity: f.AllowCommunity,
	}
	t.Registries = append(t.Registries, splitList(os.Getenv("KARMAX_LOOP_REGISTRIES"))...)
	t.Revoked = append(t.Revoked, splitList(os.Getenv("KARMAX_LOOP_REVOKED"))...)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("KARMAX_LOOP_ALLOW_COMMUNITY")), "true") {
		t.AllowCommunity = true
	}
	t.Registries = dedupe(t.Registries)
	t.Revoked = dedupe(t.Revoked)
	return t
}

// SaveTrust writes the configuration, without the environment's additions —
// what the operator set is what is stored.
func SaveTrust(dir string, registries, revoked []string, allowCommunity bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(trustFile{
		Registries:     dedupe(registries),
		Revoked:        dedupe(revoked),
		AllowCommunity: allowCommunity,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(TrustPath(dir), append(data, '\n'), 0o644)
}

// StoredTrust reads only the file, for a command that is editing it.
func StoredTrust(dir string) (registries, revoked []string, allowCommunity bool) {
	var f trustFile
	if data, err := os.ReadFile(TrustPath(dir)); err == nil {
		_ = json.Unmarshal(data, &f)
	}
	return f.Registries, f.Revoked, f.AllowCommunity
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
