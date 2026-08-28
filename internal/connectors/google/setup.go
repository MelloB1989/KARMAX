package google

import (
	"strings"

	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// Setting up Google has one step that fails silently if you skip it: the
// redirect URI must be registered with the OAuth client, character for
// character. Google rejects a mismatch with "Error 400: redirect_uri_mismatch",
// which most people read as "something is wrong with the app".
//
// So the URI is the first thing on the page, with the value to copy, rather
// than a field nobody knew to ask for.

// SetupSteps implements connectorkit.SetupGuide.
//
// redirectURI is where Google sends people back after they consent.
func (c *Connector) SetupSteps(cr connectorkit.Credentials, redirectURI string) []connectorkit.SetupStep {
	yes, no := true, false

	configured := strings.TrimSpace(cr.Get("client_id")) != "" &&
		strings.TrimSpace(cr.Get("client_secret")) != ""
	done := &no
	if configured {
		done = &yes
	}

	steps := []connectorkit.SetupStep{
		{
			Title: "Create an OAuth client",
			Body: "Google Cloud console → APIs & Services → Credentials → Create credentials → " +
				"OAuth client ID. Application type must be WEB APPLICATION: the other types " +
				"cannot use a redirect URI, and Google will not let you paste one.",
			URL:  "https://console.cloud.google.com/apis/credentials",
			Done: done,
		},
	}

	if redirectURI != "" {
		steps = append(steps, connectorkit.SetupStep{
			Title: "Add this to Authorised redirect URIs",
			Body: "Paste it exactly — no trailing slash, https not http. Google compares it " +
				"character for character against what KARMAX sends, and a mismatch is refused " +
				"with \"Error 400: redirect_uri_mismatch\", which reads like a broken app rather " +
				"than a missing line.",
			Value: redirectURI,
		})
	} else {
		steps = append(steps, connectorkit.SetupStep{
			Title: "Set console.public_url first",
			Body: "This server does not know its own public address, so it cannot tell you the " +
				"redirect URI to register. Set console.public_url in karmax.yaml and reload — " +
				"until then no Google sign-in can work.",
		})
	}

	steps = append(steps,
		connectorkit.SetupStep{
			Title: "Enable the APIs you want",
			Body: "APIs & Services → Enabled APIs → the Gmail, Google Calendar, Google Drive and " +
				"Google Chat APIs. A scope granted for an API that is not enabled fails at the " +
				"first call, not at consent, so this is easy to miss.",
			URL: "https://console.cloud.google.com/apis/library",
		},
		connectorkit.SetupStep{
			Title: "Configure the consent screen",
			Body: "Internal, if this is a Workspace org — then only your own people can " +
				"authorise and no Google review is needed. External means anybody with a Google " +
				"account, and unverified apps are limited to test users you list by hand.",
			URL: "https://console.cloud.google.com/apis/credentials/consent",
		},
		connectorkit.SetupStep{
			Title: "Paste the client ID and secret below",
			Body: "Then each person connects their own account from this page. The org's app is " +
				"configured once; the mailbox access is per person.",
			Done: done,
		},
	)
	return steps
}
