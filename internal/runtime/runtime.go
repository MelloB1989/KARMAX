package runtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/chatmemory"
	"github.com/MelloB1989/karmax/internal/clock"
	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/MelloB1989/karmax/internal/comms/slack"
	"github.com/MelloB1989/karmax/internal/comms/webhookchat"
	"github.com/MelloB1989/karmax/internal/comms/whatsapp"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/connectors"
	githubconn "github.com/MelloB1989/karmax/internal/connectors/github"
	googleconn "github.com/MelloB1989/karmax/internal/connectors/google"
	googleworkspaceconn "github.com/MelloB1989/karmax/internal/connectors/googleworkspace"
	linkedinconn "github.com/MelloB1989/karmax/internal/connectors/linkedin"
	lyznconn "github.com/MelloB1989/karmax/internal/connectors/lyzn"
	slackconn "github.com/MelloB1989/karmax/internal/connectors/slack"
	xconn "github.com/MelloB1989/karmax/internal/connectors/x"
	"github.com/MelloB1989/karmax/internal/harness"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/identity"
	"github.com/MelloB1989/karmax/internal/integrations"
	"github.com/MelloB1989/karmax/internal/mcp"
	"github.com/MelloB1989/karmax/internal/memmerge"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/policy"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/reflex"
	"github.com/MelloB1989/karmax/internal/review"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/internal/tracker"
	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/internal/wasmloop"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	wacli "github.com/MelloB1989/wacli/tools"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type KarmaxRuntime struct {
	cfg       *config.KarmaxConfig
	log       *zap.Logger
	store     *store.Store
	bus       *bus.Log
	memory    *memory.ManagerFactory
	tools     *tools.Registry
	mcpBridge *mcp.MCPBridge
	agents    *agent.Registry
	scheduler *scheduler.Scheduler
	webhooks  *webhook.WebhookServer
	comms     *comms.Manager
	api       *api.Server
	// google is the Google Calendar and Chat sources (googlesource.go).
	google *googleSources

	// who is the one answer to "who is speaking": the operator's name and
	// accounts and the linked account KARMAX sends from. See internal/identity.
	who *identity.Directory
	// labelSpeaker says who wrote each message of a WhatsApp thread.
	labelSpeaker builtin.SpeakerLabeller

	// apiBrowserToken and apiBaseURL let a harness this instance spawns (for a
	// loop or a LYZN task — see loophost.go's HarnessWith) call back into this
	// engine's own local API for `karmax browser ...` without ever holding
	// the operator's full API token. apiBrowserToken is scoped server-side to
	// the browser tool only (internal/api's browserScopedTools); both are ""
	// when the API server is disabled (cfg.API.Enabled == false).
	apiBrowserToken string
	apiBaseURL      string

	// broker decides what each loop, peer and connector may do.
	broker *broker.Broker

	// connectors are the integrations the operator has enabled.
	connectors *connectors.Host

	// clock fires durable timers into the log — "continue on Thursday".
	clock *clock.Clock

	// harness runs coding harnesses as long-lived conversations. Nil when the
	// feature is off, and every caller treats nil as "use the API path".
	harness        *harness.Supervisor
	harnessBreaker *harness.Breaker
	// browserMCPCache holds the last --mcp-config probe, refreshed on the
	// browser's own start/stop signal rather than once per turn. Nil exactly
	// when harness is nil.
	browserMCPCache *browserMCPCache

	// routedKinds are the event kinds that reach agent inboxes, computed at
	// construction and consumed once the runtime starts.
	routedKinds []bus.EventKind

	// recipeLoops are the YAML recipes currently loaded from disk.
	recipeMu sync.RWMutex
	// recipeCtx is the context startRecipes runs the watcher under, kept so an
	// enable or disable from the API can re-apply the recipes on the spot.
	recipeCtx   context.Context
	recipeLoops map[string]*recipes.Recipe

	// wasmRunners hold the compiled signed loops, released on shutdown.
	wasmRunners []*wasmloop.Runner

	// wasmByName resolves a workflow so the tools it provides can be lent to
	// the agent for one turn.
	providedMu sync.RWMutex
	wasmByName map[string]*wasmloop.Runner

	// attributions link work a workflow started to the workflow, so a turn
	// arriving later — a delegation completing — can still be traced back.
	attributions *attributions

	// loopkit runtime state (set by startLoopkitLoops)
	loopkitLoops     map[string]loopkit.Loop
	loopWebhooks     map[string]string // webhook route -> loop name
	loopDefaultAgent string

	// waChannel is kept so voice can teach it to answer incoming calls once the
	// brain is up — the channel is built long before the brain is.
	waChannel *whatsapp.WhatsAppChannel

	taskHooks taskHooks
	// messageOperator delivers text to the operator's own chat, for things
	// that finish after whoever asked for them has gone — a call that ended
	// before the task it handed off came back. Nil when there is no channel.
	messageOperator func(text string) error

	// voice holds the voice integrations this instance can place calls through.
	voice *voice.Registry

	// pending holds event triggers that arrived while a loop was running, so a
	// busy loop delays a message instead of destroying it.
	pending *pendingTriggers
	// runLogs holds, per loop, the log lines of its run in flight; see looplog.go.
	runLogs sync.Map

	// reflex is System One: the cheap probability model every event is
	// screened by before anything expensive looks at it. Nil-safe — an
	// unconfigured reflex passes every event through untouched.
	reflex *reflex.Evaluator

	// policy is how KARMAX acts for the operator and when it reaches them.
	policy policy.Policy

	// startedAt is when this process came up. A loop that has not succeeded
	// yet is judged against this rather than against the epoch, so a restart
	// does not report every scheduled loop as dark.
	startedAt time.Time

	// sourceTools are lent to the turn handling an event from that source
	// (internal/sources): the Slack source's send-as-the-operator, for one.
	sourceToolsMu sync.Mutex
	sourceTools   map[string][]tools.Tool
}

