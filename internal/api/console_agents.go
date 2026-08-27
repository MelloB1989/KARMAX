package api

import (
	"net/http"

	"github.com/MelloB1989/karmax/internal/agent"
)

// Agents: the registry as the console sees it, plus each agent's permissions
// rendered by the broker. The grant strings are shown verbatim — the console
// deliberately does not re-word them, because the sentence an operator
// approved at install time is the sentence they should read here.

type agentSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Model       string   `json:"model"`
	Provider    string   `json:"provider"`
	Status      string   `json:"status"`
	OpenCases   int      `json:"open_cases"`
	Grants      []string `json:"grants"`
}

type agentTriggers struct {
	Webhooks   []string            `json:"webhooks"`
	Schedules  []map[string]string `json:"schedules"`
	Events     []string            `json:"events"`
	RunOnStart bool                `json:"run_on_start"`
}

type agentDetail struct {
	agentSummary
	Persona       string        `json:"persona"`
	Tools         []string      `json:"tools"`
	MCPs          []string      `json:"mcps"`
	RestartPolicy string        `json:"restart_policy"`
	Triggers      agentTriggers `json:"triggers"`
}

// closedCaseStates are the states that do not count as open work.
var closedCaseStates = map[string]bool{"done": true, "dropped": true}

// openCaseCount counts an agent's unfinished cases.
func (s *ConsoleServer) openCaseCount(agentID string) int {
	// Filtered by agent in SQL, by state here: the store filters on an exact
	// state and "not done and not dropped" is not one. The limit is generous
	// rather than unbounded so a runaway case table cannot stall the page.
	cases, err := s.store.ListCases(agentID, "", 1000)
	if err != nil {
		s.log.Warn("could not count open cases for agent")
		return 0
	}
	n := 0
	for _, c := range cases {
		if !closedCaseStates[c.State] {
			n++
		}
	}
	return n
}

// grantsFor renders an agent's permissions, never failing the request: a
// broker hiccup should cost the grants column, not the whole page.
func (s *ConsoleServer) grantsFor(agentID string) []string {
	if s.broker == nil {
		return []string{}
	}
	lines, err := s.broker.Describe("agent:" + agentID)
	if err != nil {
		s.log.Warn("could not describe agent grants")
		return []string{}
	}
	if lines == nil {
		return []string{}
	}
	return lines
}

func (s *ConsoleServer) summarise(a *agent.Agent) agentSummary {
	def := a.Def()
	tags := def.Tags
	if tags == nil {
		tags = []string{}
	}
	return agentSummary{
		ID: def.ID, Name: def.Name, Description: def.Description, Tags: tags,
		Model: def.Model, Provider: def.Provider,
		Status:    string(a.Status()),
		OpenCases: s.openCaseCount(def.ID),
		Grants:    s.grantsFor(def.ID),
	}
}

func (s *ConsoleServer) handleConsoleAgents(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeJSON(w, http.StatusOK, map[string]any{"agents": []agentSummary{}})
		return
	}
	list := s.agents.List()
	out := make([]agentSummary, 0, len(list))
	for _, a := range list {
		out = append(out, s.summarise(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (s *ConsoleServer) handleConsoleAgentDetail(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such agent"})
		return
	}
	a, ok := s.agents.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such agent"})
		return
	}

	def := a.Def()
	schedules := make([]map[string]string, 0, len(def.Triggers.Schedules))
	for _, sc := range def.Triggers.Schedules {
		schedules = append(schedules, map[string]string{"cron": sc.Cron})
	}
	nonNil := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}

	writeJSON(w, http.StatusOK, agentDetail{
		agentSummary:  s.summarise(a),
		Persona:       def.SystemPrompt,
		Tools:         nonNil(def.Tools),
		MCPs:          nonNil(def.MCPs),
		RestartPolicy: string(def.RestartPolicy),
		Triggers: agentTriggers{
			Webhooks:   nonNil(def.Triggers.Webhooks),
			Schedules:  schedules,
			Events:     nonNil(def.Triggers.Events),
			RunOnStart: def.Triggers.RunOnStart,
		},
	})
}
