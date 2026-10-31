package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/integration"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/sources/gcal"
	"github.com/MelloB1989/karmax/internal/sources/gchat"
	"github.com/MelloB1989/karmax/internal/sources/gmail"
	"github.com/MelloB1989/karmax/internal/sources/googleacct"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Google Calendar, Google Chat and Gmail as event sources.
//
// Every Google account a CLI on this host holds (see internal/sources/
// googleacct) is polled: its calendar every few minutes, its Chat and its
// inbox every two. What happened becomes a sources.Event published as
// webhook.fired to the primary agent, so it is screened, remembered and acted
// on like any other event. Each account's health is an integration of its
// own, which is where a signed-out account, or a login missing a scope, says
// what to run.
//
//	KARMAX_SOURCE_GCAL=off    stop polling calendars
//	KARMAX_SOURCE_GCHAT=off   stop polling Chat
//	KARMAX_SOURCE_GMAIL=off   stop polling Gmail (settings: gmailSourceConfig)
//	KARMAX_GWS_CONFIG_DIRS    more gws config directories, one login each (':'-separated)

// switchedOff reads one source's off switch.
func switchedOff(getenv func(string) string, key string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(key))) {
	case "off", "0", "false", "no", "disabled":
		return true
	}
	return false
}

// googleSourcesOn reads the calendar and Chat off switches.
func googleSourcesOn(getenv func(string) string) (cal, chat bool) {
	return !switchedOff(getenv, "KARMAX_SOURCE_GCAL"), !switchedOff(getenv, "KARMAX_SOURCE_GCHAT")
}

// gmailSourceConfig reads the Gmail source's settings, and reports false
// when KARMAX_SOURCE_GMAIL=off.
//
//	KARMAX_SOURCE_GMAIL_EVERY            how often inboxes are polled (default 2m)
//	KARMAX_SOURCE_GMAIL_SKIP_CATEGORIES  Gmail categories never told, comma-separated
//	                                     (default promotions,social,updates,forums; "none" tells all)
//	KARMAX_SOURCE_GMAIL_URGENT           with a person writing to the operator directly, what makes it
//	                                     urgent: important, own-thread, direct (default important,own-thread;
//	                                     "none" makes nothing urgent)
//	KARMAX_SOURCE_GMAIL_AUTOMATED        words in a sender's address that mark mail nobody answers
//	                                     (default noreply,donotreply; "none" for none)
//	KARMAX_SOURCE_GMAIL_LOOKBACK         how far back an account's first look reads unread mail (default 24h)
func gmailSourceConfig(getenv func(string) string) (gmail.Config, string, bool) {
	if switchedOff(getenv, "KARMAX_SOURCE_GMAIL") {
		return gmail.Config{}, "", false
	}
	get := func(k string) string { return strings.TrimSpace(getenv("KARMAX_SOURCE_GMAIL_" + k)) }
	// list is nil when unset (the default) and empty for "none".
	list := func(k string) []string {
		v := get(k)
		switch {
		case v == "":
			return nil
		case strings.EqualFold(v, "none"):
			return []string{}
		}
		return splitCSV(v)
	}
	cfg := gmail.Config{
		SkipCategories: list("SKIP_CATEGORIES"),
		UrgentWhen:     list("URGENT"),
		Automated:      list("AUTOMATED"),
		FirstLook:      durationSetting(get("LOOKBACK")),
	}
	every := get("EVERY")
	if durationSetting(every) <= 0 {
		every = "2m"
	}
	return cfg, every, true
}

// nativeReplacements are installed workflows a compiled-in source replaces,
// keyed by the workflow's name, valued by the switch of the source that does
// its job. While the source runs the workflow stands down, so one message is
// not acted on twice; switch the source off and the workflow runs again.
var nativeReplacements = map[string]string{
	// gchat-watch read one account's Chat every four hours and acted inside
	// the loop; the Chat source reads every account and hands each message
	// to the decision core.
	"gchat-watch": "KARMAX_SOURCE_GCHAT",
}