func New(cfg *config.KarmaxConfig, log *zap.Logger) (*KarmaxRuntime, error) {
	dataDir := cfg.Karmax.DataDir
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(filepath.Join(dataDir, "memory"), 0755)
	os.MkdirAll(filepath.Join(dataDir, "db"), 0755)

	s, err := store.New(cfg.DatabaseDSN(), log)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	// One workspace per daemon: multi-tenant packaging runs a separate KARMAX
	// per person, so the partition exists in the schema rather than in config.
	b := bus.NewLog(s, store.DefaultWorkspace, log)
	clk := clock.New(s, b, store.DefaultWorkspace, log)
	brk := broker.New(s, log)

	// Connectors are registered here and do nothing until the operator supplies
	// credentials and enables them.
	connHost := connectors.NewHost(s, b, brk, log)
	// Before any Register call: what this install manages is decided once, in
	// karmax.yaml, and everything below is filtered through it.
	connHost.Manage(cfg.Connectors)
	// One connector per GitHub account. The primary has no suffix, so a
	// single-account install is unchanged; additional accounts are named and
	// their tools qualified (github.issues@work), which is what lets the agent
	// act as the right identity rather than whichever token happened to load.
	connHost.Register(githubconn.New(""))
	for _, account := range splitCSV(os.Getenv("KARMAX_GITHUB_ACCOUNTS")) {
		connHost.Register(githubconn.New(account))
	}
	// LYZN hears a promise and this machine keeps it. Paired with a
	// six-character code rather than a key, because the product it talks to
	// has no login for a laptop to hold — the code is redeemed once, for a
	// token bound to this machine, and revoked from the phone that minted it.
	connHost.Register(lyznconn.New())
	// Slack is already wired as a COMMS CHANNEL — the thing that receives
	// mentions and replies in threads. That is a different subsystem, which is
	// why Slack never appeared on the Connectors page despite obviously being
	// connected. Registering it here does not replace the channel; it makes the
	// workspace visible where an operator looks for it, gives it a health check
	// that catches a missing app-level token, and adds the few tools that are
	// about the workspace rather than about a conversation.
	connHost.Register(slackconn.New())
	// Google is PER EMPLOYEE: the org registers one OAuth app and each person
	// authorises their own account against it. The host resolves whose token to
	// use from who is being acted for.
	connHost.Register(googleconn.New())

	// Renew an employee's Google token before it is used, and persist the new
	// one. Without this every connection would work for an hour and then start
	// failing with a 401 that reads like a revoked grant.
	connHost.SetUserRefresher(func(ctx context.Context, connector string, uc store.UserCredential) (store.UserCredential, error) {
		if connector != "google" || !uc.Expired() {
			return uc, nil
		}
		rec, err := s.Credential(connector)
		if err != nil || rec == nil {
			return uc, fmt.Errorf("the %s OAuth app is not configured", connector)
		}
		ts, err := googleconn.Refresh(ctx,
			connectorkit.Credentials{Config: rec.Config}, uc.RefreshToken)
		if err != nil {
			return uc, err
		}
		uc.AccessToken, uc.ExpiresAt = ts.AccessToken, &ts.Expiry
		// Google usually omits the refresh token on renewal; SaveUserCredential
		// keeps the stored one when it arrives empty.
		uc.RefreshToken = ts.RefreshToken
		if err := s.SaveUserCredential(uc); err != nil {
			log.Warn("could not persist a refreshed sign-in",
				zap.String("connector", connector), zap.String("member", uc.Member), zap.Error(err))
		}
		return uc, nil
	})
	// The public accounts. These are the only integrations that can make
	// something visible to strangers with nobody having read it, so both are
	// handed the list of names a post may not contain — built from this
	// operator's own contacts and memory, and consulted at the moment of
	// posting rather than trusted to whatever wrote the draft.
	forbidden := newForbiddenNames(s, log)
	socialLimit := newSocialLimiter(s)
	// Unconditional: their tools exist before either account is connected, so a
	// dry run can show what KARMAX would say without an X or LinkedIn account
	// existing yet. Publishing for real still needs real credentials.
	connHost.RegisterUnconditional(xconn.New(forbidden.Guard, socialLimit))
	connHost.RegisterUnconditional(linkedinconn.New(forbidden.Guard, socialLimit))
	// Google through the gog CLI, as a connector so it obeys the connectors:
	// allowlist and appears in `karmax login` like everything else.
	// Unconditional for the same reason its Auth is AuthCLI: there is no
	// credential for KARMAX to hold, so waiting for one would mean the tools
	// never appearing at all.
	gogRunner := &builtin.GogTool{Path: hostpaths.Gog(), DefaultAccount: os.Getenv("KARMAX_GOOGLE_ACCOUNT")}
	connHost.RegisterUnconditional(googleworkspaceconn.New(hostpaths.Gog(),
		func(ctx context.Context, in map[string]any) (any, error) {
			res, err := gogRunner.Execute(ctx, in)
			if err != nil {
				return nil, err
			}
			if res.IsError {
				// The output travels with the error on purpose: gog puts the
				// diagnosis in its body, and dropping it leaves the agent able
				// to say only "google failed".
				return res.Output, fmt.Errorf("%s", res.Error)
			}
			return res.Output, nil
		}))

	// Every connector is registered by now, so a name in `connectors:` that
	// matched nothing is a typo rather than a connector yet to come. Said out
	// loud, because an allowlist entry that silently matches nothing is
	// indistinguishable from one that is working.
	if unknown := connHost.UnknownDeclared(); len(unknown) > 0 {
		log.Warn("karmax.yaml names connectors this build does not have; they are being skipped",
			zap.Strings("connectors", unknown))
	}
	startedAt := time.Now()

	// Set provider env vars from config
	if p, ok := cfg.AI.Providers["anthropic"]; ok {
		if p.BaseURL != "" {
			os.Setenv("ANTHROPIC_BASE_URL", p.BaseURL)
		}
		if p.AuthToken != "" {
			os.Setenv("ANTHROPIC_AUTH_TOKEN", p.AuthToken)
		}
		if p.APIKey != "" {
			os.Setenv("ANTHROPIC_API_KEY", p.APIKey)
		}
	}

	// Set Google API key from config if present (for Gemini fallback)
	if p, ok := cfg.AI.Providers["google"]; ok {
		if p.APIKey != "" {
			os.Setenv("GOOGLE_API_KEY", p.APIKey)
		}
	}

	mcpBridge := mcp.NewBridge(log)
	for _, mcpCfg := range cfg.MCPs {
		if err := mcpBridge.AddServer(mcpCfg); err != nil {
			log.Error("failed to add MCP server", zap.String("id", mcpCfg.ID), zap.Error(err))
		}
	}

	// Create comms manager
	commsMgr := comms.NewManager(b, s, log)

	// Capture WhatsApp settings so the whatsapp.read tool can reuse them.
	waCLIPath := hostpaths.Wacli()
	waTarget := ""
	// Default agent for channel-originated notifications: the first configured
	// agent (channels can override via agent_id).
	waAgentID := ""
	if len(cfg.Agents) > 0 {
		waAgentID = cfg.Agents[0].ID
	}

	// WhatsApp is event-based: wacli pushes message events to KARMAX's webhook
	// endpoint (/comms/whatsapp, mounted below). KARMAX does NOT register or
	// scope that webhook — it's managed in wacli (via the `wacli` agent tool or
	// CLI), so no chat is hardcoded. The optional HMAC secret must match the
	// wacli webhook's --secret (set WHATSAPP_WEBHOOK_SECRET; empty = no verify).
	waWebhookSecret := os.Getenv("WHATSAPP_WEBHOOK_SECRET")
	var waChannel *whatsapp.WhatsAppChannel

	// Channel credentials come through the integration layer, so a token can be
	// in karmax.yaml OR obtained by `karmax login` — and the operator does not
	// have to know which one a given channel supports.
	integrationReg := integrations.Build(cfg, s)
	// So a browser sign-in that expires overnight renews itself rather than
	// turning into a connector that quietly stopped working.
	connHost.SetRefresher(integrationReg.Refresh)
	credential := func(id, field, fallback string) string {
		creds, _, err := integrationReg.Credentials(id)
		if err == nil {
			if v := strings.TrimSpace(creds.Get(field)); v != "" {
				return v
			}
		}
		return fallback
	}

	var webhookChannels []*webhookchat.Channel
	for _, chCfg := range cfg.Comms.Channels {
		switch chCfg.Type {
		case "slack":
			// Socket Mode: Slack dials out from us, so like Telegram this needs no
			// public URL. Two tokens — xapp- opens the socket, xoxb- posts messages.
			appToken := credential(chCfg.ID, "app_token", chCfg.Settings["app_token"])
			if appToken == "" {
				appToken = os.Getenv("SLACK_APP_TOKEN")
			}
			botToken := credential(chCfg.ID, "bot_token", chCfg.Token)
			if botToken == "" {
				botToken = os.Getenv("SLACK_BOT_TOKEN")
			}
			if unconfiguredChannel(log, chCfg.ID, "slack", appToken, botToken) {
				continue
			}
			ch := slack.New(chCfg.ID, appToken, botToken, log)
			if err := commsMgr.RegisterWithOptions(ch, chCfg.AgentID, comms.ChannelOptions{
				DND: dndEnabled(chCfg.Settings),
			}); err != nil {
				log.Error("failed to register comms channel",
					zap.String("id", chCfg.ID),
					zap.Error(err),
				)
			}
		case "whatsapp":
			wacliPath := chCfg.Settings["wacli_path"]
			if wacliPath == "" {
				wacliPath = hostpaths.Wacli()
			}
			targetChat := chCfg.Settings["target_chat"]
			waCLIPath = wacliPath
			waTarget = targetChat
			if chCfg.AgentID != "" {
				waAgentID = chCfg.AgentID
			}
			ch := whatsapp.New(chCfg.ID, wacliPath, targetChat, waWebhookSecret, log)
			// Makes restarts non-lossy: the channel records how far it has
			// processed and, on start, replays whatever arrived while the
			// daemon was down.
			ch.SetCursorStore(s)
			waChannel = ch
			if err := commsMgr.RegisterWithOptions(ch, chCfg.AgentID, comms.ChannelOptions{
				DND: dndEnabled(chCfg.Settings),
			}); err != nil {
				log.Error("failed to register comms channel",
					zap.String("id", chCfg.ID),
					zap.Error(err),
				)
			}
		case "webhook":
			// Any chat that speaks webhooks — Google Chat, Teams, Mattermost, a
			// Slack or Discord incoming webhook, or a custom bridge — by preset.
			ch := webhookchat.New(chCfg.ID, chCfg.Settings, log)
			if !ch.Configured() {
				log.Warn("webhook channel has no url; it can receive but not send", zap.String("id", chCfg.ID))
			}
			webhookChannels = append(webhookChannels, ch)
			if err := commsMgr.RegisterWithOptions(ch, chCfg.AgentID, comms.ChannelOptions{
				DND: dndEnabled(chCfg.Settings),
			}); err != nil {
				log.Error("failed to register comms channel",
					zap.String("id", chCfg.ID),
					zap.Error(err),
				)
			}
		default:
			log.Warn("unknown comms channel type", zap.String("type", chCfg.Type))
		}
	}

	// Now that the channel exists, the dry run has somewhere to send a draft.
	socialLimit.Preview = socialPreview(commsMgr, waTarget, log)

	// Operator identity: the operator's own chats (commands to KARMAX) vs
	// monitored third-party chats (proactive proxy). Comma-separated
	// phone/JID/@lid in WHATSAPP_OPERATOR_CHATS; falls back to WHATSAPP_TARGET.
	operatorChats := splitCSV(os.Getenv("WHATSAPP_OPERATOR_CHATS"))
	operatorFromFallback := false
	if len(operatorChats) == 0 && waTarget != "" {
		operatorChats = []string{waTarget}
		operatorFromFallback = true
	}
	// Logged because this one value decides, silently, whether a chat is a
	// command or something to watch — and the monitor loop reads the SAME
	// setting from the environment WITHOUT this fallback. When the fallback is
	// in play the two disagree: the agent hands the message to the loop, and
	// the loop, seeing an empty set, hands it back. Nobody answers, nothing is
	// logged.
	// Who is who, decided once from the same configuration: the operator's
	// name, their accounts (plus the LIDs their own chat has taught before),
	// and — once the bridge says — the linked account KARMAX sends from.
	// Everything that has to know who is speaking asks this.
	who := newIdentity(os.Getenv, operatorChats, s)
	waNames := chatmemory.NewWacli(hostpaths.WacliAPIURL())
	labelSpeaker := speakerLabeller(who, s.SentWhatsApp, waNames.Name)
	if waChannel != nil {
		waChannel.SetIdentity(who)
		waChannel.SetNames(func(jid string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return waNames.Name(ctx, jid)
		})
	}
	log.Info("operator identity resolved",
		zap.Int("operator_chats", len(operatorChats)),
		zap.Bool("from_whatsapp_target_fallback", operatorFromFallback),
		zap.Bool("operator_named", who.Name() != ""))

	// Act-and-inform: messages KARMAX sends to people OTHER than the operator
	// don't need approval, but the operator is shown every one via an app push.
	// Replies to the operator's own chats are skipped (they see those directly).
	commsMgr.RegisterOperatorTarget(waTarget)
	commsMgr.SetProactiveNotifier(func(target, content string) {
		body := content
		if len(body) > 240 {
			body = body[:240] + "…"
		}
		builtin.PushAppNotification(s, waAgentID, "update", "Sent to "+target, body)
	})

	voiceReg := voice.NewRegistry()
	toolReg := tools.NewRegistry()
	registerBuiltinTools(toolReg)

	// Before the tools: a tool the Broker will refuse is worse than no tool,
	// because the agent offers it and then fails at the point of use.
	connHost.ReconcileGrants()

	// Enabled connectors contribute tools indistinguishable from the built-ins.
	for _, t := range connHost.Tools() {
		toolReg.Register(t)
	}

	// Register new builtin tools
	// Registered before the agents are built, because an agent binds its
	// toolset at construction. The runtime they need does not exist yet, so
	// they hold a reference that is filled in below.
	harnessRT := &harnessRef{}
	toolReg.Register(&harnessSendTool{ref: harnessRT})
	toolReg.Register(&harnessListTool{ref: harnessRT})
	toolReg.Register(&harnessCloseTool{ref: harnessRT})
	toolReg.Register(&harnessStopTool{ref: harnessRT})
	toolReg.Register(&harnessShowTool{ref: harnessRT})
	toolReg.Register(&harnessModelTool{ref: harnessRT})
	toolReg.Register(&harnessTranscriptTool{ref: harnessRT})
	toolReg.Register(&harnessPruneTool{ref: harnessRT})
	// Work the orchestrator hands to itself, and keeps until it is done.
	toolReg.Register(&taskCreateTool{ref: harnessRT, agentID: agentIDOf(cfg)})
	toolReg.Register(&taskListTool{ref: harnessRT})
	toolReg.Register(&taskUpdateTool{ref: harnessRT})

	// apiBrowserToken/apiBaseURL are minted here — before either
	// ClaudeCodeTool registration in this function needs them — and threaded
	// through to the API server itself further down, when cfg.API.Enabled.
	// See the KarmaxRuntime field doc above for what this is for.
	var apiBrowserToken, apiBaseURL string
	if cfg.API.Enabled {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("mint browser-scoped API token: %w", err)
		}
		apiBrowserToken = base64.RawURLEncoding.EncodeToString(raw)
		apiBaseURL = fmt.Sprintf("http://localhost:%d", cfg.API.Port)
	}

	// One browser for the whole instance: the window the operator signs into is
	// the window the harness attaches to.
	browserSession := browser.Shared(cfg.Karmax.DataDir)
	toolReg.Register(&builtin.ClaudeCodeTool{Store: s, AgentID: "", Browser: browserSession,
		DataDir: cfg.Karmax.DataDir, EngineAPIURL: apiBaseURL, EngineBrowserToken: apiBrowserToken})
	toolReg.Register(&builtin.BrowserTool{Session: browserSession})
	toolReg.Register(&builtin.SubagentTool{Store: s, AgentID: "", Registry: toolReg})
	// Wired after construction: the runner belongs to the runtime, which does
	// not exist yet here. See the assignment further down.
	recipeTool := &builtin.RecipeTool{}
	toolReg.Register(recipeTool)
	// Registered through the Google Workspace connector rather than here — see
	// the registration above. GogTool remains as the thing that actually runs
	// gog, because it owns the flag defaults and the exit-code reasons, and
	// gog_schema.go resolves the binary through it.
	toolReg.Register(&builtin.GogSchemaTool{Path: hostpaths.Gog()})
	toolReg.Register(&builtin.SelfRemindTool{Clock: clk, AgentID: ""})
	toolReg.Register(&builtin.CapabilitiesTool{Registry: toolReg, Store: s, AgentID: ""})
	toolReg.Register(&builtin.ToolSearchTool{Registry: toolReg})
	toolReg.Register(&builtin.VoiceCallTool{Voice: voiceReg})
	toolReg.Register(&builtin.CostTool{Store: s, BudgetUSDPerMonth: cfg.Karmax.BudgetUSDPerMonth})
	toolReg.Register(&builtin.CommsSendTool{
		SendFunc:         commsMgr.Send,
		DefaultChannelID: commsMgr.DefaultChannelID,
		KnownChannelID:   commsMgr.HasChannel,
		HeardFromSince:   s.HeardFromSince,
	})
	// WhatsApp comes from wacli itself.
	//
	// It publishes its capabilities as karma tools — 29 of them, covering
	// messages, chats, contacts, calls, triggers, webhooks and media — so
	// KARMAX adopts them rather than hand-writing a wrapper per API call and
	// falling behind the moment wacli gains a feature. Four hand-written ones
	// were deleted to make room; wacli's are a superset.
	//
	// They go through the same registry as everything else, so the agent gets
	// them, a WASM workflow reaches them through the generic `tool` host
	// function, and the Broker gates them by name — none of which needed
	// changing, because tools were already the currency.
	//
	// Every message they return says who wrote it — the operator, KARMAX, or
	// someone else — from the account, so no reader of a thread has to work
	// that out from bare ids. Labelled after the guard, which recognises the
	// tools it wraps by type.
	waTools := builtin.AttributeSpeakers(builtin.GuardUntrusted(
		builtin.FromGoFunctionTools(wacli.All(wacli.New(hostpaths.WacliAPIURL()))),
		"WhatsApp, written by whoever sent it"), labelSpeaker)
	for _, t := range waTools {
		toolReg.Register(t)
	}
	// KARMAX's own policy on top of wacli's facts: which of the watched chats
	// are third parties rather than the operator's own. wacli knows the
	// webhooks; the subtraction is ours.
	toolReg.Register(&builtin.WhatsAppMonitoredTool{})
	toolReg.Register(&builtin.WhatsAppViewMediaTool{WacliPath: waCLIPath, Store: s, AgentID: waAgentID})
	// The memory model and the loop host each built one of these privately, so
	// the agent that actually talks to the operator was the only caller without
	// a way to read a chat — it had forty-six wacli tools it was never given and
	// a config asking for a name nobody had registered.
	toolReg.Register(&builtin.WhatsAppReadTool{WacliPath: waCLIPath, Store: s, Label: labelSpeaker})
	var waMonitorTool *builtin.WhatsAppMonitorTool
	if cfg.Webhooks.Enabled {
		waMonitorTool = &builtin.WhatsAppMonitorTool{
			WacliPath:  waCLIPath,
			WebhookURL: fmt.Sprintf("http://127.0.0.1:%d/comms/whatsapp", cfg.Webhooks.Port),
			Secret:     waWebhookSecret,
			Protected:  operatorChats,
		}
		toolReg.Register(waMonitorTool)

		// Webhook health: enforce the single-secured-webhook invariant on a timer
		// AND at startup. A stray/secret-less webhook (e.g. added out-of-band via
		// the raw wacli tool) 401s on every delivery, silently dropping that
		// contact's messages — this collapses the set back to one secured webhook.
		loopkit.Register(loopkit.Loop{
			Name:        "webhook-health",
			Description: "Every 15m verifies the wacli→KARMAX webhook is a single webhook carrying the HMAC secret; reconciles stray/secret-less webhooks so no contact's messages are silently dropped (401).",
			Schedule:    loopkit.Every("15m"),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				changed, n, err := waMonitorTool.Reconcile(ctx)
				if err != nil {
					k.Logf("webhook-health: reconcile failed: %v", err)
					return nil
				}
				if changed {
					k.Logf("webhook-health: reconciled %d fragmented/insecure webhook(s) into one secured webhook", n)
				}
				return nil
			},
		})
	}
	// Only expose notify.push (ntfy) to the agent when a topic is actually
	// configured — otherwise the tool can only ever fail, so we don't offer it.
	if ntfyTopic := os.Getenv("NTFY_TOPIC"); ntfyTopic != "" {
		toolReg.Register(&builtin.NtfyPushTool{Server: os.Getenv("NTFY_SERVER"), Topic: ntfyTopic})
	}
	toolReg.Register(&builtin.AppPushTool{Store: s})
	// The tool enforces the operating policy: under act-then-report it is for
	// the hard floor only.
	operating := policy.New(cfg.Policy)
	toolReg.Register(&builtin.ProposeTool{Store: s, Policy: &operating})
	toolReg.Register(&builtin.CalendarAddTool{Store: s})
	toolReg.Register(&builtin.ReminderAddTool{Store: s})
	toolReg.Register(&builtin.ContactAddTool{Store: s})
	toolReg.Register(&builtin.ContactUpdateTool{Store: s})

	// The scheduler (and its tool) must exist BEFORE agents resolve their tool
	// lists — registering scheduler.add afterwards silently dropped it from
	// every agent's toolset.
	sched := scheduler.New(s, b, log)
	toolReg.Register(&builtin.SchedulerTool{Scheduler: sched, AgentID: ""})
	// What the operator has been building. KARMAX has recorded every delegated
	// engineering task since the first one; nothing could read them back.
	toolReg.Register(&builtin.ActivityTool{Store: s, AgentID: ""})
	// Registers a long-running background job as a tracked task before a turn
	// hands the work to a detached process — see longrun.go. task.start opens
	// the row, task.progress is the detached process's write-and-control-
	// signal, task.status is the pause button (and a plain read/list).
	toolReg.Register(&builtin.TaskStartTool{Store: s, AgentID: ""})
	toolReg.Register(&builtin.TaskProgressTool{Store: s})
	toolReg.Register(&builtin.TaskStatusTool{Store: s})

	memFactory := memory.NewFactory(filepath.Join(dataDir, "memory"), s, log)
	forbidden.attach(memFactory)

	// Long-term memory IS GitLoom when a key is configured. Without one KARMAX
	// stays entirely local, which is the self-hosted default and what an
	// open-source user gets with no account.
	var conversations *agent.Conversations
	if glCfg, ok := memory.GitLoomConfigFromEnv(defaultNamespace(cfg)); ok {
		memFactory.UseGitLoom(glCfg)
		// The same store also holds the operator's conversations with KARMAX,
		// which is what turns "what did we decide about Contoso" from something
		// the agent had to remember to write down into something it can read
		// back from what was actually said.
		model := ""
		if len(cfg.Agents) > 0 {
			model = cfg.Agents[0].Model
		}
		conversations = agent.NewConversations(agent.ConversationsConfig{
			APIKey: glCfg.APIKey, BaseURL: glCfg.BaseURL,
			Namespace: glCfg.Namespace, Model: model,
		}, log)
	} else {
		log.Info("memory: GitLoom not configured; long-term memory stays in the local database")
	}

	// Memory upkeep (the forgetting curve: TTL pruning + capacity cap) is a
	// regular loop — visible, disableable, and manually triggerable — not a
	// hidden goroutine. It needs the memory managers, so the runtime registers
	// it here rather than the marketplace hosting it.
	// Integration health, on a timer.
	//
	// A dead credential is otherwise found by whatever depended on it failing —
	// an expired Google token surfaced as a loop erroring at 4am, which is both
	// the worst time and the least informative place to learn it. Checked here
	// so `karmax integrations` and the app can say so first.
	loopkit.Register(loopkit.Loop{
		Name: "integration-health",
		Description: "Checks every connected integration's credentials against the provider, " +
			"so an expired token is visible before something depends on it.",
		Schedule: loopkit.Every("30m"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			var broken []string
			for _, st := range integrationReg.CheckAll(ctx) {
				if st.Configured && !st.Healthy {
					broken = append(broken, st.ID+" ("+st.Error+")")
				}
			}
			if len(broken) > 0 {
				k.Logf("integration-health: %d not working — %s",
					len(broken), strings.Join(broken, "; "))
			}
			return nil
		},
	})

	// Short-term memory upkeep: drop KV entries whose TTL has passed. Reads
	// already hide expired rows, so this only keeps the table from growing.
	loopkit.Register(loopkit.Loop{
		Name:        "shortmem-sweep",
		Description: "Hourly sweep of expired short-term (KV) memories written by loops — the scratch store's garbage collector.",
		Schedule:    loopkit.Every("1h"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			n, err := s.KVPurgeExpired()
			if err != nil {
				return err
			}
			if n > 0 {
				k.Logf("shortmem-sweep: purged %d expired short-term memories", n)
			}
			return nil
		},
	})

	loopkit.Register(loopkit.Loop{
		Name:        "memory-maintenance",
		Description: "Hourly forgetting pass over every memory namespace: prunes TTL-expired facts and enforces the capacity cap (least-valuable, non-pinned entries go first).",
		Schedule:    loopkit.Every("1h"),
		Run: func(ctx context.Context, k loopkit.Kit) error {
			removed := 0
			for _, m := range memFactory.Managers() {
				removed += m.Maintain()
			}
			if removed > 0 {
				k.Logf("memory-maintenance: forgot %d entries", removed)
			}
			return nil
		},
	})

	// Staleness review: the "is this still relevant?" check-ins that keep memory
	// current. Aggressive cadence, but capped + latched so it never spams. Needs
	// the store + the agent's model + the WhatsApp channel, so it's a runtime
	// loop like memory-maintenance.
	var googleSrc *googleSources
	if len(cfg.Agents) > 0 {
		a0 := cfg.Agents[0]
		ns := a0.Memory.Namespace
		if ns == "" {
			ns = a0.ID
		}
		var fbs []karmahelper.FallbackModel
		for _, fb := range a0.FallbackModels {
			fbs = append(fbs, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
		}
		provider, model := a0.SummaryModel.Provider, a0.SummaryModel.Model
		if provider == "" {
			provider = a0.Provider
		}
		if model == "" {
			model = a0.Model
		}
		waChannelID, _ := commsMgr.FindChannelIDByType("whatsapp")
		reviewer := review.New(review.Config{
			Namespace: ns, AgentID: waAgentID, Provider: provider, Model: model, Fallbacks: fbs,
			WAChannelID: waChannelID, WATarget: waTarget, SendFunc: func(c, t, m string) error { return commsMgr.SendFrom(comms.ProducerLoop, c, t, m) },
			// Unanswered questions expire (default a week), so they cannot
			// hold the open-question cap and stop the loop asking.
			ExpireAfter: reviewExpireAfter(os.Getenv),
			// Answered by a warm session when one is available; the metered
			// path stays as the fallback and is used whenever it is not.
			Ask: func(ctx context.Context, key, prompt string) (string, bool) {
				// Late-bound: review is built before the runtime exists.
				return harnessRT.get().harnessAnswer(ctx, key, prompt)
			},
		}, s, memFactory.For(waAgentID, ns), log)
		loopkit.Register(loopkit.Loop{
			Name:        "memory-review",
			Description: "Finds stale, time-sensitive memories & reminders and asks the operator (app + WhatsApp) if each is still relevant — once per item, capped so it never spams.",
			Schedule:    loopkit.Every("45m"),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				return reviewer.Tick(ctx)
			},
		})

		// Memory consolidation: an LLM merges duplicate / near-duplicate /
		// superseded entries within a category into one canonical fact. This is
		// the "keep memory clean as it grows" pass — stronger than the
		// write-time Jaccard dedup. Uses the memory model (strong reasoning);
		// one category per tick so each run stays cheap.
		mergeProvider, mergeModel := a0.MemoryModel.Provider, a0.MemoryModel.Model
		if mergeProvider == "" {
			mergeProvider = a0.Provider
		}
		if mergeModel == "" {
			mergeModel = a0.Model
		}
		merger := memmerge.New(memmerge.Config{
			Namespace: ns, Provider: mergeProvider, Model: mergeModel, Fallbacks: fbs,
			Ask: func(ctx context.Context, key, prompt string) (string, bool) {
				return harnessRT.get().harnessAnswer(ctx, key, prompt)
			},
		}, s, memFactory.For(a0.ID, ns), log)
		loopkit.Register(loopkit.Loop{
			Name:        "memory-merge",
			Description: "Every few hours an LLM consolidates duplicate / near-duplicate / superseded memories in the largest category into a single canonical fact, keeping memory clean as it grows.",
			Schedule:    loopkit.Every("3h"),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				n, err := merger.Tick(ctx)
				if err != nil {
					return err
				}
				if n > 0 {
					k.Logf("memory-merge: consolidated %d entries", n)
				}
				return nil
			},
		})

		// Chat services → memory: every conversation's new messages, the
		// operator's own included, set beside what memory already holds,
		// become facts added, corrected or retired — one loop per service,
		// over the same syncer. A chat never read before starts at the loop's
		// _SINCE, which is how the same loop backfills history. See
		// chatmemoryhost.go and internal/chatmemory.
		home, _ := os.UserHomeDir()
		chatMem := chatMemoryDeps{Store: s, Memory: memFactory.For(a0.ID, ns), Getenv: os.Getenv,
			Home: filepath.Join(home, ".karmax"), Log: log,
			Extract: func(ctx context.Context, key, prompt string) (string, error) {
				return harnessRT.get().harnessExtract(ctx, key, prompt)
			}}
		waMemory.start(chatMem, waNames, func(c *chatmemory.Config) {
			// The configured accounts, and the directory's answer, which
			// includes the LIDs it has learned.
			c.OperatorIDs, c.IsOperator = operatorChats, who.IsOperator
		}, func(c chatmemory.Chat, text string, at time.Time) bool { return s.SentWhatsApp(c.ID, text, at) })

		// Google Calendar and Chat: every signed-in account's events to the
		// primary agent, and the actions to answer them. See googlesource.go.
		googleSrc = startGoogleSources(s, b, toolReg, integrationReg, a0.ID, log)
		// Google Chat → memory, through the same logins. KARMAX's own sends
		// there are the operator's account too, told apart by the sent-as
		// ledger.
		gchatMemory.start(chatMem, googleSrc.chatMemorySource(log), nil,
			func(c chatmemory.Chat, text string, at time.Time) bool {
				return s.SentAsOperator(gchatMemory.Tag, c.Account, text, at)
			})

		// AWS: every usable profile's CloudWatch alarms and spend, as events
		// to the primary agent. KARMAX_SOURCE_AWS=off turns it off. See
		// awssource.go and internal/sources/aws.
		registerAWSSource(os.Getenv, home, s, b, a0.ID, toolReg, log)

		// GitHub → events, and acting as the right login: every account gh
		// is logged in to. Before the agents bind their tools. See
		// githubsource.go; KARMAX_SOURCE_GITHUB=off stops the polling.
		registerGitHubSource(os.Getenv, s, b, toolReg, integrationReg, a0.ID, log)

		// Every registered tool that sends as the operator records through
		// the one recorder (sentas.go), so its sends are told after.
		toolReg.GiveSentRecorder(recordSentAs(s, a0.ID))

		// Brain monitor: pings the actual brain the agent depends on — the
		// harness (Claude Code via the supervisor; see
		// docs/CLAUDE-ONLY-ORCHESTRATOR.md), not the metered API path nothing
		// in this daemon actually answers on — and alerts the operator (app +
		// WhatsApp, fixed text — NOT model-composed, since a dead brain can't
		// write) the moment it goes down, and again when it recovers. Latched
		// so it alerts on transitions, not every tick.
		//
		// An open circuit breaker is reported as healthy-but-paused, never as
		// down: it means the daemon is deliberately declining to spend quota
		// right now, which is the system working as designed. Reporting that
		// as an outage would replace one false alarm with another that fires
		// on exactly the schedule a quota pause is likeliest to happen on.
		// checkBrainHealth (brainmonitor.go) makes that distinction the same
		// way harnesstools.go's asBreakerOpen does.
		//
		// Rides the cheapest kind ("classify" — see the tier table in
		// CLAUDE-ONLY-ORCHESTRATOR.md) on its own session key, never an
		// operator-priority kind: a ten-minute health check must not spend a
		// share of the operator's own quota.
		brainLatch := &brainMonitorLatch{}
		loopkit.Register(loopkit.Loop{
			Name:        "brain-monitor",
			Description: "Pings the harness (the agent's real brain) every few minutes on the cheapest tier and alerts you if it goes down or comes back — in the app, and on WhatsApp as an urgent item when the operating policy sends those — so an outage never silently deafens KARMAX. A paused circuit breaker (quota policy) is never reported as down.",
			Schedule:    loopkit.Every("10m"),
			Run: func(ctx context.Context, k loopkit.Kit) error {
				h := harnessRT.get()
				if h == nil || h.harness == nil {
					return nil // harness disabled: nothing to monitor
				}
				pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				event, reason := brainLatch.tick(pctx, h.harness)
				switch event {
				case brainMonitorAlertDown:
					// One notice, urgent: the app, and WhatsApp through the
					// policy. It used to go to WhatsApp twice — directly, and
					// again through the mirror of the app notification.
					msg := fmt.Sprintf("⚠️ KARMAX brain is DOWN (harness: %.140s). Your messages won't be answered until it recovers.", reason)
					builtin.PushAppNotificationUrgent(s, waAgentID, "alert", "⚠️ KARMAX brain is down", msg)
					k.Logf("brain-monitor: DOWN (%s)", reason)
				case brainMonitorAlertRecovered:
					// Closes the urgent item above, so it travels the same way.
					msg := "✅ KARMAX brain is back online. Resend anything I missed."
					builtin.PushAppNotificationUrgent(s, waAgentID, "update", "✅ KARMAX brain recovered", msg)
					k.Logf("brain-monitor: recovered")
				}
				return nil
			},
		})
	}

	agentReg := agent.NewRegistry(b, s, log)

	for _, agentCfg := range cfg.Agents {
		def := configToAgentDef(agentCfg)

		var mem *memory.Manager
		if def.Memory.Enabled {
			mem = memFactory.For(def.ID, def.Memory.Namespace)
		} else {
			mem = memFactory.For(def.ID, def.ID)
		}

		agentTools, unresolved := toolReg.ResolveForAgent(def.Tools)
		// Agent-scoped tools (memory.*, comms.escalate, profile.update) are
		// injected per-agent in initModels, so they are expected to be absent
		// from the global registry. Only warn about genuinely unknown names,
		// and never drop the tools that did resolve.
		var unknownTools []string
		for _, name := range unresolved {
			if !tools.IsAgentScoped(name) {
				unknownTools = append(unknownTools, name)
			}
		}
		if len(unknownTools) > 0 {
			log.Warn("agent lists unknown tools (skipped)", zap.String("agent", def.ID), zap.Strings("tools", unknownTools))
			def.UnknownTools = unknownTools
		}

		var mcpTools []tools.Tool
		for _, mcpID := range def.MCPs {
			mt, err := mcpBridge.GetTools(mcpID)
			if err != nil {
				log.Warn("failed to get MCP tools", zap.String("agent", def.ID), zap.String("mcp", mcpID), zap.Error(err))
				continue
			}
			mcpTools = append(mcpTools, mt...)
		}

		a, err := agentReg.Register(def, mem, agentTools, mcpTools)
		if err != nil {
			log.Error("failed to register agent", zap.String("agent", def.ID), zap.Error(err))
			continue
		}

		// Two tiers of memory: the organisation's, shared by every agent,
		// recipe and workflow, and one per person that is only in play while
		// acting for them.
		//
		// Wired whether or not GitLoom is configured — the separation is about
		// who may read what, not about where it is stored.
		a.SetScopes(memory.NewScopes(memFactory, s, def.ID, defaultNamespace(cfg), log))

		// Wire comms send function into the agent
		a.SetCommsSend(commsMgr.Send)
		a.SetStatusSend(func(channelID, target, content string) error {
			return commsMgr.SendFrom(comms.ProducerStatus, channelID, target, content)
		})
		a.SetCommsEscalate(commsMgr.RequestEscalation)

		a.SetOperatorChats(operatorChats)
		a.SetIdentity(who)
		// Nil when GitLoom is not configured, which the agent treats as "do not
		// archive" rather than as an error.
		a.SetConversations(conversations)

		// Inject available comms channel info into the agent for context building
		agentChannels := commsMgr.GetChannelsForAgent(agentCfg.ID)
		var channelInfos []agent.CommsChannelInfo
		for _, ch := range agentChannels {
			channelInfos = append(channelInfos, agent.CommsChannelInfo{
				KarmaxChannelID: ch.ID(),
				Type:            ch.Type(),
				DND:             commsMgr.ChannelDND(ch.ID()),
			})
		}
		a.SetCommsChannels(channelInfos)
	}

	whAddr := fmt.Sprintf("%s:%d", cfg.Webhooks.Host, cfg.Webhooks.Port)
	wh := webhook.New(whAddr, b, s, log)

	// Mount the WhatsApp event endpoint that wacli pushes message events to.
	if waChannel != nil {
		wh.AddHandler("/comms/whatsapp", waChannel.HandleWebhook)
	}
	for _, ch := range webhookChannels {
		wh.AddHandler(ch.InboundPath(), ch.HandleInbound)
	}

	// Issue trackers and code hosts are event sources, not conversations: each
	// delivery becomes one normalised tracker.event on the bus for loops to judge.
	// A platform is only mounted when its secret is set, so an unconfigured
	// endpoint doesn't sit there accepting anonymous POSTs.
	for _, t := range []struct {
		src    tracker.Source
		path   string
		secret string
	}{
		{tracker.GitHub, "/hooks/github", os.Getenv("GITHUB_WEBHOOK_SECRET")},
		{tracker.Jira, "/hooks/jira", os.Getenv("JIRA_WEBHOOK_TOKEN")},
		{tracker.YouTrack, "/hooks/youtrack", os.Getenv("YOUTRACK_WEBHOOK_TOKEN")},
	} {
		if t.secret == "" {
			continue
		}
		h := tracker.New(tracker.Config{
			Source:  t.src,
			Secret:  t.secret,
			AgentID: waAgentID,
		}, b, log)
		wh.AddHandler(t.path, h.ServeHTTP)
		log.Info("tracker webhook mounted",
			zap.String("source", string(t.src)), zap.String("path", t.path))
	}

	connHost.MountWebhooks(wh.AddHandler)

	// Operator-defined webhooks, dispatched from a lookup rather than the mux.
	// One handler for all of them is what lets an endpoint be created, edited
	// or deleted from the console and take effect on the next delivery — the
	// mux-registered routes below can only ever be set up at boot.
	webhook.NewCustomDispatcher(s, b, log).Mount(wh.AddHandler)

	for _, route := range cfg.Webhooks.Routes {
		wh.AddRoute(webhook.WebhookRoute{
			Path:            route.Path,
			Method:          route.Method,
			AgentID:         route.AgentID,
			BusEvent:        route.BusEvent,
			Secret:          route.Secret,
			SignatureHeader: route.SignatureHeader,
			Response:        route.Response,
		})
	}

	var apiSrv *api.Server
	if cfg.API.Enabled {
		apiAddr := fmt.Sprintf("%s:%d", cfg.API.Host, cfg.API.Port)
		apiSrv = api.New(apiAddr, cfg.API.Port, os.Getenv("KARMAX_API_TOKEN"), apiBrowserToken, agentReg, s, sched, memFactory, cfg, log)
	}

	// Wire bus events to agent inboxes (webhooks, scheduled jobs, user-defined,
	// and comms messages). Webhook routes may remap their event to a custom
	// bus_event kind, and agents may declare extra event kinds in
	// triggers.events — subscribe to those too, or they are published and then
	// silently dropped.
	// EventTimerFired is here so the agent can wake ITSELF: self.remind arms a
	// durable timer, and without this the timer fired into the loops subscriber
	// only and the agent never heard about the reminder it set.
	routedKinds := []bus.EventKind{bus.EventWebhookFired, bus.EventScheduledJob, bus.EventUserDefined,
		bus.EventCommsMessage, bus.EventDelegationDone, bus.EventTimerFired}
	seenKinds := map[bus.EventKind]bool{}
	for _, k := range routedKinds {
		seenKinds[k] = true
	}
	for _, route := range cfg.Webhooks.Routes {
		if k := bus.EventKind(route.BusEvent); route.BusEvent != "" && !seenKinds[k] {
			seenKinds[k] = true
			routedKinds = append(routedKinds, k)
		}
	}
	for _, agentCfg := range cfg.Agents {
		for _, ev := range agentCfg.Triggers.Events {
			if k := bus.EventKind(ev); ev != "" && !seenKinds[k] {
				seenKinds[k] = true
				routedKinds = append(routedKinds, k)
			}
		}
	}
	rt := &KarmaxRuntime{
		cfg:          cfg,
		log:          log,
		store:        s,
		bus:          b,
		clock:        clk,
		broker:       brk,
		connectors:   connHost,
		recipeLoops:  map[string]*recipes.Recipe{},
		waChannel:    waChannel,
		who:          who,
		labelSpeaker: labelSpeaker,
		messageOperator: func() func(string) error {
			chID, _ := commsMgr.FindChannelIDByType("whatsapp")
			if chID == "" || waTarget == "" {
				return nil
			}
			return func(text string) error { return commsMgr.SendFrom(comms.ProducerLoop, chID, waTarget, text) }
		}(),
		voice:        voiceReg,
		routedKinds:  routedKinds,
		policy:       operating,
		pending:      newPendingTriggers(),
		startedAt:    startedAt,
		wasmByName:   map[string]*wasmloop.Runner{},
		attributions: newAttributions(),
		memory:       memFactory,
		tools:        toolReg,
		mcpBridge:    mcpBridge,
		agents:       agentReg,
		scheduler:    sched,
		webhooks:     wh,
		comms:        commsMgr,
		api:          apiSrv,

		google:          googleSrc,
		apiBrowserToken: apiBrowserToken,
		apiBaseURL:      apiBaseURL,
	}
	recipeTool.Run = func(ctx context.Context, name string) (bool, error) {
		rt.recipeMu.RLock()
		_, known := rt.recipeLoops[name]
		rt.recipeMu.RUnlock()
		if !known {
			return false, nil
		}
		return true, rt.RunRecipe(ctx, name, loopkit.Trigger{Kind: loopkit.TriggerManual})
	}

	// Every model call in the process reports here, so the tracker measures
	// the bill rather than the parts of it somebody remembered to record.
	// Installed once: the meter is package-level in karmahelper because that
	// is the only place a session cannot avoid passing through.
	karmahelper.OnUsage(func(u karmahelper.Usage) {
		if err := s.RecordModelUsage(store.ModelUsage{
			AgentID: u.AgentID, Provider: u.Provider, Model: u.Model, Kind: u.Kind,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		}); err != nil {
			log.Warn("could not record model usage", zap.Error(err))
		}
	})

	// Installed here rather than where the agents are registered, because it
	// closes over the runtime and the runtime does not exist until now.
	for _, a := range agentReg.List() {
		a.SetLentTools(rt.lentToolsForEvent)
		a.SetScreener(rt.screenEvent)
		a.SetDecider(rt.decide)
		rt.applyOperatingPolicy(a)
		a.SetAccounts(accountsFrom(integrationReg))
	}

	// The tools were registered before the agents; this is the runtime they
	// were waiting for.
	harnessRT.rt = rt

	// System One. An error here is a misconfiguration worth refusing to start
	// over; a missing key is not, and returns a nil evaluator that passes every
	// event straight through.
	rt.reflex, err = reflex.New(cfg.Reflex, log.Named("reflex"))
	if err != nil {
		return nil, fmt.Errorf("reflex: %w", err)
	}
	if rt.reflex != nil {
		log.Info("reflex screening is on",
			zap.String("model", cfg.Reflex.Model),
			zap.Float64("drop_threshold", rt.reflex.Thresholds().Drop))
	}

	rt.installOperatorMirror(s, waAgentID, rt.messageOperator)

	// Event sources: each polls its accounts and publishes what happened as a
	// webhook.fired event, and stays off — saying what to run — until its
	// credentials exist. See internal/sources.
	rt.registerSlackSource(integrationReg)
	rt.registerPostHogSource(integrationReg)

	return rt, nil
}

