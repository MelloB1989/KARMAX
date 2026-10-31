package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/chatmemory"
	"github.com/MelloB1989/karmax/internal/identity"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/sources/gchat"
	"github.com/MelloB1989/karmax/internal/sources/googleacct"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Chat services → long-term memory.
//
// Each chat service the operator talks on has a loop of its own that reads
// every conversation's new messages, everyone's and the operator's own, and
// keeps memory in step with them (see internal/chatmemory). The loops share
// one syncer; what differs is the Source it reads and the settings below,
// all named after the loop:
//
//	KARMAX_<LOOP>             off turns the loop off (KARMAX_WA_MEMORY, KARMAX_GCHAT_MEMORY)
//	KARMAX_<LOOP>_SINCE       where a chat never read before starts (YYYY-MM-DD); by default a
//	                          week before KARMAX first looked at the chat's account
//	KARMAX_<LOOP>_DENY_FILE   chats never read, one id or name per line (default ~/.karmax/<loop>-deny.txt)
//	KARMAX_<LOOP>_BATCHES     model calls one tick may make (default 12)
//
// Cursors are kept in the KV group "<loop>-cursor", first looks in
// "<loop>-first-look".

// chatMemory is one chat service read into memory by its own loop.
type chatMemory struct {
	// Loop names the loop and, from it, its settings, cursors and deny file.
	Loop string
	// Service is the chat service as a person names it.
	Service string
	// Tag files its facts under the service as well as their subject.
	Tag string
	// Covers is the event source whose conversations the loop reads, if
	// any. While the loop is on, that source's event turns leave filing what
	// a message says to it (sources.MemorySync), so memory holds each
	// conversation once.
	Covers      string
	Every       string
	Description string
}

var (
	waMemory = chatMemory{Loop: "wa-memory", Service: "WhatsApp", Tag: "whatsapp", Every: "20m",
		Description: "Reads every WhatsApp chat's new messages and keeps long-term memory in step with them: durable facts added under their person or project, outdated ones corrected, finished ones retired."}
	gchatMemory = chatMemory{Loop: "gchat-memory", Service: "Google Chat", Tag: "gchat", Covers: gchat.SourceName, Every: "20m",
		Description: "Reads every Google Chat space and direct message of every Workspace account signed in on this host — the operator's own messages included — and keeps long-term memory in step with them: durable facts added, outdated ones corrected, finished ones retired."}
)

// env names one of the loop's settings: KARMAX_WA_MEMORY, KARMAX_WA_MEMORY_SINCE.
func (m chatMemory) env(suffix string) string {
	k := "KARMAX_" + strings.ToUpper(strings.ReplaceAll(m.Loop, "-", "_"))
	if suffix != "" {
		k += "_" + suffix
	}
	return k
}

// config reads the loop's settings, and reports false when it is switched off.
func (m chatMemory) config(getenv func(string) string, karmaxHome string) (chatmemory.Config, bool) {
	switch strings.ToLower(strings.TrimSpace(getenv(m.env("")))) {
	case "off", "0", "false", "no", "disabled":
		return chatmemory.Config{}, false
	}
	cfg := chatmemory.Config{Name: m.Loop, Service: m.Service, Tag: m.Tag,
		Lookback: 7 * 24 * time.Hour, Location: memory.OperatorLocation()}
	if d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(getenv(m.env("SINCE"))), cfg.Location); err == nil {
		cfg.Since = d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(getenv(m.env("BATCHES")))); err == nil && n > 0 {
		cfg.BatchesPerTick = n
	}
	cfg.Operator = identity.NameFromEnv(getenv)
	deny := strings.TrimSpace(getenv(m.env("DENY_FILE")))
	if deny == "" {
		deny = filepath.Join(karmaxHome, m.Loop+"-deny.txt")
	}
	if b, err := os.ReadFile(deny); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			l, _, _ = strings.Cut(l, "#")
			if l = strings.TrimSpace(l); l != "" {
				cfg.Deny = append(cfg.Deny, l)
			}
		}
	}
	return cfg, true
}

// cursors keeps the loop's per-chat cursors in the store.
func (m chatMemory) cursors(s *store.Store) chatmemory.Cursors {
	return kvCursors{s: s, group: m.Loop + "-cursor"}
}

// firstLook records when the loop first looked at each account, once.
func (m chatMemory) firstLook(s *store.Store, now func() time.Time) func(account string) (time.Time, error) {
	fl := sources.FirstLook{Group: m.Loop + "-first-look", Get: s.KVGet, Set: s.KVSet}
	return func(account string) (time.Time, error) {
		if account == "" {
			account = m.Loop // a service of one account
		}
		at, _, err := fl.Since(strings.ToLower(account), now())
		return at, err
	}
}

