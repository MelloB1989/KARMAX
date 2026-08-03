// Package tracker turns issue-tracker and code-host webhooks into KARMAX bus
// events.
//
// GitHub, Jira and YouTrack are not conversations — they are event sources. So
// rather than pretending they're chat channels, each delivery is normalised into
// one Event and published on the bus, where loops decide what (if anything) it
// deserves: a reply, an approval, a reminder, or nothing.
//
// Mount one endpoint per platform:
//
//	/hooks/github    X-Hub-Signature-256 (HMAC-SHA256 of the raw body)
//	/hooks/jira      ?token=… shared secret
//	/hooks/youtrack  ?token=… shared secret
package tracker

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"go.uber.org/zap"
)

// EventKind is the bus event every tracker delivery becomes.
const EventKind bus.EventKind = "tracker.event"

// Source identifies a supported platform.
type Source string

const (
	GitHub   Source = "github"
	Jira     Source = "jira"
	YouTrack Source = "youtrack"
)

// Config wires one platform's endpoint.
type Config struct {
	Source  Source
	Secret  string // GitHub: HMAC key. Jira/YouTrack: shared ?token= value.
	AgentID string // agent the resulting events are addressed to
}

// Handler receives tracker webhooks and publishes normalised bus events.
type Handler struct {
	cfg Config
	bus *bus.Bus
	log *zap.Logger
}

func New(cfg Config, b *bus.Bus, log *zap.Logger) *Handler {
	return &Handler{cfg: cfg, bus: b, log: log}
}

// Normalised shape every platform is mapped onto, so loops and prompts don't
// need per-platform knowledge.
type normalized struct {
	Source   string `json:"source"`   // github | jira | youtrack
	Kind     string `json:"kind"`     // issue | pull_request | comment | build | push
	Action   string `json:"action"`   // opened | closed | commented | assigned | …
	Title    string `json:"title"`    // human-readable subject
	Ref      string `json:"ref"`      // #482, PROJ-17, KAR-3
	Actor    string `json:"actor"`    // who did it
	Assignee string `json:"assignee"` // who it's now on, if anyone
	Project  string `json:"project"`  // repo or project key
	URL      string `json:"url"`      // link a human can open
	Body     string `json:"body"`     // comment/description excerpt
}

// summary renders the one-line form a loop puts in front of a model.
func (n normalized) summary() string {
	var b strings.Builder
	b.WriteString(n.Source)
	if n.Project != "" {
		b.WriteString(" · " + n.Project)
	}
	b.WriteString(": ")
	if n.Ref != "" {
		b.WriteString(n.Ref + " ")
	}
	b.WriteString(n.Action)
	if n.Title != "" {
		b.WriteString(" — " + n.Title)
	}
	if n.Actor != "" {
		b.WriteString(" (by " + n.Actor + ")")
	}
	return b.String()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !h.authorized(r, body) {
		h.log.Warn("tracker webhook rejected", zap.String("source", string(h.cfg.Source)))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Ack immediately — trackers retry aggressively on a slow endpoint.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))

	var n *normalized
	switch h.cfg.Source {
	case GitHub:
		n = parseGitHub(r.Header.Get("X-GitHub-Event"), body)
	case Jira:
		n = parseJira(body)
	case YouTrack:
		n = parseYouTrack(body)
	}
	if n == nil {
		return // event type we deliberately ignore
	}

	h.log.Info("tracker event",
		zap.String("source", n.Source), zap.String("kind", n.Kind),
		zap.String("action", n.Action), zap.String("ref", n.Ref))

	h.bus.Publish(bus.NewEvent(EventKind, h.cfg.AgentID, map[string]any{
		"source":    n.Source,
		"kind":      n.Kind,
		"action":    n.Action,
		"title":     n.Title,
		"ref":       n.Ref,
		"actor":     n.Actor,
		"assignee":  n.Assignee,
		"project":   n.Project,
		"url":       n.URL,
		"body":      n.Body,
		"summary":   n.summary(),
		"timestamp": time.Now(),
	}))
}

