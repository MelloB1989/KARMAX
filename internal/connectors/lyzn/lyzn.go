// Package lyzn connects KARMAX to LYZN, so a promise somebody made out loud
// is kept by the machine they made it on.
//
// LYZN is a pendant and a phone app. It hears a conversation, notices the
// commitments in it, and shows them to the person who made them. When they
// approve one, somebody still has to do it — and LYZN cannot, because doing it
// takes a shell, a filesystem and a set of signed-in accounts, none of which
// exist in a cloud that only heard the sentence. KARMAX is where they are.
//
// **The laptop asks.** There is no inbound path: a machine behind a home
// router has no address LYZN could call, and karmax-desktop pins the API to
// loopback. So this connector polls — it says it is alive, asks what has been
// approved, and posts back what happened. That is the whole connection.
//
// **Paired, not keyed.** LYZN mints a six-character code that lives five
// minutes and is spent by being used; redeeming it yields a bearer token bound
// to one machine. So the thing a person carries from a phone screen to a
// laptop is short, typable and worthless a minute later, and the long-lived
// credential is never seen by anybody — the software fetches it. That
// redemption is `CompleteCredentials`' job, for the same reason GitHub's
// installation id is discovered there rather than asked for.
//
// **Nothing here is handed work that was not approved.** The work endpoint
// only ever answers with tasks a person approved in the app, on an account
// whose plan carries automation. A daemon cannot approve its own work, and
// this connector has no call that would let it.
package lyzn

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// ID is the manifest id, and the key everything else is stored under.
const ID = "lyzn"

// Config keys. Named here because three files write them and a typo in one
// copy is a connector that pairs and then cannot find its own token.
const (
	// keyCode is the six characters a person types in. Spent on first use.
	keyCode = "pairing_code"
	// keyToken is the bearer credential, fetched by redeeming the code.
	keyToken = "token"
	// keyDaemonID is which machine LYZN thinks this is, for support and for
	// the unpair row in the app.
	keyDaemonID = "daemon_id"
	// keyAPI overrides the endpoint. Present for a preview stack, and for
	// the tests, which point it at a local stub.
	keyAPI = "api_url"
	// keyName is what the app calls this machine in its list.
	keyName = "machine_name"
)

// Connector is one paired LYZN account.
//
// Install-wide rather than per-user (`PerUser: false`), and that is forced by
// the shape of the thing: a pairing code is redeemed once into one token for
// one machine, and the poller that uses it runs on a ticker with nobody
// acting. A per-user connector refuses every call that has no actor behind
// it, which would be all of them.
type Connector struct{}

func New() *Connector { return &Connector{} }

func (c *Connector) Manifest() connectorkit.Manifest {
	return connectorkit.Manifest{
		ID:   ID,
		Name: "LYZN",
		Description: "Carry out the commitments LYZN heard you make. It polls for " +
			"tasks you approved in the app, runs them here, and posts back what " +
			"happened so LYZN can print the receipt.",
		Capabilities: []string{"http:api.lyzn.ai"},
		Config: []connectorkit.ConfigField{
			{
				Key:         keyCode,
				Description: "The six-character pairing code from the LYZN app",
				Help:        "In LYZN: Settings → Laptop daemon → Pair a laptop. It is good for five minutes and one machine.",
				Required:    true,
			},
			// Written by CompleteCredentials, never by a person. Secret so the
			// console shows "set" instead of echoing it back into a page.
			{
				Key:         keyToken,
				Description: "Written automatically when the pairing code is redeemed",
				Secret:      true,
			},
			{
				Key:         keyDaemonID,
				Description: "Written automatically — which machine LYZN thinks this is",
			},
			{
				Key:         keyName,
				Description: "What this machine is called in the LYZN app",
				Help:        "Defaults to this computer's hostname.",
			},
			{
				Key:         keyAPI,
				Description: "LYZN API root",
				Default:     defaultAPI,
			},
		},
	}
}

// Auth is a paste, as far as KARMAX is concerned.
//
// What the operator supplies is a short-lived code rather than a key, but the
// console's paste-a-credential form is exactly the right surface for it, and
// declaring OAuth2 would offer a browser round trip LYZN does not have. The
// field named here is the token because that is what ends up authenticating
// every request; the code is the thing that fetches it.
func (c *Connector) Auth() connectorkit.AuthMethod {
	return connectorkit.AuthMethod{Kind: connectorkit.AuthAPIKey, APIKeyField: keyToken}
}

