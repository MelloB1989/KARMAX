package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	googleconn "github.com/MelloB1989/karmax/internal/connectors/google"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
	"go.uber.org/zap"
)

// Signing in with the Google account you already have.
//
// Two rules decide who gets in, and both matter:
//
//  1. An email on a console account is what a Google identity matches. Being
//     visible in a connected Slack does not make you a console user, which is
//     why this does NOT consult the directory table.
//  2. Auto-provisioning happens ONLY when the connector declares a hosted
//     domain. Without one, "sign in with Google" would mean anybody on earth
//     with a Google account, and the first person to find the URL would get an
//     account on someone else's install.
//
// A provisioned account gets `viewer` — read everything, change nothing. An
// admin promotes from there. Handing out operator on the strength of an email
// domain would let anyone in the company approve an agent's actions.

const googleLoginPurpose = "login"

// googleSignInAvailable reports whether the connector is set up enough to
// offer it, and whether new people can provision themselves.
func (s *ConsoleServer) googleSignInAvailable() (enabled bool, autoProvisionDomain string) {
	if s.conns == nil || s.store == nil {
		return false, ""
	}
	if _, ok := s.connectorByID("google"); !ok {
		return false, ""
	}
	cred, err := s.store.Credential("google")
	if err != nil || cred == nil {
		return false, ""
	}
	if strings.TrimSpace(cred.Config["client_id"]) == "" ||
		strings.TrimSpace(cred.Config["client_secret"]) == "" {
		return false, ""
	}
	return true, strings.ToLower(strings.TrimSpace(cred.Config["hosted_domain"]))
}

// handleGoogleSignInStatus tells the login page whether to offer the button.
func (s *ConsoleServer) handleGoogleSignInStatus(w http.ResponseWriter, r *http.Request) {
	enabled, domain := s.googleSignInAvailable()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": enabled,
		// Shown on the button as "Continue with Google (acme.com)", so someone
		// with a personal account knows before clicking that it will be
		// refused.
		"domain": domain,
	})
}

// handleGoogleSignInStart begins a sign-in.
//
// Unauthenticated by necessity — the whole point is that the caller has no
// session yet — so it must not accept any identity from the request. The
// member is decided at the callback, from what Google says, and nothing here
// is trusted.
func (s *ConsoleServer) handleGoogleSignInStart(w http.ResponseWriter, r *http.Request) {
	enabled, _ := s.googleSignInAvailable()
	if !enabled {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "signing in with Google needs an admin to set up the Google connector first",
		})
		return
	}

	cred, err := s.store.Credential("google")
	if err != nil || cred == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the Google connector is not configured"})
		return
	}
	redirect := s.oauthCallbackURL("google")
	if redirect == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "this server does not know its own public address — set console.public_url",
		})
		return
	}

	// No member: there is nobody signed in to attribute this to yet.
	state, err := s.store.CreateOAuthStateFor("google", "", "", "", googleLoginPurpose, oauthStateTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// Sign-in scopes only: this consent screen establishes who you are, and
	// must not ask for the mailbox access the connector needs.
	authURL, err := googleconn.AuthCodeURLForSignIn(
		connectorkit.Credentials{Config: cred.Config}, redirect, state)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorize_url": authURL})
}

