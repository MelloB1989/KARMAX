// Package archive keeps every session the fleet ends: copied, verified by
// hash, and only then may anything stop it. Nothing archived is ever lost —
// a session can be restored onto its agent at any time.
//
// Layout, under the archive root:
//
//	<agent>/<date>-<sid8>[-n]/
//	  manifest.json
//	  transcript.jsonl.zst
//	  subagents/…​.jsonl.zst
//	  worktrees.json
package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/MelloB1989/karmax/fleet/internal/agent"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
)

// Source is the slice of an agent's container that archiving needs.
type Source interface {
	PullTranscript(ctx context.Context, sid string) (*agent.Transcript, error)
	HashTranscript(ctx context.Context, sid string) (string, error)
	Worktrees(ctx context.Context) ([]agent.Worktree, error)
}

// Sink is where a restore puts a transcript back.
type Sink interface {
	PushTranscript(ctx context.Context, sid string, tr *agent.Transcript) error
}

// ErrVerify is a copy that does not match the container's transcript.
var ErrVerify = errors.New("archive verification failed")

// Info is what the caller knows about the session being archived.
type Info struct {
	Agent, Host, Account string
	SessionID, Name      string
	Task, Reason         string
	At                   time.Time
}

// Manifest is the archive's record of one session.
type Manifest struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	Host       string    `json:"host"`
	Account    string    `json:"account"`
	SessionID  string    `json:"session_id"`
	Name       string    `json:"name,omitempty"`
	Task       string    `json:"task,omitempty"`
	Reason     string    `json:"reason"`
	ArchivedAt time.Time `json:"archived_at"`

	Started       time.Time     `json:"started"`
	Ended         time.Time     `json:"ended"`
	Turns         int           `json:"turns"`
	PeerMessages  int           `json:"peer_messages"`
	Usage         observe.Usage `json:"usage"`
	CostUSD       float64       `json:"cost_usd"` // list-price estimate
	Models        []string      `json:"models,omitempty"`
	Cwd           string        `json:"cwd"`
	LastAssistant string        `json:"last_assistant"`

	Project   string            `json:"project"`
	SHA256    string            `json:"sha256"`
	Bytes     int64             `json:"bytes"`
	Subagents map[string]string `json:"subagents,omitempty"` // path → sha256
	Worktrees []agent.Worktree  `json:"worktrees"`
}

// KeptWorktrees are the worktrees the archive does not allow removing.
func (m *Manifest) KeptWorktrees() []agent.Worktree {
	var out []agent.Worktree
	for _, w := range m.Worktrees {
		if !w.Safe() {
			out = append(out, w)
		}
	}
	return out
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Archive copies a session out of its container and verifies the copy. It
// stops nothing: the caller stops the session only after this returns nil.
func Archive(ctx context.Context, src Source, root string, in Info) (*Manifest, error) {
	tr, err := src.PullTranscript(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	want, err := src.HashTranscript(ctx, in.SessionID)
	if err != nil {
		return nil, err
	}
	if got := digest(tr.Main); got != want {
		return nil, fmt.Errorf("%w: %s copied as %s, container has %s", ErrVerify, in.SessionID, got[:12], want[:min(12, len(want))])
	}
	wts, err := src.Worktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("worktrees: %w", err)
	}
	sum, _ := observe.Summarize(bytes.NewReader(tr.Main))

	m := &Manifest{
		Agent: in.Agent, Host: in.Host, Account: in.Account, SessionID: in.SessionID, Name: in.Name,
		Task: in.Task, Reason: in.Reason, ArchivedAt: in.At,
		Started: sum.Started, Ended: sum.Ended, Turns: sum.Turns, PeerMessages: sum.PeerMessages,
		Usage: sum.Usage, CostUSD: sum.CostUSD, Models: sum.Models, Cwd: sum.Cwd, LastAssistant: sum.LastAssistant,
		Project: tr.Project, SHA256: want, Bytes: int64(len(tr.Main)), Worktrees: wts,
	}
	if m.Worktrees == nil {
		m.Worktrees = []agent.Worktree{}
	}
	if len(tr.Sub) > 0 {
		m.Subagents = map[string]string{}
	}

	agentDir := filepath.Join(root, in.Agent)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return nil, err
	}
	base := in.At.UTC().Format("2006-01-02") + "-" + in.SessionID[:min(8, len(in.SessionID))]
	name := base
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(agentDir, name)); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	m.ID = in.Agent + "/" + name

	tmp, err := os.MkdirTemp(agentDir, ".partial-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp) // a no-op once renamed into place
	if err := writeZst(filepath.Join(tmp, "transcript.jsonl.zst"), tr.Main); err != nil {
		return nil, err
	}
	for rel, body := range tr.Sub {
		if !fs.ValidPath(rel) {
			return nil, fmt.Errorf("subagent transcript path %q", rel)
		}
		m.Subagents[rel] = digest(body)
		if err := writeZst(filepath.Join(tmp, rel+".zst"), body); err != nil {
			return nil, err
		}
	}
	if err := writeJSON(filepath.Join(tmp, "worktrees.json"), m.Worktrees); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(tmp, "manifest.json"), m); err != nil {
		return nil, err
	}
	// Verify what is on disk, not what was in memory.
	if err := verify(tmp, m); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, filepath.Join(agentDir, name)); err != nil {
		return nil, err
	}
	return m, nil
}

