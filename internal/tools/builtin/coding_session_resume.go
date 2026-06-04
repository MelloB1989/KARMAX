package builtin

import (
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
)

func findReusableCodingSession(s *store.Store, agentID, toolType, prompt string) *store.StoredCodingSession {
	if s == nil || agentID == "" {
		return nil
	}

	sessions, err := s.ListCodingSessions(agentID)
	if err != nil {
		return nil
	}

	prompt = strings.TrimSpace(prompt)
	var best *store.StoredCodingSession
	bestScore := 0.0
	for i := range sessions {
		cs := sessions[i]
		if cs.ToolType != toolType || cs.SessionID == "" {
			continue
		}
		status := strings.ToLower(cs.Status)
		if status == "failed" {
			continue
		}

		searchText := strings.TrimSpace(cs.Description + " " + cs.Output)
		score := wordOverlap(prompt, searchText)
		if status == "active" || status == "running" {
			score += 0.25
		}
		if score > bestScore {
			bestScore = score
			cp := cs
			best = &cp
		}
	}

	if bestScore < 0.2 {
		return nil
	}
	return best
}

func prependSessionContext(prompt string, session *store.StoredCodingSession) string {
	if session == nil {
		return prompt
	}

	var sb strings.Builder
	sb.WriteString("Continue from prior KARMAX coding session ")
	sb.WriteString(session.SessionID)
	sb.WriteString(".\n\nPrevious task: ")
	sb.WriteString(session.Description)
	if strings.TrimSpace(session.Output) != "" {
		sb.WriteString("\n\nRecent output:\n")
		sb.WriteString(truncate(session.Output, 2000))
	}
	sb.WriteString("\n\nCurrent task:\n")
	sb.WriteString(prompt)
	return sb.String()
}