// Health is also the heartbeat, which is not a coincidence.
//
// LYZN decides a machine is asleep after three missed beats, and the app's
// settings screen shows exactly that. A health check that beat nothing would
// answer "reachable" here while the phone said the laptop was gone, so the one
// request answers both questions: it is the cheapest call in the API, it
// records the machine as present, and it comes back with how much work is
// waiting.
//
// Safe with nothing stored, because the prober calls it that way.
func (c *Connector) Health(ctx context.Context, cr connectorkit.Credentials) error {
	if strings.TrimSpace(token(cr)) == "" {
		if strings.TrimSpace(cr.Get(keyCode)) != "" {
			return fmt.Errorf("lyzn: the pairing code has not been redeemed yet — run the health check again to finish pairing")
		}
		return fmt.Errorf("lyzn: not paired — get a code from the LYZN app (Settings → Laptop daemon → Pair a laptop) and enter it here")
	}
	_, err := beat(ctx, cr)
	return err
}

// ValidateCredentials implements connectorkit.CredentialValidator.
//
// Shape only, and offline on purpose: telling somebody they have typed five
// characters while the code is still on the screen in front of them beats a
// redemption that fails a click later, by which time the honest report is
// indistinguishable from "that code expired".
func (c *Connector) ValidateCredentials(cr connectorkit.Credentials) error {
	raw := strings.TrimSpace(cr.Get(keyCode))
	if raw == "" {
		// Whether a required field may be empty is the form's question, not
		// this one's — and once paired, it is empty for good.
		return nil
	}
	if _, err := normaliseCode(raw); err != nil {
		return err
	}
	return nil
}

// CompleteCredentials implements connectorkit.CredentialCompleter: it redeems
// the code for a token and hands both halves back for KARMAX to store.
//
// The code is returned **empty**. It is single-use, and a spent code left in
// the configuration invites a later save to redeem it again, fail, and read as
// a broken pairing when the truth is that it worked the first time.
//
// Silent when there is nothing to do, so re-running a health check on a paired
// install does not attempt a second redemption.
func (c *Connector) CompleteCredentials(ctx context.Context, cr connectorkit.Credentials) (map[string]string, error) {
	raw := strings.TrimSpace(cr.Get(keyCode))
	if raw == "" || strings.TrimSpace(token(cr)) != "" {
		return nil, nil
	}
	code, err := normaliseCode(raw)
	if err != nil {
		return nil, err
	}

	this := machine(cr)
	var out struct {
		DaemonID string `json:"daemonId"`
		Token    string `json:"token"`
		Name     string `json:"name"`
	}
	// The one call in this package that carries no credential: the code is
	// the credential, and it is spent by being presented.
	err = send(ctx, cr, http.MethodPost, "/daemons/claim", map[string]any{
		"code":         code,
		"name":         this.Name,
		"hostname":     this.Hostname,
		"os":           this.OS,
		"version":      this.Version,
		"capabilities": this.Capabilities,
	}, &out, withoutToken())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Token) == "" {
		return nil, fmt.Errorf("lyzn: the pairing was accepted but no token came back — get a new code and try again")
	}

	return map[string]string{
		keyToken:    out.Token,
		keyDaemonID: out.DaemonID,
		keyName:     out.Name,
		keyCode:     "",
	}, nil
}

// SetupSteps implements connectorkit.SetupGuide.
//
// Two of these four steps have no field beside them, which is the whole reason
// this exists: that the code is redeemed by the health check rather than by
// saving, and that pollers are wired at start-up, are the two things somebody
// cannot discover from a form and will otherwise sit waiting for.
func (c *Connector) SetupSteps(cr connectorkit.Credentials, _ string) []connectorkit.SetupStep {
	paired := strings.TrimSpace(token(cr)) != ""
	return []connectorkit.SetupStep{
		{
			Title: "Get a code from the LYZN app",
			Body:  "Settings → Laptop daemon → Pair a laptop. Six characters, good for five minutes and one machine.",
		},
		{
			Title: "Enter it below and save",
			Body:  "Nothing is sent to LYZN yet — saving only writes the code down.",
		},
		{
			Title: "Run the health check",
			Body:  "That is what redeems the code for this machine's token. The code is spent here, and the app's list should show this machine straight after.",
			Done:  &paired,
		},
		{
			Title: "Restart KARMAX",
			Body:  "Polling sources are wired when KARMAX starts, so approved tasks begin arriving after the next restart.",
		},
	}
}