// supersededByNative reports whether an installed workflow stands down for a
// compiled-in source.
func supersededByNative(loop string, getenv func(string) string) bool {
	sw, ok := nativeReplacements[loop]
	if !ok {
		return false
	}
	cal, chat := googleSourcesOn(getenv)
	switch sw {
	case "KARMAX_SOURCE_GCAL":
		return cal
	case "KARMAX_SOURCE_GCHAT":
		return chat
	}
	return false
}

// chatUnsupported reports whether Google Chat's API serves an address at all:
// it serves Workspace accounts, not personal Google accounts.
func chatUnsupported(email string) bool {
	_, domain, _ := strings.Cut(strings.ToLower(email), "@")
	return domain == "gmail.com" || domain == "googlemail.com"
}

type kvStore interface {
	KVGet(group, key string) (string, bool, error)
	KVSet(group, key, value string, ttl time.Duration) error
}

type googleSourceDeps struct {
	Host     *googleacct.Host
	KV       kvStore
	Publish  func(bus.Event) error
	AgentID  string
	Loc      *time.Location
	Log      *zap.Logger
	Register func(integration.Integration)
	Getenv   func(string) string
	// Sent records what the Google actions send as the operator.
	Sent tools.SentRecorder
}

// googleSources is the Google sources over one client.
type googleSources struct {
	client    *googleacct.Client
	loc       *time.Location
	cal       *gcal.Tracker
	chat      *gchat.Watcher
	mail      *gmail.Watcher
	mailEvery string
	agentID   string
	publish  func(bus.Event) error
	seen     sources.Seen
	log      *zap.Logger
	register func(integration.Integration)
	sent     tools.SentRecorder

	calMu, chatMu, mailMu sync.Mutex // one tick of each at a time

	mu         sync.Mutex
	health     map[string]map[string]sourceHealth // account -> source -> last outcome
	registered map[string]bool
	present    map[string]bool // accounts in the latest discovery
}

type sourceHealth struct {
	err  error
	note string
	at   time.Time
}

const (
	googleSeenGroup  = "google-source-seen"
	googleSeenTTL    = 30 * 24 * time.Hour
	gcalStateGroup   = "gcal-state"
	gchatStateGroup  = "gchat-state"
	gmailStateGroup  = "gmail-state"
	googleHealthName = "google_account"
)

// startGoogleSources builds the sources over this host's Google CLIs,
// registers their loops, and registers the Google actions as tools an agent
// can be granted (gcal.*, gchat.*, gmail.*, google.accounts).
func startGoogleSources(s *store.Store, b *bus.Log, toolReg *tools.Registry, reg *integration.Registry, agentID string, log *zap.Logger) *googleSources {
	g := newGoogleSources(googleSourceDeps{
		Host: googleacct.NewHost(hostpaths.Gog(), os.Getenv), KV: s, Publish: b.Publish, AgentID: agentID,
		Loc: memory.OperatorLocation(), Log: log, Register: reg.Register, Getenv: os.Getenv,
		Sent: recordSentAs(s, agentID),
	})
	for _, l := range g.loops() {
		loopkit.Register(l)
	}
	for _, t := range googleacct.Tools(g.client, g.loc, "", g.sent) {
		toolReg.Register(t)
	}
	return g
}

func newGoogleSources(d googleSourceDeps) *googleSources {
	if d.Loc == nil {
		d.Loc = time.UTC
	}
	if d.Log == nil {
		d.Log = zap.NewNop()
	}
	g := &googleSources{
		client: googleacct.NewClient(d.Host, d.Loc), loc: d.Loc, agentID: d.AgentID,
		publish: d.Publish, log: d.Log, register: d.Register, sent: d.Sent,
		seen:       sources.Seen{Group: googleSeenGroup, TTL: googleSeenTTL, Get: d.KV.KVGet, Set: d.KV.KVSet},
		health:     map[string]map[string]sourceHealth{},
		registered: map[string]bool{},
		present:    map[string]bool{},
	}
	cal, chat := googleSourcesOn(d.Getenv)
	if cal {
		g.cal = gcal.New(gcal.Config{Location: d.Loc}, g.client, jsonState[gcal.State]{kv: d.KV, group: gcalStateGroup}, g.seen)
	}
	if chat {
		g.chat = gchat.New(gchat.Config{Location: d.Loc}, g.client, jsonState[gchat.State]{kv: d.KV, group: gchatStateGroup}, g.seen)
	}
	if cfg, every, on := gmailSourceConfig(d.Getenv); on {
		cfg.Location = d.Loc
		// Mail from any of the operator's signed-in addresses is their own,
		// not a person writing to them.
		cfg.Own = func() []string { return g.client.Host().Accounts(context.Background()).Emails() }
		g.mail = gmail.New(cfg, g.client, jsonState[gmail.State]{kv: d.KV, group: gmailStateGroup}, g.seen)
		g.mailEvery = every
	}
	return g
}

