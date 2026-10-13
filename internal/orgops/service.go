package orgops

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// KV is the durable scratch store the office keeps its state in. The office's
// state is shared by several loops, so it cannot use a loop's own short-term
// memory, which is scoped to that loop.
type KV interface {
	KVSet(group, key, value string, ttl time.Duration) error
	KVGet(group, key string) (string, bool, error)
	KVList(group string) ([]store.KVEntry, error)
	KVDelete(group, key string) error
}

// Publisher puts an event on KARMAX's bus.
type Publisher func(kind string, payload map[string]any) error

// Service is the office. It holds no per-run state of its own beyond probe
// windows — everything that must survive a restart is in KV.
type Service struct {
	cfg     Config
	gh      *GitHub
	kv      KV
	publish Publisher
	probes  *probeSet
	now     func() time.Time
	// runCmd runs an external command (fleetctl); tests replace it.
	runCmd func(ctx context.Context, stdin string, name string, args ...string) (string, error)

	rulesOnce sync.Once
	orgRules  string
}

// New builds the office. cfg is taken as given; call WithDefaults first.
func New(cfg Config, gh *GitHub, kv KV, publish Publisher) *Service {
	return &Service{
		cfg: cfg, gh: gh, kv: kv, publish: publish,
		probes: newProbeSet(),
		now:    time.Now,
		runCmd: runCommand,
	}
}

// Config returns the effective configuration.
func (s *Service) Config() Config { return s.cfg }

// GitHub returns the client, for tools that read directly.
func (s *Service) GitHub() *GitHub { return s.gh }

// TokenFrom resolves the GitHub token from the config, then the named env var,
// then GITHUB_TOKEN / GH_TOKEN.
func TokenFrom(c GitHubConfig) string {
	if c.Token != "" {
		return c.Token
	}
	if c.TokenEnv != "" {
		if v := os.Getenv(c.TokenEnv); v != "" {
			return v
		}
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		return v
	}
	return os.Getenv("GH_TOKEN")
}

// --- state ----------------------------------------------------------------

const (
	groupPending   = "orgops:pending"
	groupAutomerge = "orgops:automerge"
	groupIncidents = "orgops:incidents"
	groupKnowledge = "orgops:knowledge"
	groupReviews   = "orgops:reviews"
)

// Pending is an action waiting for a human's "approve <code>".
type Pending struct {
	Code      string         `json:"code"`
	Action    Action         `json:"action"`
	Title     string         `json:"title"`
	Repo      string         `json:"repo,omitempty"`
	Number    int            `json:"number,omitempty"`
	CaseID    string         `json:"case_id,omitempty"`
	Channel   string         `json:"channel,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Automerge is a PR the office will merge as soon as its CI is green.
type Automerge struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Action    Action    `json:"action"`
	CaseID    string    `json:"case_id,omitempty"`
	Channel   string    `json:"channel,omitempty"`
	Incident  string    `json:"incident,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) putJSON(group, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.kv.KVSet(group, key, string(b), ttl)
}

func (s *Service) getJSON(group, key string, v any) (bool, error) {
	raw, ok, err := s.kv.KVGet(group, key)
	if err != nil || !ok {
		return ok, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func listJSON[T any](s *Service, group string) ([]T, error) {
	entries, err := s.kv.KVList(group)
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(entries))
	for _, e := range entries {
		var v T
		if json.Unmarshal([]byte(e.Value), &v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// newCode is a short code a person can type back in any chat.
func newCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// ask files an action for approval and says so in chat.
func (s *Service) ask(ctx context.Context, k loopkit.Kit, p Pending, why string) (Pending, error) {
	p.Code = newCode()
	p.CreatedAt = s.now()
	if p.Channel == "" {
		p.Channel = s.cfg.ChannelFor("approval", p.Repo)
	}
	if err := s.putJSON(groupPending, p.Code, p, 7*24*time.Hour); err != nil {
		return p, err
	}
	msg := fmt.Sprintf("🟡 *Approval needed* — %s\n%s\nReply `approve %s` to go ahead or `reject %s` to drop it.",
		p.Title, why, p.Code, p.Code)
	s.say(ctx, k, p.CaseID, p.Channel, msg)
	_ = k.Audit("propose", p.Repo, "pending", p.Title)
	return p, nil
}

// say speaks in a case's thread when there is a case, otherwise posts plainly.
// Chat failures are logged, never fatal: the office's work is the GitHub side,
// and a dead chat must not stop a merge or a fix.
func (s *Service) say(ctx context.Context, k loopkit.Kit, caseID, channel, text string) {
	if channel == "" {
		channel = s.cfg.Channels.Default
	}
	if channel == "" {
		k.Logf("orgops: no chat channel configured; would have said: %s", truncate(text, 200))
		return
	}
	var err error
	if caseID != "" {
		err = k.CaseSay(ctx, caseID, channel, text)
	} else {
		err = k.SendTo(ctx, channel, "", text)
	}
	if err != nil {
		k.Logf("orgops: chat send to %s failed: %v", channel, err)
	}
}

// mention renders owners for a chat message.
func mention(owners []string) string {
	if len(owners) == 0 {
		return ""
	}
	out := make([]string, 0, len(owners))
	for _, o := range owners {
		if strings.HasPrefix(o, "@") || strings.HasPrefix(o, "<") {
			out = append(out, o)
		} else {
			out = append(out, "@"+o)
		}
	}
	return strings.Join(out, " ")
}

// --- payload helpers --------------------------------------------------------

func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func num(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimPrefix(v, "#"))
		return n
	}
	return 0
}

// --- status -----------------------------------------------------------------

// Status is a snapshot of what the office is doing, for a chat "status" or a tool.
func (s *Service) Status() string {
	var b strings.Builder
	incidents, _ := listJSON[Incident](s, groupIncidents)
	sort.Slice(incidents, func(i, j int) bool { return incidents[i].OpenedAt.After(incidents[j].OpenedAt) })
	open := 0
	for _, in := range incidents {
		if in.State != "resolved" {
			open++
		}
	}
	fmt.Fprintf(&b, "*KARMAX org office* — %s\n", s.cfg.Org)
	fmt.Fprintf(&b, "Incidents open: %d\n", open)
	for _, in := range incidents {
		if in.State == "resolved" {
			continue
		}
		fmt.Fprintf(&b, "  • %s [%s] %s — %s", in.ID, in.Severity, in.Title, in.State)
		if in.PRURL != "" {
			fmt.Fprintf(&b, " (%s)", in.PRURL)
		}
		b.WriteString("\n")
	}
	pend, _ := listJSON[Pending](s, groupPending)
	fmt.Fprintf(&b, "Waiting on approval: %d\n", len(pend))
	for _, p := range pend {
		fmt.Fprintf(&b, "  • `%s` %s\n", p.Code, p.Title)
	}
	am, _ := listJSON[Automerge](s, groupAutomerge)
	fmt.Fprintf(&b, "Merging when CI is green: %d\n", len(am))
	for _, a := range am {
		fmt.Fprintf(&b, "  • %s#%d\n", a.Repo, a.Number)
	}
	for _, p := range s.probes.snapshot() {
		fmt.Fprintf(&b, "Probe %s: %s\n", p.name, p.summary())
	}
	return strings.TrimRight(b.String(), "\n")
}

func runCommand(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
