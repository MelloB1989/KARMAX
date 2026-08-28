package api

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	googleconn "github.com/MelloB1989/karmax/internal/connectors/google"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// Per-employee OAuth: the org registers one app, each person authorises their
// own account against it.
//
// Two halves, with deliberately different auth:
//
//   - /connect is called by the console with a session, and starts the flow FOR
//     THE SIGNED-IN PERSON. The member comes from their session and never from
//     the request body, or one employee could start a flow that binds their
//     Google account to another employee's name.
//   - /callback is opened by Google in a browser, carrying no session at all.
//     Its security is the state token: single-use, short-lived, and the only
//     thing that says which member this consent belongs to.

// oauthStateTTL is how long someone has to finish a consent screen.
const oauthStateTTL = 15 * time.Minute

// handleConnectStart begins an OAuth authorisation for the signed-in operator.
func (s *ConsoleServer) handleConnectStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	member := consoleUser(r).Member

	c, ok := s.connectorByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such connector"})
		return
	}
	if !c.Manifest().PerUser {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": id + " is configured once for the whole install, not per person",
		})
		return
	}

	cred, err := s.store.Credential(id)
	if err != nil || cred == nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "an admin has to set up the " + id + " OAuth app before anyone can connect",
		})
		return
	}
	known := connectorkit.Credentials{Config: cred.Config, AccessToken: cred.AccessToken}

	redirect := s.oauthCallbackURL(id)
	if redirect == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this server does not know its own public address — set console.public_url in karmax.yaml",
		})
		return
	}

	state, err := s.store.CreateOAuthStateFor(id, member, "", "", "connect", oauthStateTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	authURL, err := s.authCodeURL(id, known, redirect, state)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", member, "console.connector.connect_start", id, "", "")
	// Returned rather than redirected: the caller is fetch(), and a 302 to
	// accounts.google.com would be followed by the fetch and fail CORS instead
	// of moving the person's browser.
	writeJSON(w, http.StatusOK, map[string]any{"authorize_url": authURL, "expires_in": int(oauthStateTTL.Seconds())})
}

// authCodeURL builds a connector's consent URL.
func (s *ConsoleServer) authCodeURL(id string, cr connectorkit.Credentials, redirect, state string) (string, error) {
	switch id {
	case "google":
		return googleconn.AuthCodeURL(cr, redirect, state)
	default:
		return "", fmt.Errorf("%s does not support connecting an individual account yet", id)
	}
}

// oauthCallbackURL is where the provider sends the person back.
//
// Under /api/console/ on purpose: that prefix is already the one routed to this
// server from the outside, so the callback needs no separate network path and
// cannot be forgotten when one is added.
func (s *ConsoleServer) oauthCallbackURL(id string) string {
	base := s.publicBase()
	if base == "" {
		return ""
	}
	return base + "/api/console/oauth/" + id + "/callback"
}

