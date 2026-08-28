package webhook

import "github.com/MelloB1989/karmax/internal/tracker"

// What KARMAX can receive, as a list something can render a dropdown from.
//
// Written down here rather than left for an operator to type, because typing
// the wrong signature header or the wrong event kind produces a webhook that
// silently never fires — and there is nothing on the screen to tell them which
// of the two they got wrong.

// Platform describes one service KARMAX knows how to decode.
type Platform struct {
	// ID is stored on the endpoint. Empty is the custom option.
	ID   string `json:"id"`
	Name string `json:"name"`
	// EventKind is what a recipe triggers on for this platform.
	EventKind string `json:"event_kind"`
	// SecretKind is how a delivery proves itself: "hmac" signs the body,
	// "token" sends the secret as-is, "none" is unverified.
	SecretKind string `json:"secret_kind"`
	// SignatureHeader is where the HMAC arrives, for the hmac kind.
	SignatureHeader string `json:"signature_header,omitempty"`
	// Fields are what the normalised event carries, so whoever writes the
	// recipe knows what they can read without guessing.
	Fields []string `json:"fields,omitempty"`
	// SetupHint is the one sentence worth saying at the moment of creating it.
	SetupHint string `json:"setup_hint"`
}

// trackerFields are what every normalised tracker event carries.
var trackerFields = []string{"source", "kind", "summary", "url", "assignee", "body", "actor"}

// Platforms is everything offerable, custom last.
//
// The three tracker platforms share one normalised event on purpose: a recipe
// written for "an issue changed" should not need rewriting because the team
// moved from Jira to YouTrack.
func Platforms() []Platform {
	return []Platform{
		{
			ID: string(tracker.GitHub), Name: "GitHub", EventKind: string(tracker.EventKind),
			SecretKind: "hmac", SignatureHeader: "X-Hub-Signature-256", Fields: trackerFields,
			SetupHint: "In the repository or App settings, add a webhook pointing at this URL and " +
				"paste the same secret. Content type must be application/json.",
		},
		{
			ID: string(tracker.Jira), Name: "Jira", EventKind: string(tracker.EventKind),
			SecretKind: "token", Fields: trackerFields,
			SetupHint: "In Jira → System → Webhooks, use this URL with ?token=<secret> appended, " +
				"and choose the issue and comment events you care about.",
		},
		{
			ID: string(tracker.YouTrack), Name: "YouTrack", EventKind: string(tracker.EventKind),
			SecretKind: "token", Fields: trackerFields,
			SetupHint: "In YouTrack, add a workflow HTTP handler posting to this URL with " +
				"?token=<secret> appended.",
		},
		{
			ID: "", Name: "Custom", EventKind: "", SecretKind: "any",
			SetupHint: "For a service KARMAX has no decoder for. The payload is published exactly " +
				"as it arrives, under the event kind you choose.",
		},
	}
}

// PlatformByID finds a platform, reporting whether it is one KARMAX knows.
func PlatformByID(id string) (Platform, bool) {
	for _, p := range Platforms() {
		if p.ID == id {
			return p, true
		}
	}
	return Platform{}, false
}

// SignatureHeaders are the headers senders commonly sign with, offered so
// nobody has to remember the exact spelling of X-Hub-Signature-256.
func SignatureHeaders() []string {
	return []string{
		"X-Hub-Signature-256",
		"X-Hub-Signature",
		"X-Signature",
		"X-Signature-256",
		"X-Webhook-Signature",
		"Stripe-Signature",
	}
}
