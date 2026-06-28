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

		// Score against the task DESCRIPTION (the original prompt), not the
		// session output — including the (often large) output in the comparison
		// balloons the word set and tanks the match even for identical tasks.
		// Containment-based so a short follow-up on the same subject still scores.
		score := taskContainment(prompt, cs.Description)
		if status == "active" || status == "running" {
			score += 0.2
		}
		if score > bestScore {
			bestScore = score
			cp := cs
			best = &cp
		}
	}

	// Conservative threshold: only auto-resume on a strong subject match. Weaker
	// follow-ups should resume via an explicit session_id (the agent sees prior
	// sessions + their ids in its "Active Coding Sessions" context).
	if bestScore < 0.6 {
		return nil
	}
	return best
}

// taskContainment scores how much the smaller of the two word sets is covered
// by the other (intersection / min set size). Unlike Jaccard, a short follow-up
// fully contained in a longer task description still scores ~1.0.
func taskContainment(a, b string) float64 {
	setA := wordSet(strings.ToLower(a))
	setB := wordSet(strings.ToLower(b))
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	inter := 0
	for w := range setA {
		if setB[w] {
			inter++
		}
	}
	smaller := len(setA)
	if len(setB) < smaller {
		smaller = len(setB)
	}
	return float64(inter) / float64(smaller)
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
