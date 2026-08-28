package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/broker"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/connectors"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// The web console's HTTP surface.
//
// This runs on its OWN listener, not as extra routes on internal/api.Server.
// That server exposes POST /api/tools/{name}, which can invoke shell.exec —
// remote code execution by design, for an operator on a trusted network. The
// console is meant to be published. Putting both behind one port would mean
// publishing the first to publish the second, and no login is worth putting a
// shell on the open internet.
//
// So: this mux carries /api/console/* and the SPA, and nothing else. There is
// no route here that runs a command, and adding one would defeat the split.

// ConsoleServer serves the admin console and its API.
type ConsoleServer struct {
	addr      string
	cfg       *config.KarmaxConfig
	store     *store.Store
	agents    *agent.Registry
	scheduler *scheduler.Scheduler
	broker    *broker.Broker
	conns     *connectors.Host
	log       *zap.Logger
	httpSrv   *http.Server

	sessionTTL time.Duration
	distFS     fs.FS

	mux *http.ServeMux

	// decide executes an approval decision; injected by the runtime so this
	// package does not import the proposal machinery.
	decide func(id, decision, note, by string) error
	// runDirectorySync triggers a directory sync across connectors.
	runDirectorySync func(ctx context.Context) (int, []string, error)
	// generate is the model call the recipe builder uses; nil when no model is
	// configured, which the endpoint reports rather than pretending to work.
	generate func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// ConsoleDeps are the pieces the console needs that the api package cannot
// build for itself.
type ConsoleDeps struct {
	Store     *store.Store
	Agents    *agent.Registry
	Scheduler *scheduler.Scheduler
	Broker    *broker.Broker
	Conns     *connectors.Host
	Config    *config.KarmaxConfig
	Log       *zap.Logger

	Decide           func(id, decision, note, by string) error
	RunDirectorySync func(ctx context.Context) (int, []string, error)
	Generate         func(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// NewConsole builds the console server. distDir is the directory holding the
// built SPA; when it is missing the API still serves and the SPA routes 503,
// which is a far better failure than a binary that will not start.
func NewConsole(addr string, distDir string, d ConsoleDeps) *ConsoleServer {
	ttl := 12 * time.Hour
	if d.Config != nil && d.Config.Console.SessionHours > 0 {
		ttl = time.Duration(d.Config.Console.SessionHours) * time.Hour
	}

	s := &ConsoleServer{
		addr:             addr,
		cfg:              d.Config,
		store:            d.Store,
		agents:           d.Agents,
		scheduler:        d.Scheduler,
		broker:           d.Broker,
		conns:            d.Conns,
		log:              d.Log,
		sessionTTL:       ttl,
		decide:           d.Decide,
		runDirectorySync: d.RunDirectorySync,
		generate:         d.Generate,
	}
	// Both default to the console's own implementations; ConsoleDeps overrides
	// exist so a test can substitute them without a live model or agent.
	if s.decide == nil {
		s.decide = s.decideProposal
	}
	if s.generate == nil {
		s.generate = s.generateWithModel
	}

	if distDir != "" {
		if _, err := os.Stat(filepath.Join(distDir, "index.html")); err == nil {
			s.distFS = os.DirFS(distDir)
		} else {
			d.Log.Warn("console assets not found; the API will serve but the UI will not",
				zap.String("dist", distDir))
		}
	}

	mux := http.NewServeMux()

	// Auth bootstrap — the only routes reachable without a session.
	mux.HandleFunc("GET /api/console/auth/bootstrap-status", s.handleBootstrapStatus)
	mux.HandleFunc("POST /api/console/auth/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/console/auth/login", s.handleLogin)
	// Signing in with Google. Both unauthenticated by necessity — the caller
	// has no session yet, which is the point — so neither accepts an identity
	// from the request. Who you are is decided at the callback, from what
	// Google says.
	mux.HandleFunc("GET /api/console/auth/google/status", s.handleGoogleSignInStatus)
	mux.HandleFunc("POST /api/console/auth/google/start", s.handleGoogleSignInStart)
	mux.HandleFunc("GET /api/console/auth/me", s.session(s.handleMe))
	mux.HandleFunc("POST /api/console/auth/logout", s.session(s.handleLogout))

	mux.HandleFunc("GET /api/console/cases", s.session(s.handleCases))
	mux.HandleFunc("GET /api/console/cases/{id}", s.session(s.handleCaseDetail))

	mux.HandleFunc("GET /api/console/agents", s.session(s.handleConsoleAgents))
	mux.HandleFunc("GET /api/console/agents/{id}", s.session(s.handleConsoleAgentDetail))

	mux.HandleFunc("GET /api/console/recipes", s.session(s.handleConsoleRecipes))
	mux.HandleFunc("GET /api/console/recipes/{name}", s.session(s.handleConsoleRecipeDetail))
	mux.HandleFunc("POST /api/console/recipes/generate", s.role("operator", s.handleRecipeGenerate))
	mux.HandleFunc("POST /api/console/recipes", s.role("operator", s.handleRecipeCreate))
	mux.HandleFunc("PUT /api/console/recipes/{name}", s.role("operator", s.handleRecipeUpdate))
	mux.HandleFunc("POST /api/console/recipes/{name}/enable", s.role("operator", s.handleRecipeEnable))
	mux.HandleFunc("POST /api/console/recipes/{name}/disable", s.role("operator", s.handleRecipeDisable))

	mux.HandleFunc("GET /api/console/approvals", s.session(s.handleConsoleApprovals))
	mux.HandleFunc("POST /api/console/approvals/{id}/decision", s.role("operator", s.handleConsoleApprovalDecision))

	mux.HandleFunc("GET /api/console/audit", s.session(s.handleConsoleAudit))

	mux.HandleFunc("GET /api/console/connectors", s.session(s.handleConnectors))
	mux.HandleFunc("GET /api/console/connectors/{id}/setup", s.session(s.handleConnectorSetup))
	mux.HandleFunc("POST /api/console/connectors/{id}/credentials", s.role("admin", s.handleConnectorCredentials))
	mux.HandleFunc("POST /api/console/connectors/{id}/health-check", s.role("operator", s.handleConnectorHealthCheck))

	// Per-employee authorisation. Starting a flow needs a session, because the
	// member comes from it — never from the request body, or one person could
	// bind their Google account to another person's name.
	mux.HandleFunc("POST /api/console/connectors/{id}/connect", s.session(s.handleConnectStart))
	mux.HandleFunc("GET /api/console/connectors/{id}/connections", s.session(s.handleConnections))
	mux.HandleFunc("DELETE /api/console/connectors/{id}/connection", s.session(s.handleDisconnect))
	// The provider redirects a BROWSER here, carrying no bearer token, so this
	// one cannot require a session. Its security is the state token: single-use,
	// expiring, and the only thing that names the member.
	mux.HandleFunc("GET /api/console/oauth/{id}/callback", s.handleOAuthCallback)

	// Console accounts. Admin-only: handing out console access hands out the
	// ability to approve actions and read the org's memory.
	mux.HandleFunc("GET /api/console/users", s.role("admin", s.handleListUsers))
	mux.HandleFunc("POST /api/console/users", s.role("admin", s.handleCreateUser))
	mux.HandleFunc("PUT /api/console/users/{member}", s.role("admin", s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/console/users/{member}", s.role("admin", s.handleDeleteUser))
	// Not admin-gated at the route: anyone may change their OWN password, and
	// the handler enforces the difference.
	mux.HandleFunc("PUT /api/console/users/{member}/password", s.session(s.handleSetPassword))

	// The organisation the agents work for. Readable by anyone signed in;
	// writable by admins, because it is injected into every agent's prompt.
	mux.HandleFunc("GET /api/console/organisation", s.session(s.handleGetOrg))
	mux.HandleFunc("PUT /api/console/organisation", s.role("admin", s.handleSetOrg))

	mux.HandleFunc("GET /api/console/settings", s.session(s.handleSettings))
	mux.HandleFunc("PUT /api/console/settings/model/{id}", s.role("admin", s.handleSetModelProvider))
	mux.HandleFunc("PUT /api/console/settings/sandbox-token", s.role("admin", s.handleSetSandboxToken))
	mux.HandleFunc("POST /api/console/settings/directory/sync", s.role("admin", s.handleDirectorySync))
	mux.HandleFunc("PUT /api/console/settings/roles/{member}", s.role("admin", s.handleSetRole))

	// Everything else is the SPA.
	mux.HandleFunc("/", s.serveSPA)

	s.mux = mux
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Start runs the console server until ctx is cancelled.
func (s *ConsoleServer) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	go s.purgeSessions(ctx)

	s.log.Info("console server listening",
		zap.String("addr", s.addr), zap.Bool("ui", s.distFS != nil))
	if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop shuts the console server down.
func (s *ConsoleServer) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
}

// purgeSessions clears lapsed sessions hourly so the table does not grow
// without bound on a long-lived install.
func (s *ConsoleServer) purgeSessions(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.store.PurgeExpiredConsoleSessions(); err == nil && n > 0 {
				s.log.Debug("purged expired console sessions", zap.Int64("count", n))
			}
		}
	}
}

// securityHeaders sets the defensive headers a published console needs.
func (s *ConsoleServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// serveSPA serves the built console, falling back to index.html so that a
// client-side route survives a refresh.
func (s *ConsoleServer) serveSPA(w http.ResponseWriter, r *http.Request) {
	if s.distFS == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "console assets are not installed on this server"})
		return
	}

	clean := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if clean == "" {
		clean = "index.html"
	}
	if f, err := s.distFS.Open(clean); err == nil {
		if st, serr := f.Stat(); serr == nil && !st.IsDir() {
			f.Close()
			// Hashed assets are immutable; index.html must never be cached or
			// a deploy leaves browsers on the old bundle.
			if strings.HasPrefix(clean, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache, must-revalidate")
			}
			http.ServeFileFS(w, r, s.distFS, clean)
			return
		}
		f.Close()
	}

	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFileFS(w, r, s.distFS, "index.html")
}

// --- request helpers -------------------------------------------------------

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(dst)
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// rfc3339Ptr renders a nullable timestamp. The contract is explicit that a
// missing time is null and never an empty string.
func rfc3339Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func last4(secret string) string {
	if len(secret) < 4 {
		return ""
	}
	return secret[len(secret)-4:]
}

func parseRFC3339(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }

func nowUTC() time.Time { return time.Now().UTC() }