func (rt *KarmaxRuntime) Start(ctx context.Context) error {
	rt.printBanner()

	// Started before anything can ask for a session, and it reaps the previous
	// process's orphans on the way up.
	rt.harness = rt.startHarness()
	// Which account the bridge is linked to — KARMAX's own sends and the
	// operator typing on it. Asked in the background; the session brief is
	// rewritten when it answers.
	rt.learnLinkedAccount(ctx, hostpaths.WacliAPIURL())
	// Before anything can open a model session: every claude-code session
	// runs on the harness, and one opened before this would find no path.
	rt.wireClaudeCodeInference()
	if rt.harness != nil {
		rt.startHarnessReaper(ctx)
		// Brains are wired AFTER agents start, further down. An agent's API
		// session — the harness's fallback — does not exist until StartAll
		// runs initModels, and capturing it here captures nil.
	}

	rt.clock.Start(ctx)
	rt.connectors.StartPollers(ctx)
	// Keep the console's health column true without anyone clicking. A status
	// nobody has established is not a status, and one that resets on every
	// restart is worse — it teaches people the column means nothing.
	rt.connectors.StartProbing(ctx)
	rt.startRecipes(ctx)
	rt.startAgentRouter(ctx)
	rt.startCriticalAlertLoop(ctx)
	rt.startDeadLetterAlerts()

	// Runs parked on an event: this daemon is answerable for them and would
	// otherwise silently drop them on restart.
	rt.startWaiters(ctx)

	if err := rt.mcpBridge.StartAll(ctx); err != nil {
		rt.log.Error("MCP bridge start error", zap.Error(err))
		rt.publishCritical("", "MCP bridge start error", map[string]any{"error": err.Error()})
	}

	if err := rt.comms.StartAll(ctx); err != nil {
		rt.log.Error("comms start error", zap.Error(err))
		rt.publishCritical("", "comms start error", map[string]any{"error": err.Error()})
	}

	if err := rt.scheduler.Start(ctx); err != nil {
		rt.log.Error("scheduler start error", zap.Error(err))
		rt.publishCritical("", "scheduler start error", map[string]any{"error": err.Error()})
	}

	// Mounted once agents exist, because the relay answers as one of them.
	if rt.webhooks != nil {
		if a, ok := rt.agents.Get(rt.voiceAgentID()); ok {
			rt.mountVoice(rt.webhooks, a)
		}
	}

	for _, id := range rt.cfg.AutoStarted {
		rt.log.Info("agent has no triggers.run_on_start but a comms channel or webhook route delivers to it; starting it so messages are not dead-lettered",
			zap.String("agent", id))
	}
	if err := rt.agents.StartAll(ctx); err != nil {
		rt.log.Error("agent start error", zap.Error(err))
		rt.publishCritical("", "agent start error", map[string]any{"error": err.Error()})
	}

	// Owned work is driven forward on its own clock, not by whoever happens to
	// send the next message.
	rt.startTaskRunner(ctx)

	// Now that every agent has its API session, the harness can be given one
	// to fall back to. Wired before this point, the fallback is nil and a
	// declined turn — a tripped breaker, an exhausted window, expired auth —
	// returns an error instead of an answer, which is a silent agent.
	if rt.harness != nil {
		rt.wireHarnessBrains()
	}

	// Start health checks for all agents
	for _, a := range rt.agents.List() {
		a.StartHealthCheck(ctx)
	}

	// Register scheduler triggers from agent definitions. Stable IDs prevent
	// duplicate jobs from accumulating in the store across restarts.
	for _, agentCfg := range rt.cfg.Agents {
		for i, sched := range agentCfg.Triggers.Schedules {
			rt.scheduler.AddJob(scheduler.ScheduledJob{
				ID:      fmt.Sprintf("agent:%s:sched:%d", agentCfg.ID, i),
				Name:    fmt.Sprintf("%s-trigger", agentCfg.ID),
				Cron:    sched.Cron,
				AgentID: agentCfg.ID,
				Payload: sched.Payload,
				Enabled: true,
			})
		}
	}

	// Register declarative loops: each fires its prompt to the target agent.
	for _, loop := range rt.cfg.Loops {
		if loop.Enabled != nil && !*loop.Enabled {
			continue
		}
		payload := map[string]any{
			"loop":   loop.Name,
			"prompt": loop.Prompt,
		}
		if loop.Harness != "" {
			payload["harness"] = loop.Harness
		}
		for k, v := range loop.Payload {
			if k == "loop" || k == "prompt" || k == "harness" {
				continue
			}
			payload[k] = v
		}
		if err := rt.scheduler.AddJob(scheduler.ScheduledJob{
			ID:      "loop:" + loop.Name,
			Name:    "loop:" + loop.Name,
			Cron:    loop.Cron,
			AgentID: loop.Agent,
			Payload: payload,
			Enabled: true,
		}); err != nil {
			rt.log.Error("failed to register loop", zap.String("loop", loop.Name), zap.Error(err))
		}
	}

	// Loops authored via the public loopkit SDK (third-party + installed).
	rt.startLoopkitLoops(ctx)

	// Drop persisted scheduler jobs for loops that no longer exist or are
	// disabled, so stale entries don't reload and fire as duplicates.
	rt.pruneStaleLoopJobs()

	// Dispatches retries for loops that failed, including ones that failed
	// before the last restart. This is what makes a failed run survive the
	// process rather than dying with the goroutine that logged it.
	go rt.retryWorker(ctx)

	// One out-of-band model path for EVERY karmahelper session in the process —
	// the agent, the summariser, the memory merger, the loops. Installed here
	// because karmahelper must not know about the harness; it only knows there
	// is a last resort. Without this the configured "fallback models" are not
	// redundancy at all: they share one base URL and die with one process.
	karmahelper.SetTransportFallback(func(c context.Context, prompt string) (string, error) {
		tool := &builtin.ClaudeCodeTool{Store: rt.store, AgentID: rt.loopDefaultAgent,
			MemoryMgr: rt.memory.For(rt.loopDefaultAgent, rt.loopNamespace()),
			Browser:   browser.Shared(rt.cfg.Karmax.DataDir),
			DataDir:   rt.cfg.Karmax.DataDir}
		res, err := tool.Execute(c, map[string]any{"prompt": prompt, "ephemeral": true})
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "", fmt.Errorf("harness: %s", res.Error)
		}
		out := loopToolField(res, "output")
		if strings.TrimSpace(out) == "" {
			return "", fmt.Errorf("harness returned nothing")
		}
		return out, nil
	})

	// Let the API run any loop on demand (manual trigger) and report the live
	// loop list (the daemon's truth — includes runtime-registered loops).
	if rt.api != nil {
		rt.api.SetRunLoop(rt.RunLoopByName)
		rt.api.SetChatTurn(rt.chatTurn)
		rt.api.SetLoopHealth(func() (any, error) { return rt.LoopHealthReport() })
		// listLoopInfos (loopinfo.go) covers every tier, including disabled
		// ones — this used to only walk rt.loopkitLoops, which misses the
		// recipe and prompt tiers entirely and drops anything disabled.
		rt.api.SetListLoops(rt.listLoopInfos)
		rt.api.SetLoopsChanged(rt.ReapplyRecipes)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	if rt.cfg.Webhooks.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.webhooks.Start(ctx); err != nil {
				errCh <- fmt.Errorf("webhook server: %w", err)
			}
		}()
	}

	if rt.api != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rt.api.Start(ctx); err != nil {
				errCh <- fmt.Errorf("api server: %w", err)
			}
		}()
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-errCh:
				if err == nil {
					continue
				}
				rt.log.Error("runtime component failed", zap.Error(err))
				rt.publishCritical("", "runtime component failed", map[string]any{"error": err.Error()})
			}
		}
	}()

	<-ctx.Done()
	rt.log.Info("shutting down...")

	rt.agents.StopAll()
	rt.scheduler.Stop()
	rt.comms.StopAll()
	rt.mcpBridge.StopAll()
	rt.memory.StopAll()
	rt.webhooks.Stop()
	if rt.api != nil {
		rt.api.Stop()
	}
	rt.closeWasmLoops(context.Background())
	rt.store.Close()

	wg.Wait()
	return nil
}