// jsonState keeps one account's state as JSON in the KV store.
type jsonState[S any] struct {
	kv    kvStore
	group string
}

func (j jsonState[S]) Load(account string) (S, bool, error) {
	var st S
	v, ok, err := j.kv.KVGet(j.group, strings.ToLower(account))
	if err != nil || !ok {
		return st, false, err
	}
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		// Unreadable state is a first look again, which tells only a day's
		// worth: better than an account that never polls again.
		var zero S
		return zero, false, nil
	}
	return st, true, nil
}

func (j jsonState[S]) Save(account string, st S) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return j.kv.KVSet(j.group, strings.ToLower(account), string(b), 0)
}

// loops are the polling loops for the sources that are on.
func (g *googleSources) loops() []loopkit.Loop {
	var out []loopkit.Loop
	if g.cal != nil {
		out = append(out, loopkit.Loop{
			Name: "gcal-source",
			Description: "Every 5m reads the calendar of every Google account signed in on this host: new, moved and " +
				"cancelled events, invitations awaiting an answer, and an urgent reminder 15 minutes before a meeting.",
			Schedule: loopkit.Every("5m"),
			Run:      func(ctx context.Context, k loopkit.Kit) error { return g.run(ctx, k, "gcal") },
		})
	}
	if g.chat != nil {
		out = append(out, loopkit.Loop{
			Name: "gchat-source",
			Description: "Every 2m reads new Google Chat messages on every Workspace account signed in on this host; " +
				"a DM or an @mention of the operator is urgent.",
			Schedule: loopkit.Every("2m"),
			Run:      func(ctx context.Context, k loopkit.Kit) error { return g.run(ctx, k, "gchat") },
		})
	}
	if g.mail != nil {
		out = append(out, loopkit.Loop{
			Name: "gmail-source",
			Description: "Every " + g.mailEvery + " reads new inbox mail on every Google account signed in on this host with " +
				"Gmail, leaving out what the operator sent and what Gmail and its sender mark as bulk; a person writing to " +
				"the operator directly about something Gmail marks important, or answering their thread, is urgent.",
			Schedule: loopkit.Every(g.mailEvery),
			Run:      func(ctx context.Context, k loopkit.Kit) error { return g.run(ctx, k, gmail.Name) },
		})
	}
	return out
}

func (g *googleSources) run(ctx context.Context, k loopkit.Kit, source string) error {
	err := g.tick(ctx, source)
	if err != nil {
		k.Logf("%s: %v", source, err)
	}
	return err
}

