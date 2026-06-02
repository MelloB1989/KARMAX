package webhook

import (
	"encoding/json"
)

type WebhookRoute struct {
	Path            string         `yaml:"path"             json:"path"`
	Method          string         `yaml:"method"           json:"method"`
	AgentID         string         `yaml:"agent_id"         json:"agent_id"`
	BusEvent        string         `yaml:"bus_event"        json:"bus_event"`
	Secret          string         `yaml:"secret"           json:"secret,omitempty"`
	SignatureHeader string         `yaml:"signature_header" json:"signature_header,omitempty"`
	Response        map[string]any `yaml:"response"         json:"response,omitempty"`
}

type WebhookEvent struct {
	ID         string            `json:"id"`
	Route      string            `json:"route"`
	Method     string            `json:"method"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
	ReceivedAt string            `json:"received_at"`
	Dispatched bool              `json:"dispatched"`
}