// authorized verifies the delivery really came from the platform.
func (h *Handler) authorized(r *http.Request, body []byte) bool {
	if h.cfg.Secret == "" {
		return true // no secret configured: accept (localhost-only deployments)
	}
	switch h.cfg.Source {
	case GitHub:
		// GitHub signs the raw body with HMAC-SHA256.
		sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
		mac := hmac.New(sha256.New, []byte(h.cfg.Secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
	default:
		// Jira and YouTrack have no signing scheme; a shared token in the URL is
		// the documented approach.
		got := r.URL.Query().Get("token")
		return subtle.ConstantTimeCompare([]byte(h.cfg.Secret), []byte(got)) == 1
	}
}

func excerpt(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- GitHub -----------------------------------------------------------------

func parseGitHub(event string, body []byte) *normalized {
	var p struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Issue *struct {
			Number   int    `json:"number"`
			Title    string `json:"title"`
			HTMLURL  string `json:"html_url"`
			Body     string `json:"body"`
			Assignee *struct {
				Login string `json:"login"`
			} `json:"assignee"`
		} `json:"issue"`
		PullRequest *struct {
			Number   int    `json:"number"`
			Title    string `json:"title"`
			HTMLURL  string `json:"html_url"`
			Body     string `json:"body"`
			Assignee *struct {
				Login string `json:"login"`
			} `json:"assignee"`
		} `json:"pull_request"`
		Comment *struct {
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
		} `json:"comment"`
		WorkflowRun *struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_run"`
	}
	if json.Unmarshal(body, &p) != nil {
		return nil
	}

	n := &normalized{
		Source:  string(GitHub),
		Action:  p.Action,
		Actor:   p.Sender.Login,
		Project: p.Repository.FullName,
	}

	switch event {
	case "issues":
		if p.Issue == nil {
			return nil
		}
		n.Kind, n.Ref, n.Title, n.URL = "issue", fmt.Sprintf("#%d", p.Issue.Number), p.Issue.Title, p.Issue.HTMLURL
		n.Body = excerpt(p.Issue.Body, 400)
		if p.Issue.Assignee != nil {
			n.Assignee = p.Issue.Assignee.Login
		}
	case "pull_request":
		if p.PullRequest == nil {
			return nil
		}
		n.Kind, n.Ref, n.Title, n.URL = "pull_request", fmt.Sprintf("#%d", p.PullRequest.Number), p.PullRequest.Title, p.PullRequest.HTMLURL
		n.Body = excerpt(p.PullRequest.Body, 400)
		if p.PullRequest.Assignee != nil {
			n.Assignee = p.PullRequest.Assignee.Login
		}
	case "issue_comment", "pull_request_review_comment":
		if p.Comment == nil {
			return nil
		}
		n.Kind, n.Action = "comment", "commented"
		n.Body, n.URL = excerpt(p.Comment.Body, 400), p.Comment.HTMLURL
		if p.Issue != nil {
			n.Ref, n.Title = fmt.Sprintf("#%d", p.Issue.Number), p.Issue.Title
		}
	case "workflow_run":
		if p.WorkflowRun == nil || p.WorkflowRun.Conclusion == "" {
			return nil // only report finished runs
		}
		// Green builds are noise; a failure is the thing worth surfacing.
		if p.WorkflowRun.Conclusion == "success" {
			return nil
		}
		n.Kind, n.Action = "build", p.WorkflowRun.Conclusion
		n.Title, n.URL = p.WorkflowRun.Name, p.WorkflowRun.HTMLURL
	default:
		return nil // ping, push, star, … — not actionable
	}
	return n
}

// ---- Jira -------------------------------------------------------------------

func parseJira(body []byte) *normalized {
	var p struct {
		WebhookEvent string `json:"webhookEvent"`
		User         struct {
			DisplayName string `json:"displayName"`
		} `json:"user"`
		Issue struct {
			Key    string `json:"key"`
			Self   string `json:"self"`
			Fields struct {
				Summary     string `json:"summary"`
				Description string `json:"description"`
				Project     struct {
					Key string `json:"key"`
				} `json:"project"`
				Assignee *struct {
					DisplayName string `json:"displayName"`
				} `json:"assignee"`
				Status struct {
					Name string `json:"name"`
				} `json:"status"`
			} `json:"fields"`
		} `json:"issue"`
		Comment *struct {
			Body   string `json:"body"`
			Author struct {
				DisplayName string `json:"displayName"`
			} `json:"author"`
		} `json:"comment"`
	}
	if json.Unmarshal(body, &p) != nil || p.Issue.Key == "" {
		return nil
	}

	n := &normalized{
		Source:  string(Jira),
		Kind:    "issue",
		Ref:     p.Issue.Key,
		Title:   p.Issue.Fields.Summary,
		Actor:   p.User.DisplayName,
		Project: p.Issue.Fields.Project.Key,
		URL:     browseURL(p.Issue.Self, p.Issue.Key),
		Body:    excerpt(p.Issue.Fields.Description, 400),
	}
	if a := p.Issue.Fields.Assignee; a != nil {
		n.Assignee = a.DisplayName
	}

	switch {
	case p.Comment != nil:
		n.Kind, n.Action = "comment", "commented"
		n.Body = excerpt(p.Comment.Body, 400)
		if p.Comment.Author.DisplayName != "" {
			n.Actor = p.Comment.Author.DisplayName
		}
	case strings.Contains(p.WebhookEvent, "created"):
		n.Action = "opened"
	case strings.Contains(p.WebhookEvent, "deleted"):
		n.Action = "deleted"
	default:
		n.Action = "updated → " + p.Issue.Fields.Status.Name
	}
	return n
}

// browseURL turns a Jira REST self link into a human-openable browse link.
func browseURL(self, key string) string {
	if i := strings.Index(self, "/rest/"); i > 0 {
		return self[:i] + "/browse/" + key
	}
	return self
}

// ---- YouTrack ---------------------------------------------------------------

func parseYouTrack(body []byte) *normalized {
	// YouTrack workflow webhooks are user-defined; this handles the common
	// shape and degrades gracefully rather than dropping unknown payloads.
	var p struct {
		Issue struct {
			IDReadable  string `json:"idReadable"`
			Summary     string `json:"summary"`
			Description string `json:"description"`
			URL         string `json:"url"`
			Project     struct {
				ShortName string `json:"shortName"`
			} `json:"project"`
			Reporter struct {
				FullName string `json:"fullName"`
			} `json:"reporter"`
			Assignee struct {
				FullName string `json:"fullName"`
			} `json:"assignee"`
		} `json:"issue"`
		Action  string `json:"action"`
		Comment struct {
			Text   string `json:"text"`
			Author struct {
				FullName string `json:"fullName"`
			} `json:"author"`
		} `json:"comment"`
	}
	if json.Unmarshal(body, &p) != nil || p.Issue.IDReadable == "" {
		return nil
	}
	n := &normalized{
		Source:   string(YouTrack),
		Kind:     "issue",
		Ref:      p.Issue.IDReadable,
		Title:    p.Issue.Summary,
		Actor:    p.Issue.Reporter.FullName,
		Assignee: p.Issue.Assignee.FullName,
		Project:  p.Issue.Project.ShortName,
		URL:      p.Issue.URL,
		Action:   p.Action,
	}
	if n.Action == "" {
		n.Action = "updated"
	}
	if p.Comment.Text != "" {
		n.Kind, n.Action = "comment", "commented"
		n.Body = excerpt(p.Comment.Text, 400)
		if p.Comment.Author.FullName != "" {
			n.Actor = p.Comment.Author.FullName
		}
	}
	return n
}