// startAgentRouter delivers routed events to agent inboxes. No agent means
// nowhere to go, which is fine; a full inbox is retried and then dead-lettered.
//
// Every delivery opens a turn row first. The bus advances its offset when this
// handler returns, and this handler returns as soon as the event is in an
// in-memory mailbox — so without the row, a crash between here and the end of
// the turn loses the work AND the event, because the subscriber resumes past
// it and nothing redelivers it. The row is what a restart finds and retries.
func (rt *KarmaxRuntime) startAgentRouter(ctx context.Context) {
	rt.bus.Consume(ctx, bus.SubAgentRouter, rt.routedKinds,
		func(_ context.Context, evt bus.Event) error { return rt.routeToAgent(evt) })
}

// routeToAgent is the router's handler for one event.
func (rt *KarmaxRuntime) routeToAgent(evt bus.Event) error {
	if evt.AgentID == "" {
		return nil
	}
	if age, stale := staleEvent(evt); stale {
		// Dropped rather than retried: a subscriber that falls behind
		// would otherwise replay weeks of conversation as if it had all
		// just arrived, and each replay costs a model turn it can never
		// catch up on. Answering a seven-week-old message is worse than
		// not answering it.
		rt.log.Warn("skipped an event too old to act on",
			zap.String("kind", string(evt.Kind)), zap.String("event", evt.ID),
			zap.Duration("age", age.Round(time.Minute)))
		return nil
	}
	a, ok := rt.agents.Get(evt.AgentID)
	if !ok {
		return nil
	}
	if !rt.openTurn(evt) {
		// Already in flight — a redelivery of something still running.
		return nil
	}
	if err := a.Send(evt); err != nil {
		// It never reached the agent, so nothing will ever finish this
		// row. Close it here or the next restart would "resume" a turn
		// that never started.
		_ = rt.store.FinishAgentTurn(evt.ID, store.TurnFailed, err.Error())
		return err
	}
	return nil
}