// chatMemoryDeps is what every chat-memory loop is built from.
type chatMemoryDeps struct {
	Store  *store.Store
	Memory chatmemory.Memory
	// Extract runs one extraction on the harness, in a session keyed by key.
	Extract func(ctx context.Context, key, prompt string) (string, error)
	Getenv  func(string) string
	// Home is KARMAX's own directory, where the deny files are.
	Home string
	Log  *zap.Logger
	// Covered is told which event sources the running loops read.
	Covered *sources.MemorySync
	// Register registers a loop; nil is loopkit.Register.
	Register func(loopkit.Loop)
}

// start registers the loop when it is on, reading src. tune sets what only
// the caller knows (the operator's accounts on that service); sent says
// whether KARMAX itself sent a message on the operator's account.
func (m chatMemory) start(d chatMemoryDeps, src chatmemory.Source, tune func(*chatmemory.Config),
	sent func(c chatmemory.Chat, text string, at time.Time) bool) bool {
	cfg, on := m.config(d.Getenv, d.Home)
	if !on {
		d.Log.Info(m.Loop+" is off", zap.String("switch", m.env("")))
		return false
	}
	if cfg.Since.IsZero() {
		cfg.FirstLook = m.firstLook(d.Store, time.Now)
	}
	if tune != nil {
		tune(&cfg)
	}
	syncer := chatmemory.New(cfg, src, d.Memory, m.cursors(d.Store), sent,
		func(ctx context.Context, prompt string) (string, error) { return d.Extract(ctx, m.Loop, prompt) }, d.Log)
	register := d.Register
	if register == nil {
		register = loopkit.Register
	}
	register(loopkit.Loop{
		Name:        m.Loop,
		Description: m.Description,
		Schedule:    loopkit.Every(m.Every),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			res, err := syncer.Tick(ctx)
			if res.Batches > 0 || len(res.Stalled) > 0 || err != nil {
				k.Logf("%s: %d batches: added %d, corrected %d, forgot %d, refused %d; more to read: %v; stalled chats: %d",
					m.Loop, res.Batches, res.Added, res.Updated, res.Forgotten, res.Refused, res.More, len(res.Stalled))
			}
			return err
		},
	})
	if m.Covers != "" {
		d.Covered.Cover(m.Covers)
	}
	return true
}

// memorySync is the runtime's one record of which event sources the running
// memory loops read; screenEvent marks their events by it.
type memorySync = sources.MemorySync

func newMemorySync() *memorySync { return &memorySync{} }

// gchatSentAs says whether KARMAX itself sent a Chat message: it speaks
// through the operator's own account, which the sent-as ledger tells apart.
func gchatSentAs(s *store.Store) func(c chatmemory.Chat, text string, at time.Time) bool {
	return func(c chatmemory.Chat, text string, at time.Time) bool {
		return s.SentAsOperator(gchat.SourceName, c.Account, text, at)
	}
}

// chatAccounts are the accounts whose Google Chat is read: the ones the Chat
// event source reads (see skipReason).
func chatAccounts(d googleacct.Discovery) []string {
	var out []string
	for _, a := range d.Accounts {
		if skipReason(a, "chat") == "" {
			out = append(out, a.Email)
		}
	}
	return out
}

// chatMemorySource reads Google Chat for memory through the same client, and
// so the same logins, as the Chat event source.
func (g *googleSources) chatMemorySource(log *zap.Logger) chatmemory.Source {
	return chatmemory.NewGChat(g.client, func(ctx context.Context) []string {
		return chatAccounts(g.client.Host().Accounts(ctx))
	}, log)
}

// kvCursors keeps a syncer's per-chat cursors in one KV group.
type kvCursors struct {
	s     *store.Store
	group string
}

func (c kvCursors) Get(chat string) (chatmemory.Cursor, bool, error) {
	v, ok, err := c.s.KVGet(c.group, chat)
	if err != nil || !ok {
		return chatmemory.Cursor{}, false, err
	}
	at, id, _ := strings.Cut(v, "|")
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return chatmemory.Cursor{}, false, nil // unreadable: read the chat from its start again
	}
	return chatmemory.Cursor{At: t, ID: id}, true, nil
}

func (c kvCursors) Set(chat string, cur chatmemory.Cursor) error {
	if c.group == "" {
		return fmt.Errorf("a cursor with no group")
	}
	return c.s.KVSet(c.group, chat, cur.At.Format(time.RFC3339Nano)+"|"+cur.ID, 0)
}

// harnessExtract runs one extraction on the "extract" kind, in a session of
// its own: each batch is one chat, and a session carried over would set one
// chat's people beside the next one's.
func (rt *KarmaxRuntime) harnessExtract(ctx context.Context, key, prompt string) (string, error) {
	if rt == nil || rt.harness == nil {
		return "", errors.New("the harness is off")
	}
	defer rt.harness.Close(key)
	turn, err := rt.harness.Send(ctx, key, "extract", prompt)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(turn.Text) == "" {
		return "", errors.New("the harness returned an empty turn")
	}
	return turn.Text, nil
}