func verify(dir string, m *Manifest) error {
	main, err := readZst(filepath.Join(dir, "transcript.jsonl.zst"))
	if err != nil {
		return err
	}
	if digest(main) != m.SHA256 {
		return fmt.Errorf("%w: transcript on disk", ErrVerify)
	}
	for rel, want := range m.Subagents {
		b, err := readZst(filepath.Join(dir, rel+".zst"))
		if err != nil {
			return err
		}
		if digest(b) != want {
			return fmt.Errorf("%w: %s on disk", ErrVerify, rel)
		}
	}
	return nil
}

func writeZst(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return err
	}
	defer enc.Close()
	return writeSync(path, enc.EncodeAll(body, nil))
}

func readZst(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(b, nil)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeSync(path, append(b, '\n'))
}

func writeSync(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func dirOf(root, id string) (string, error) {
	clean := filepath.Clean(id)
	if !fs.ValidPath(clean) || strings.Count(clean, "/") != 1 {
		return "", fmt.Errorf("archive id %q: want <agent>/<name>", id)
	}
	return filepath.Join(root, clean), nil
}

// Load reads one archive's manifest.
func Load(root, id string) (*Manifest, error) {
	dir, err := dirOf(root, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ReadTranscript reads an archived transcript back, verified.
func ReadTranscript(root, id string) (*agent.Transcript, error) {
	m, err := Load(root, id)
	if err != nil {
		return nil, err
	}
	dir, _ := dirOf(root, id)
	if err := verify(dir, m); err != nil {
		return nil, err
	}
	main, _ := readZst(filepath.Join(dir, "transcript.jsonl.zst"))
	tr := &agent.Transcript{Project: m.Project, Main: main, Sub: map[string][]byte{}}
	for rel := range m.Subagents {
		tr.Sub[rel], _ = readZst(filepath.Join(dir, rel+".zst"))
	}
	return tr, nil
}

// Restore puts an archived transcript back into an agent's container. The
// caller then points the agent's next session at it.
func Restore(ctx context.Context, dst Sink, root, id string) (*Manifest, error) {
	m, err := Load(root, id)
	if err != nil {
		return nil, err
	}
	tr, err := ReadTranscript(root, id)
	if err != nil {
		return nil, err
	}
	return m, dst.PushTranscript(ctx, m.SessionID, tr)
}

// List is every archive since a time (zero for all), newest first.
func List(root string, since time.Time) ([]*Manifest, error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", "*", "manifest.json"))
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, p := range paths {
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		if strings.HasPrefix(filepath.Base(rel), ".partial-") {
			continue
		}
		m, err := Load(root, filepath.ToSlash(rel))
		if err != nil {
			continue
		}
		if m.ArchivedAt.Before(since) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchivedAt.After(out[j].ArchivedAt) })
	return out, nil
}

// Prune removes archives made before a time. Archives are kept forever
// unless you run this.
func Prune(root string, before time.Time) (int, error) {
	all, err := List(root, time.Time{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range all {
		if !m.ArchivedAt.Before(before) {
			continue
		}
		dir, err := dirOf(root, m.ID)
		if err != nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