// Freshness windows for routed events.
//
// A conversation is perishable: replying to a message fifteen minutes late is
// still a reply, an hour late is noise, and weeks late is a bug the operator
// experiences as the agent talking to itself about the past. Timers and
// scheduled work are different — those are meant to survive downtime and fire
// late, so they get a day, which still rejects a months-old replay.
const (
	commsFreshness   = 15 * time.Minute
	defaultFreshness = 24 * time.Hour
)

// staleEvent reports whether an event is too old to be worth acting on.
func staleEvent(evt bus.Event) (time.Duration, bool) {
	// A missing timestamp is not evidence of age, and dropping on it would
	// discard live work.
	if evt.Timestamp.IsZero() {
		return 0, false
	}
	limit := defaultFreshness
	switch evt.Kind {
	case bus.EventCommsMessage, bus.EventAgentMessage, bus.EventSystemCritical:
		limit = commsFreshness
	}
	age := time.Since(evt.Timestamp)
	return age, age > limit
}

// openTurn records a turn as running, reporting whether this caller owns it.
func (rt *KarmaxRuntime) openTurn(evt bus.Event) bool {
	payload, _ := json.Marshal(evt)
	owned, err := rt.store.StartAgentTurn(store.AgentTurn{
		ID:        uuid.New().String(),
		AgentID:   evt.AgentID,
		EventID:   evt.ID,
		EventKind: string(evt.Kind),
		EventJSON: string(payload),
		Attempt:   1,
	})
	if err != nil {
		// A journal that cannot be written is not a reason to drop the work:
		// losing the turn is strictly worse than losing its crash-safety.
		rt.log.Warn("could not journal agent turn; delivering anyway",
			zap.String("event", evt.ID), zap.Error(err))
		return true
	}
	return owned
}