// tick polls every account for one source. An account that cannot be reached
// is reported in its health (and told once), not as the loop failing: one
// signed-out account must not look like the whole source being down, nor
// stop the others.
func (g *googleSources) tick(ctx context.Context, source string) error {
	mu := &g.calMu
	service := "calendar"
	switch source {
	case "gchat":
		mu, service = &g.chatMu, "chat"
	case gmail.Name:
		mu, service = &g.mailMu, googleacct.GmailRead
	}
	if !mu.TryLock() {
		return nil // the previous tick is still working
	}
	defer mu.Unlock()

	d := g.client.Host().Accounts(ctx)
	g.observe(d)
	var failed []string
	for _, a := range d.Accounts {
		if note := skipReason(a, service); note != "" {
			g.note(a.Email, source, nil, note)
			continue
		}
		var err error
		switch source {
		case "gcal":
			var res gcal.Result
			res, err = g.cal.Tick(ctx, a.Email, g.emit)
			if err == nil && res.First {
				g.log.Info("gcal: first look at an account", zap.String("account", a.Email), zap.Int("events", res.Known), zap.Int("published", res.Published))
			}
		case "gchat":
			var res gchat.Result
			res, err = g.chat.Tick(ctx, a.Email, g.emit)
			if err == nil && len(res.Failed) > 0 {
				g.note(a.Email, source, nil, fmt.Sprintf("%d space(s) could not be read this time", len(res.Failed)))
			}
			if err == nil && res.First {
				g.log.Info("gchat: first look at an account", zap.String("account", a.Email), zap.Int("spaces", res.Spaces), zap.Int("published", res.Published))
			}
		case gmail.Name:
			var res gmail.Result
			res, err = g.mail.Tick(ctx, a.Email, g.emit)
			if err == nil && res.First {
				g.log.Info("gmail: first look at an account", zap.String("account", a.Email), zap.Int("listed", res.Listed),
					zap.Int("published", res.Published), zap.Any("skipped", res.Skipped))
			}
			if err == nil && len(res.Failed) > 0 {
				g.log.Warn("gmail: messages could not be read; tried again next tick", zap.String("account", a.Email), zap.Int("messages", len(res.Failed)))
			}
		}
		g.outcome(a.Email, source, err)
		if err == nil && source == gmail.Name {
			// Reading works; what the login cannot do yet is a health problem
			// of its own, with the command that fixes it.
			if gap := gmailGap(a); gap != nil {
				g.note(a.Email, source, gap, "")
			}
		}
		if err != nil && !isUnreachable(err) {
			failed = append(failed, a.Email+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

// skipReason says why an account is not polled for a service, or "".
func skipReason(a googleacct.Account, service string) string {
	if service == "chat" && chatUnsupported(a.Email) {
		return "Google Chat's API serves Workspace accounts only, so Chat is not read for a personal account"
	}
	for _, l := range a.Logins {
		if l.Problem != "" || l.Can(service) {
			return "" // tried, so a problem is reported as one
		}
	}
	return fmt.Sprintf("no login for this account was granted %s; to add it, run `%s`", service, a.FixFor(service))
}

// gmailGap says what KARMAX cannot do with an account whose mail it reads —
// send as it, file its mail, read whole emails — and the command that grants
// it; nil when nothing is missing. Acting freely on mail needs all three.
func gmailGap(a googleacct.Account) error {
	var cannot []string
	need := ""
	for _, p := range []struct{ power, what string }{
		{googleacct.GmailSend, "send email as it"},
		{googleacct.GmailModify, "archive, label or mark its mail read"},
		{googleacct.GmailBody, "read whole emails"},
	} {
		if a.Lacks(p.power) {
			cannot = append(cannot, p.what)
			if need == "" {
				need = p.power
			}
		}
	}
	if len(cannot) == 0 {
		return nil
	}
	return fmt.Errorf("Gmail is read, but KARMAX cannot %s: its login was not granted the scope. To fix, run on the KARMAX host: %s",
		strings.Join(cannot, ", or "), a.FixFor(need))
}

func isUnreachable(err error) bool {
	var ae *googleacct.AuthError
	var ue *googleacct.UnknownAccountError
	return errors.As(err, &ae) || errors.As(err, &ue)
}

// emit publishes one event to the primary agent.
func (g *googleSources) emit(e sources.Event) error {
	return g.publish(e.BusEvent(g.agentID))
}

// observe registers a health entry for every account seen, and remembers
// which are present now.
func (g *googleSources) observe(d googleacct.Discovery) {
	g.mu.Lock()
	present := map[string]bool{}
	var fresh []string
	for _, a := range d.Accounts {
		present[a.Email] = true
		if !g.registered[a.Email] {
			g.registered[a.Email] = true
			fresh = append(fresh, a.Email)
		}
	}
	g.present = present
	g.mu.Unlock()
	if g.register == nil {
		return
	}
	for _, email := range fresh {
		g.register(&googleAccountHealth{g: g, email: email})
	}
}

func (g *googleSources) note(account, source string, err error, note string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.health[account] == nil {
		g.health[account] = map[string]sourceHealth{}
	}
	g.health[account][source] = sourceHealth{err: err, note: note, at: time.Now()}
}

// outcome records a tick's result, and tells the operator once when an
// account stops being reachable.
func (g *googleSources) outcome(account, source string, err error) {
	g.mu.Lock()
	was := g.health[account][source].err
	g.mu.Unlock()
	g.note(account, source, err, "")

	if !isUnreachable(err) {
		if was != nil && isUnreachable(was) && err == nil {
			g.log.Info("google account reachable again", zap.String("account", account), zap.String("source", source))
		}
		return
	}
	if was == nil || !isUnreachable(was) {
		g.log.Warn("google account cannot be reached", zap.String("account", account), zap.String("source", source), zap.Error(err))
	}
	// Keyed by the day, so a restart during a long outage tells at most once
	// more, and both sources together tell once.
	e := sources.Event{
		Source: source, Account: account, Type: "account.signed_out",
		Key:     "google|" + account + "|signed_out|" + time.Now().In(g.loc).Format("2006-01-02"),
		Title:   fmt.Sprintf("Google account %s is signed out on the KARMAX host: its calendar, chat and mail are not being read", account),
		Summary: err.Error(),
		Urgent:  true,
		At:      time.Now(),
	}
	if first, serr := g.seen.First(e.Key); serr == nil && first {
		if perr := g.emit(e); perr != nil {
			g.log.Warn("could not publish a signed-out account", zap.Error(perr))
		}
	}
}

// healthOf is an account's state across both sources.
func (g *googleSources) healthOf(account string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.present[account] {
		return fmt.Errorf("%s is no longer signed in on this host; run `gog auth add %s --services calendar,chat,gmail` (or `gws auth login`)", account, account)
	}
	var problems []string
	names := make([]string, 0, len(g.health[account]))
	for src := range g.health[account] {
		names = append(names, src)
	}
	sort.Strings(names)
	for _, src := range names {
		if h := g.health[account][src]; h.err != nil {
			problems = append(problems, src+": "+h.err.Error())
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// googleAccountHealth is one account as an integration, so it appears with
// the others in integration health.
type googleAccountHealth struct {
	g     *googleSources
	email string
}

func (h *googleAccountHealth) Manifest() integration.Manifest {
	return integration.Manifest{
		ID:   googleHealthName + ":" + h.email,
		Name: "Google account " + h.email + " (Calendar, Chat and Gmail events)",
		Description: "Polled for calendar, Chat and Gmail events through the Google CLIs signed in on this host. " +
			"Unhealthy means its events are not being read; the error says what to run.",
		Kind:    integration.KindConnector,
		Account: h.email,
	}
}

func (h *googleAccountHealth) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthCLI}
}

func (h *googleAccountHealth) Health(context.Context, connectorkit.Credentials) error {
	return h.g.healthOf(h.email)
}

// googleToolsForEvent lends the Google actions to a turn: bound to the
// account for that account's own calendar, Chat or Gmail event, and unbound for the
// operator's own turns, where they say which account.
func (rt *KarmaxRuntime) googleToolsForEvent(evt bus.Event) []tools.Tool {
	g := rt.google
	if g == nil {
		return nil
	}
	if evt.Kind == bus.EventWebhookFired && evt.Payload != nil {
		src, _ := evt.Payload["source"].(string)
		if src == "gcal" || src == "gchat" || src == gmail.Name {
			account, _ := evt.Payload["account"].(string)
			return googleacct.Tools(g.client, g.loc, account, g.sent)
		}
		return nil
	}
	if isOperatorTurn(evt, rt.isOperatorChat) {
		return googleacct.Tools(g.client, g.loc, "", g.sent)
	}
	return nil
}