// handleOAuthCallback receives the provider's redirect.
//
// Unauthenticated by necessity — a browser arriving from Google carries no
// bearer token — so the state token is the whole of the security: single-use,
// expiring, and the only thing that names the member.
func (s *ConsoleServer) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()

	if e := q.Get("error"); e != "" {
		// A refusal is not a failure. Say so plainly rather than showing
		// someone a red error page for having clicked Cancel.
		s.oauthPage(w, http.StatusOK, "Not connected",
			"You cancelled the sign-in, so nothing was connected. You can close this tab.")
		return
	}

	pending, err := s.store.RedeemOAuthState(q.Get("state"))
	if err != nil {
		s.oauthPage(w, http.StatusBadRequest, "That link didn't work", html.EscapeString(err.Error()))
		return
	}
	if pending.Connector != id {
		s.oauthPage(w, http.StatusBadRequest, "That link didn't work",
			"This authorisation was started for a different connector.")
		return
	}

	code := q.Get("code")
	if code == "" {
		s.oauthPage(w, http.StatusBadRequest, "That link didn't work", "Google sent no authorisation code.")
		return
	}

	// Branch on what the authorisation was started FOR. Without this, a link
	// generated for signing in could be redeemed as a mailbox connection —
	// binding a stranger's Google account to whichever member the state named.
	if pending.Purpose == googleLoginPurpose {
		s.finishGoogleSignIn(w, r, code)
		return
	}

	cred, err := s.store.Credential(id)
	if err != nil || cred == nil {
		s.oauthPage(w, http.StatusConflict, "Not set up",
			"The "+html.EscapeString(id)+" OAuth app is no longer configured on this server.")
		return
	}
	appCreds := connectorkit.Credentials{Config: cred.Config, AccessToken: cred.AccessToken}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	ts, err := googleconn.Exchange(ctx, appCreds, code, s.oauthCallbackURL(id))
	if err != nil {
		s.log.Warn("oauth exchange failed",
			zap.String("connector", id), zap.String("member", pending.Member), zap.Error(err))
		s.oauthPage(w, http.StatusBadGateway, "Could not finish connecting", html.EscapeString(err.Error()))
		return
	}

	expiry := ts.Expiry
	if err := s.store.SaveUserCredential(store.UserCredential{
		Connector: id, Member: pending.Member, Account: ts.Email,
		AccessToken: ts.AccessToken, RefreshToken: ts.RefreshToken,
		Scopes: ts.Scopes, ExpiresAt: &expiry,
	}); err != nil {
		s.oauthPage(w, http.StatusInternalServerError, "Could not save the connection", html.EscapeString(err.Error()))
		return
	}

	// The account is recorded, the tokens are not: an audit line is read by
	// people and kept for a long time.
	if aerr := s.store.AppendAudit(store.AuditEvent{
		ActorKind: "human", ActorID: pending.Member,
		Verb: "console.connector.connected", Target: id, Detail: "connected " + ts.Email,
	}); aerr != nil {
		s.log.Error("failed to record a connector connection", zap.Error(aerr))
	}
	s.log.Info("connector connected for a member",
		zap.String("connector", id), zap.String("member", pending.Member))

	who := ts.Email
	if who == "" {
		who = "your account"
	}
	s.oauthPage(w, http.StatusOK, "Connected",
		"KARMAX can now act as "+html.EscapeString(who)+" for "+html.EscapeString(pending.Member)+
			". You can close this tab and go back to the console.")
}

// handleConnections lists who has connected a per-user connector.
func (s *ConsoleServer) handleConnections(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// 404 for an install-wide connector rather than an empty list: the console
	// decides whether to show a "connect your account" panel from this call, and
	// an empty list would render one on Slack, where it means nothing.
	c, ok := s.connectorByID(id)
	if !ok || !c.Manifest().PerUser {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "this connector is configured once for the whole install, not per person",
		})
		return
	}

	list, err := s.store.ListUserCredentials(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	me := consoleUser(r)
	out := make([]map[string]any, 0, len(list))
	mine := false
	for _, c := range list {
		if c.Member == me.Member {
			mine = true
		}
		// Tokens are never in this response; a list of names does not need them.
		out = append(out, map[string]any{
			"member":       c.Member,
			"account":      c.Account,
			"connected_at": rfc3339(c.UpdatedAt),
			"expires_at":   rfc3339Ptr(derefTime(c.ExpiresAt)),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out, "self_connected": mine})
}

// handleDisconnect removes one person's authorisation.
//
// An operator may disconnect themselves; removing somebody else's is an admin
// action, because it takes away access they granted.
func (s *ConsoleServer) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	me := consoleUser(r)

	target := strings.TrimSpace(r.URL.Query().Get("member"))
	if target == "" {
		target = me.Member
	}
	if target != me.Member && !roleAtLeast(me.Role, "admin") {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "disconnecting someone else's account requires the admin role",
		})
		return
	}

	if err := s.store.DeleteUserCredential(id, target); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.audit(r, "human", me.Member, "console.connector.disconnected", id, "", "disconnected "+target)
	writeJSON(w, http.StatusOK, map[string]any{"disconnected": true, "member": target})
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// oauthPage renders the plain page a person lands on after consenting.
//
// Self-contained HTML rather than a redirect into the SPA: the console is
// served from a different origin than this API, and bouncing someone through a
// route that may not exist is a worse ending than a sentence telling them it
// worked.
func (s *ConsoleServer) oauthPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>%s</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         font:16px/1.6 ui-sans-serif,system-ui,-apple-system,sans-serif;
         background:#fafafa; color:#18181b; padding:2rem; }
  @media (prefers-color-scheme: dark) { body { background:#09090b; color:#fafafa; } }
  main { max-width:34rem; text-align:center; }
  h1 { font-size:1.25rem; margin:0 0 .5rem; letter-spacing:-0.01em; }
  p { margin:0; opacity:.75; }
</style></head>
<body><main><h1>%s</h1><p>%s</p></main></body></html>`, title, title, body)
}