// resumeInterruptedTurns redelivers work the daemon was killed in the middle of.
//
// Modelled on resumeInterruptedRuns for loops. Anything still marked running at
// startup cannot be running — this process has only just begun — so it is reaped
// and handed back to its agent.
func (rt *KarmaxRuntime) resumeInterruptedTurns() {
	turns, err := rt.store.ReapStaleTurns(time.Now(), maxTurnAttempts)
	if err != nil {
		rt.log.Warn("could not reap interrupted turns", zap.Error(err))
		return
	}
	for _, t := range turns {
		if t.Status == store.TurnDead {
			rt.log.Error("giving up on a turn that keeps dying",
				zap.String("event", t.EventID), zap.String("kind", t.EventKind),
				zap.Int("attempts", t.Attempt))
			builtin.PushAppNotification(rt.store, t.AgentID, "alert",
				"A task was abandoned",
				fmt.Sprintf("%s kept failing across %d restarts and will not be retried.", t.EventKind, t.Attempt))
			continue
		}
		var evt bus.Event
		if err := json.Unmarshal([]byte(t.EventJSON), &evt); err != nil {
			rt.log.Warn("interrupted turn has an unreadable event", zap.String("event", t.EventID))
			continue
		}
		a, ok := rt.agents.Get(t.AgentID)
		if !ok {
			continue
		}
		// A turn that already spoke is finished, whatever the journal says.
		//
		// Resuming exists so a turn interrupted mid-flight is not lost. It
		// cannot tell "died before doing anything" from "died after replying",
		// and replaying the second kind is what put four near-identical
		// messages in one contact's chat, answered the operator's message a
		// second time in different words an hour later, and created five
		// overlapping reminder jobs from a single request. The loop tier
		// already refuses to retry a run that has messaged somebody; agent
		// turns must refuse for the same reason. A person can repeat
		// themselves; they cannot un-receive a duplicate.
		if rt.turnAlreadySpoke(t) {
			rt.log.Info("not resuming a turn that already replied",
				zap.String("event", t.EventID), zap.String("kind", t.EventKind))
			_ = rt.store.FinishAgentTurn(t.EventID, store.TurnOK, "")
			continue
		}
		rt.log.Info("resuming a turn the daemon died during",
			zap.String("event", t.EventID), zap.String("kind", t.EventKind), zap.Int("attempt", t.Attempt+1))
		if rt.openTurn(evt) {
			if err := a.Send(evt); err != nil {
				_ = rt.store.FinishAgentTurn(evt.ID, store.TurnFailed, err.Error())
			}
		}
	}
}