// finishGoogleSignIn completes a sign-in from the OAuth callback.
func (s *ConsoleServer) finishGoogleSignIn(w http.ResponseWriter, r *http.Request, code string) {
	cred, err := s.store.Credential("google")
	if err != nil || cred == nil {
		s.oauthPage(w, http.StatusConflict, "Not set up",
			"The Google connector is no longer configured on this server.")
		return
	}
	appCreds := connectorkit.Credentials{Config: cred.Config}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Exchange, not ExchangeForSignIn: a sign-in does not need a refresh token,
	// and demanding one would refuse anybody who has authorised this app
	// before.
	ts, err := googleconn.ExchangeIdentity(ctx, appCreds, code, s.oauthCallbackURL("google"))
	if err != nil {
		s.log.Warn("google sign-in exchange failed", zap.Error(err))
		s.oauthPage(w, http.StatusBadGateway, "Could not sign you in", html.EscapeString(err.Error()))
		return
	}
	email := strings.ToLower(strings.TrimSpace(ts.Email))
	if email == "" {
		s.oauthPage(w, http.StatusBadGateway, "Could not sign you in",
			"Google did not say which account this is.")
		return
	}

	_, domain := s.googleSignInAvailable()
	if domain != "" && !strings.HasSuffix(email, "@"+domain) {
		// Checked here as well as via Google's `hd` parameter: `hd` is a UI
		// hint on the consent screen, not a guarantee about what comes back.
		s.log.Warn("google sign-in refused: wrong domain", zap.String("email", email))
		s.oauthPage(w, http.StatusForbidden, "Not your console",
			"This console only accepts "+html.EscapeString(domain)+" accounts.")
		return
	}

	u, found, err := s.store.ConsoleUserByEmail(email)
	if err != nil {
		s.oauthPage(w, http.StatusInternalServerError, "Could not sign you in", html.EscapeString(err.Error()))
		return
	}

	if !found {
		if domain == "" {
			// No domain restriction means no safe way to provision: anybody
			// with any Google account would get in.
			s.oauthPage(w, http.StatusForbidden, "No account here",
				"There is no console account for "+html.EscapeString(email)+
					". Ask an admin to add you, or sign in with a password.")
			return
		}
		u, err = s.provisionFromGoogle(email, ts.Name)
		if err != nil {
			s.oauthPage(w, http.StatusInternalServerError, "Could not create your account",
				html.EscapeString(err.Error()))
			return
		}
		s.log.Info("provisioned a console account from a google sign-in",
			zap.String("member", u.Member), zap.String("email", email))
	}

	sess, err := s.store.CreateConsoleSession(u.Member, s.sessionTTL)
	if err != nil {
		s.oauthPage(w, http.StatusInternalServerError, "Could not sign you in", html.EscapeString(err.Error()))
		return
	}
	if aerr := s.store.AppendAudit(store.AuditEvent{
		ActorKind: "human", ActorID: u.Member, Verb: "console.login.google",
		Target: u.Member, Decision: "allowed", Detail: email,
	}); aerr != nil {
		s.log.Error("failed to record a google sign-in", zap.Error(aerr))
	}

	// The token travels in the URL FRAGMENT, not the query string. A fragment
	// is never sent to a server and never lands in an access log or a Referer
	// header; a query parameter would put a live session token in both.
	base := s.publicBase()
	if base == "" {
		s.oauthPage(w, http.StatusOK, "Signed in",
			"You are signed in, but this server does not know its own address to send you back to.")
		return
	}
	http.Redirect(w, r, base+"/login#token="+url.QueryEscape(sess.Token), http.StatusFound)
}

// provisionFromGoogle creates an account for somebody in the org's domain.
//
// Always `viewer`: read everything, change nothing. An admin promotes from
// there. Granting operator on the strength of an email domain would let anyone
// in the company approve an agent's actions.
func (s *ConsoleServer) provisionFromGoogle(email, name string) (store.ConsoleUser, error) {
	member := memberIDFromEmail(email)
	if name == "" {
		name = member
	}
	// No password: the account exists to be signed into with Google, and
	// giving it a password nobody chose would be a credential nobody guards.
	u, err := s.store.CreateConsoleUserWithEmail(member, name, "viewer", "", email)
	if err != nil {
		return store.ConsoleUser{}, err
	}
	return u, nil
}

// memberIDFromEmail derives a stable member id from an address.
func memberIDFromEmail(email string) string {
	local := email
	if i := strings.IndexByte(local, '@'); i > 0 {
		local = local[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == '+':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "user"
	}
	return out
}
