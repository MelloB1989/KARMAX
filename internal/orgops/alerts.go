package orgops

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Alert is any monitoring tool's alert, reduced to what the office acts on.
type Alert struct {
	Key      string `json:"key"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Service  string `json:"service"`
	Repo     string `json:"repo"`
	Severity string `json:"severity"`
	// Resolved is a recovery notice for an earlier alert with the same key.
	Resolved bool   `json:"resolved"`
	URL      string `json:"url"`
	Evidence string `json:"evidence,omitempty"`
}

// Payload is the bus form of an alert.
func (a Alert) Payload() map[string]any {
	return map[string]any{
		"key": a.Key, "source": a.Source, "title": a.Title, "detail": a.Detail,
		"service": a.Service, "repo": a.Repo, "severity": a.Severity,
		"resolved": a.Resolved, "url": a.URL, "evidence": a.Evidence,
	}
}

// AlertFromPayload reads an alert back off the bus.
func AlertFromPayload(p map[string]any) Alert {
	resolved, _ := p["resolved"].(bool)
	return Alert{
		Key: str(p, "key"), Source: str(p, "source"), Title: str(p, "title"), Detail: str(p, "detail"),
		Service: str(p, "service"), Repo: str(p, "repo"), Severity: str(p, "severity"),
		Resolved: resolved, URL: str(p, "url"), Evidence: str(p, "evidence"),
	}
}

// NormalizeAlerts understands Alertmanager/Grafana, Sentry, and a plain
// {"title","service","severity","detail","repo"} body. Unknown shapes become
// one generic alert carrying the raw body, so nothing a tool sends is dropped.
func NormalizeAlerts(body []byte) ([]Alert, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("alert body is not JSON: %w", err)
	}
	// Alertmanager and Grafana unified alerting.
	if list, ok := m["alerts"].([]any); ok {
		var out []Alert
		for _, raw := range list {
			a, _ := raw.(map[string]any)
			labels, _ := a["labels"].(map[string]any)
			ann, _ := a["annotations"].(map[string]any)
			name := first(str(labels, "alertname"), str(m, "title"), "alert")
			svc := first(str(labels, "service"), str(labels, "job"), str(labels, "app"))
			out = append(out, Alert{
				Key:      "am:" + name + ":" + svc,
				Source:   "alertmanager",
				Title:    first(str(ann, "summary"), name),
				Detail:   first(str(ann, "description"), str(ann, "message"), str(m, "message")),
				Service:  svc,
				Repo:     str(labels, "repo"),
				Severity: first(str(labels, "severity"), "warning"),
				Resolved: strings.EqualFold(str(a, "status"), "resolved"),
				URL:      first(str(a, "generatorURL"), str(a, "panelURL")),
			})
		}
		return out, nil
	}
	// Sentry issue alerts / webhooks.
	if data, ok := m["data"].(map[string]any); ok {
		ev, _ := data["event"].(map[string]any)
		if ev == nil {
			ev, _ = data["issue"].(map[string]any)
		}
		if ev != nil {
			project := first(str(ev, "project"), str(m, "project_name"), str(m, "project"))
			title := first(str(ev, "title"), str(m, "message"), "Sentry issue")
			return []Alert{{
				Key: "sentry:" + project + ":" + title, Source: "sentry", Title: title,
				Detail:  first(str(ev, "culprit"), str(ev, "message")),
				Service: project, Severity: first(str(ev, "level"), "error"),
				Resolved: str(m, "action") == "resolved",
				URL:      first(str(ev, "web_url"), str(ev, "permalink"), str(m, "url")),
			}}, nil
		}
	}
	if _, ok := m["project_name"]; ok {
		title := first(str(m, "message"), str(m, "culprit"), "Sentry issue")
		return []Alert{{Key: "sentry:" + str(m, "project_name") + ":" + title, Source: "sentry", Title: title,
			Detail: str(m, "culprit"), Service: str(m, "project_name"), Severity: first(str(m, "level"), "error"),
			URL: str(m, "url")}}, nil
	}
	// Generic.
	title := first(str(m, "title"), str(m, "summary"), str(m, "message"), str(m, "text"))
	if title == "" {
		title = "alert: " + truncate(string(body), 120)
	}
	svc := first(str(m, "service"), str(m, "app"), str(m, "component"))
	status := strings.ToLower(str(m, "status"))
	return []Alert{{
		Key:      first(str(m, "key"), "alert:"+svc+":"+title),
		Source:   first(str(m, "source"), "webhook"),
		Title:    title,
		Detail:   first(str(m, "detail"), str(m, "description"), str(m, "body")),
		Service:  svc,
		Repo:     str(m, "repo"),
		Severity: first(str(m, "severity"), str(m, "level"), "warning"),
		Resolved: status == "resolved" || status == "ok",
		URL:      str(m, "url"),
	}}, nil
}

// AlertHandler is the HTTP intake mounted on KARMAX's webhook server.
func (s *Service) AlertHandler() http.HandlerFunc {
	token := s.cfg.AlertToken
	if token == "" && s.cfg.AlertTokenEnv != "" {
		token = os.Getenv(s.cfg.AlertTokenEnv)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if token != "" && r.Header.Get("X-Karmax-Token") != token && r.URL.Query().Get("token") != token {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		alerts, err := NormalizeAlerts(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, a := range alerts {
			if err := s.publish(EventAlert, a.Payload()); err != nil {
				http.Error(w, "could not queue alert", http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": len(alerts)})
	}
}

func first(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