// maxTurnAttempts bounds how many restarts one event may survive. An event that
// crashes the daemon three times is not going to work the fourth.
const maxTurnAttempts = 3

// startDeadLetterAlerts: a dead letter means something that should have
// happened did not, so it gets the same alert a dead loop run does.
func (rt *KarmaxRuntime) startDeadLetterAlerts() {
	rt.bus.OnDeadLetter(func(d store.DeadLetter) {
		builtin.PushAppNotification(rt.store, rt.loopDefaultAgent, "alert",
			fmt.Sprintf("Event %q was never processed", d.Kind),
			truncErr(fmt.Sprintf("%s gave up after %d attempts: %s",
				d.Subscriber, d.Attempts, d.LastError), 300))
	})
}

func (rt *KarmaxRuntime) startCriticalAlertLoop(ctx context.Context) {
	rt.bus.Consume(ctx, bus.SubCritical, []bus.EventKind{bus.EventSystemCritical},
		func(_ context.Context, evt bus.Event) error {
			if attempted, _ := evt.Payload["alternative_alert_attempted"].(bool); attempted {
				return nil
			}
			// An alert is a "something is wrong now" signal. Re-sent days later
			// it is noise about a condition that has long since resolved or been
			// superseded — and a subscriber catching up on a backlog sends every
			// one of them, which the operator experiences as the alert spamming.
			if age, stale := staleEvent(evt); stale {
				rt.log.Warn("skipped a critical alert too old to send",
					zap.String("event", evt.ID), zap.Duration("age", age.Round(time.Minute)))
				return nil
			}
			return rt.raiseCritical(evt)
		})
}

// raiseCritical tells the operator about a critical system event: always on
// record in the app, and on WhatsApp (or another live channel) when the
// operating policy sends urgent items there — a critical alert is urgent by
// nature.
func (rt *KarmaxRuntime) raiseCritical(evt bus.Event) error {
	message, _ := evt.Payload["message"].(string)
	if message == "" {
		message = "KARMAX critical system event"
	}
	builtin.PushAppNotificationQuiet(rt.store, evt.AgentID, "alert", "Critical KARMAX alert", message)
	if !rt.policy.ToWhatsApp(policy.Urgent) {
		return nil
	}
	primary, _ := evt.Payload["karmax_channel_id"].(string)
	// Returned, not logged: an undelivered critical alert is what the
	// retry and dead-letter path is for.
	if err := rt.comms.AlertAlternative(evt.AgentID, primary, "Critical KARMAX alert: "+message); err != nil {
		return fmt.Errorf("alternative channel alert for %s: %w", evt.AgentID, err)
	}
	return nil
}

func (rt *KarmaxRuntime) publishCritical(agentID, message string, fields map[string]any) {
	payload := map[string]any{
		"severity": "critical",
		"message":  message,
	}
	for k, v := range fields {
		payload[k] = v
	}
	rt.bus.Publish(bus.NewEvent(bus.EventSystemCritical, agentID, payload))
}

func (rt *KarmaxRuntime) printBanner() {
	agentCount := len(rt.agents.List())
	toolCount := len(rt.tools.List())
	mcpToolCounts := rt.mcpBridge.ServerToolCount()
	totalMCPTools := 0
	for _, c := range mcpToolCounts {
		totalMCPTools += c
	}
	commsCount := len(rt.comms.List())

	fmt.Println()
	fmt.Println("  karmax v0.2.0  |  data:", rt.cfg.Karmax.DataDir, " |  db:", rt.store.Kind())
	fmt.Println("  -------------------------------------------------")
	fmt.Printf("  + %s store    (migrations applied)\n", rt.store.Kind())
	fmt.Printf("  + MCP bridge      (%d servers)\n", len(rt.cfg.MCPs))
	fmt.Printf("  + Tool registry   (%d built-in + %d MCP tools)\n", toolCount, totalMCPTools)
	fmt.Printf("  + Memory manager  (%d namespaces)\n", len(rt.memory.List()))
	fmt.Printf("  + Comms channels  (%d channels)\n", commsCount)
	fmt.Printf("  + %d agents loaded\n", agentCount)

	for _, a := range rt.agents.List() {
		snap := a.Snapshot()
		triggers := ""
		if len(snap.Def.Triggers.Webhooks) > 0 {
			triggers += fmt.Sprintf("webhooks%v", snap.Def.Triggers.Webhooks)
		}
		if len(snap.Def.Triggers.Schedules) > 0 {
			for _, s := range snap.Def.Triggers.Schedules {
				triggers += fmt.Sprintf(" cron[%s]", s.Cron)
			}
		}
		if snap.Def.Triggers.RunOnStart {
			triggers += " run_on_start"
		}
		if triggers == "" {
			triggers = "manual"
		}
		fmt.Printf("    > %-18s [%s]   (%s)\n", snap.ID, snap.Status, triggers)
	}

	fmt.Printf("  + Scheduler        (%d jobs)\n", len(rt.scheduler.ListJobs()))
	if rt.cfg.Webhooks.Enabled {
		fmt.Printf("  + Webhook server   http://%s:%d\n", rt.cfg.Webhooks.Host, rt.cfg.Webhooks.Port)
	}
	if rt.cfg.API.Enabled {
		fmt.Printf("  + API server       http://%s:%d  (phone app)\n", rt.cfg.API.Host, rt.cfg.API.Port)
	}
	fmt.Println("  -------------------------------------------------")
	fmt.Println("  karmax is running. Press Ctrl+C to stop.")
	fmt.Println()
}

func registerBuiltinTools(reg *tools.Registry) {
	reg.Register(&builtin.HTTPTool{})
	reg.Register(&builtin.ShellTool{})
	reg.Register(&builtin.FileReadTool{})
	reg.Register(&builtin.FileWriteTool{})
	reg.Register(&builtin.FileListTool{})
	reg.Register(&builtin.EmailTool{})
	reg.Register(&builtin.NotifyTool{})
}

// splitCSV splits a comma-separated env value into trimmed non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func dndEnabled(settings map[string]string) bool {
	for _, key := range []string{"dnd", "do_not_disturb", "do-not-disturb"} {
		switch strings.ToLower(strings.TrimSpace(settings[key])) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func configToAgentDef(cfg config.AgentDefConfig) agent.AgentDef {
	def := agent.AgentDef{
		ID:                   cfg.ID,
		Name:                 cfg.Name,
		Description:          cfg.Description,
		Tags:                 cfg.Tags,
		SystemPrompt:         cfg.SystemPrompt,
		Model:                cfg.Model,
		Provider:             cfg.Provider,
		Temperature:          cfg.Temperature,
		MaxTokens:            cfg.MaxTokens,
		Tools:                cfg.Tools,
		CoreTools:            cfg.CoreTools,
		MCPs:                 cfg.MCPs,
		HarnessKind:          cfg.HarnessKind,
		RestartPolicy:        agent.RestartPolicy(cfg.RestartPolicy),
		MaxRestarts:          cfg.MaxRestarts,
		Env:                  cfg.Env,
		CompactionThreshold:  cfg.CompactionThreshold,
		CompactionKeepRecent: cfg.CompactionKeepRecent,
		MemoryModelCfg: agent.ModelConfig{
			Model:    cfg.MemoryModel.Model,
			Provider: cfg.MemoryModel.Provider,
		},
		SummaryModelCfg: agent.ModelConfig{
			Model:    cfg.SummaryModel.Model,
			Provider: cfg.SummaryModel.Provider,
		},
		VoiceModelCfg: agent.ModelConfig{
			Model:    cfg.VoiceModel.Model,
			Provider: cfg.VoiceModel.Provider,
		},
		VoiceFallbackModels: voiceFallbackDefs(cfg.VoiceFallbacks),
		VoiceBedrockKeyEnv:  cfg.VoiceBedrockKeyEnv,
		VoiceBedrockRegion:  cfg.VoiceBedrockRegion,
		VoiceBudgetUSD:      cfg.VoiceBudgetUSD,
		Memory: agent.AgentMemoryConfig{
			Enabled:    cfg.Memory.Enabled,
			Namespace:  cfg.Memory.Namespace,
			MaxEntries: cfg.Memory.MaxEntries,
			Summarize:  cfg.Memory.Summarize,
		},
		HealthCheck: agent.HealthCheckConfig{
			IntervalSeconds: cfg.HealthCheck.IntervalSeconds,
			ToolName:        cfg.HealthCheck.ToolName,
			ToolInput:       cfg.HealthCheck.ToolInput,
			PingPrompt:      cfg.HealthCheck.PingPrompt,
		},
		Triggers: agent.AgentTriggers{
			Webhooks:   cfg.Triggers.Webhooks,
			Events:     cfg.Triggers.Events,
			RunOnStart: cfg.Triggers.RunOnStart,
		},
	}

	for _, s := range cfg.Triggers.Schedules {
		def.Triggers.Schedules = append(def.Triggers.Schedules, agent.ScheduleTrigger{
			Cron:    s.Cron,
			Payload: s.Payload,
		})
	}

	for _, fb := range cfg.FallbackModels {
		def.FallbackModels = append(def.FallbackModels, agent.FallbackModelDef{
			Provider: fb.Provider,
			Model:    fb.Model,
		})
	}

	return def
}

// defaultNamespace is the memory namespace the first agent uses, which is what
// a GitLoom namespace defaults to when none is configured explicitly.
func defaultNamespace(cfg *config.KarmaxConfig) string {
	if len(cfg.Agents) == 0 {
		return "karmax"
	}
	if ns := cfg.Agents[0].Memory.Namespace; ns != "" {
		return ns
	}
	return cfg.Agents[0].ID
}

// unconfiguredChannel reports a channel that is declared but has no credentials
// yet, so it can be skipped rather than started and failed.
//
// A channel in karmax.yaml with its token still unset is somebody who has not
// finished setting it up — not a fault. Started anyway it fails, and a failed
// channel publishes a critical AND sends an alert down another channel, so
// three placeholder blocks become three alerts on the operator's phone about
// work they had not begun. It still appears in `karmax integrations` as not
// connected, which is where "you have not finished this" belongs.
func unconfiguredChannel(log *zap.Logger, id, kind string, tokens ...string) bool {
	for _, t := range tokens {
		if strings.TrimSpace(t) != "" {
			continue
		}
		log.Info("channel is configured but has no credentials yet; not starting it",
			zap.String("id", id), zap.String("type", kind),
			zap.String("next", "karmax login "+id))
		return true
	}
	return false
}

// turnAlreadySpoke reports whether this turn put a message in the chat it was
// answering before the daemon died.
//
// The comms store is the evidence: every outbound message is recorded there
// with its target and time, so an outbound message to the triggering chat
// timestamped after the turn began can only have come from this turn.
//
// Only chat-shaped turns can be judged this way. A scheduled job or a timer
// has no chat to check, and those are safe to retry — nobody has heard them.
func (rt *KarmaxRuntime) turnAlreadySpoke(t store.AgentTurn) bool {
	if t.EventKind != string(bus.EventCommsMessage) {
		return false
	}
	var evt bus.Event
	if json.Unmarshal([]byte(t.EventJSON), &evt) != nil {
		return false
	}
	chatID, _ := evt.Payload["channel_id"].(string)
	if strings.TrimSpace(chatID) == "" {
		return false
	}
	msgs, err := rt.store.ListChannelMessages(chatID, 20)
	if err != nil {
		// Unreadable history is not evidence of silence, but it is not evidence
		// of speech either. Resuming is the behaviour that loses nothing.
		return false
	}
	for _, m := range msgs {
		if m.Direction == string(comms.Outbound) && !m.CreatedAt.Before(t.StartedAt) {
			return true
		}
	}
	return false
}

// voiceFallbackDefs converts the configured call fallbacks.
func voiceFallbackDefs(in []config.FallbackModelConfig) []agent.FallbackModelDef {
	out := make([]agent.FallbackModelDef, 0, len(in))
	for _, f := range in {
		out = append(out, agent.FallbackModelDef{Provider: f.Provider, Model: f.Model})
	}
	return out
}
