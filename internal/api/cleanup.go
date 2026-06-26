package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

// Memory cleanup: an LLM reviews stored memories, finds the one most lacking in
// clarity/context/confidence, and asks the operator a clarifying question (with
// suggested options; the operator may also answer freely). The answer is then
// applied to correct or enrich that memory entry.

const cleanupQuestionPrompt = `You help the operator correct and enrich the AI's long-term memory about THEIR life and work. You are given stored memory entries (id + content).

Pick the SINGLE entry that is most ambiguous, low-confidence, missing context, or possibly wrong — where one clarifying question would most improve accuracy. Strongly prefer entries about real PEOPLE, PROJECTS, COMMITMENTS, EVENTS, DECISIONS, and PREFERENCES. Use the OTHER entries as context: when entries are about the same person or project, ask a question that connects or disambiguates them (relationships matter).

Your question MUST be specific and concrete about the operator's world — e.g. "Who is <name> to you?", "Is <project> still active?", "When is <deadline>?", "How are <A> and <B> related?".

NEVER ask about the AI itself, its system prompt, its instructions, how it works, or the memory system. If an entry is AI/system scaffolding rather than a real fact about the operator, ignore it entirely — do not ask about it.

Provide 2-4 plausible short answer options; the operator can also answer freely.

Respond with ONLY a JSON object, no prose:
{"memory_id":"<id>","question":"<question>","options":["opt1","opt2"]}
If no entry needs clarification, respond with exactly: {"memory_id":"","question":"","options":[]}`

const cleanupAnswerPrompt = `You correct ONE memory entry using the operator's answer to a clarifying question. Given the original memory, the question, and the operator's answer, produce the corrected/enriched memory content. Keep the same compact, factual style and preserve any leading [category][importance] tag. If the answer shows the memory is wrong and should be removed entirely, use action "delete".

Respond with ONLY JSON: {"action":"update"|"delete","content":"<new memory content>"}`

// cleanupSession builds the model session for structured utility tasks (memory
// cleanup + relationship graph). It uses the agent's model + fallbacks, so it
// works whenever the agent can reason (i.e. when Claude access is available) and
// fails fast otherwise rather than hanging on a slow/uncompliant fallback.
func (s *Server) cleanupSession(prompt string) *karmahelper.Session {
	provider, model := "anthropic", ""
	var fbs []karmahelper.FallbackModel
	if len(s.cfg.Agents) > 0 {
		a := s.cfg.Agents[0]
		if a.Provider != "" {
			provider = a.Provider
		}
		model = a.Model
		for _, fb := range a.FallbackModels {
			fbs = append(fbs, karmahelper.FallbackModel{Provider: fb.Provider, Model: fb.Model})
		}
	}
	return karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:       provider,
		Model:          model,
		SystemPrompt:   prompt,
		MaxTokens:      1200,
		FallbackModels: fbs,
	}, nil)
}

// extractJSON returns the outermost {...} block from an LLM response.
func extractJSON(s string) string {
	i := strings.IndexByte(s, '{')
	j := strings.LastIndexByte(s, '}')
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return ""
}

// isJunkMemory filters out AI/system scaffolding and trivial entries so the
// cleanup only ever asks about real facts in the operator's world.
func isJunkMemory(content string) bool {
	c := strings.ToLower(strings.TrimSpace(content))
	// Strip leading [category][importance] tags before judging length/content.
	for strings.HasPrefix(c, "[") {
		i := strings.IndexByte(c, ']')
		if i < 0 {
			break
		}
		c = strings.TrimSpace(c[i+1:])
	}
	if len(c) < 12 { // "hi", "hey", "ok", trivial chatter
		return true
	}
	for _, bad := range []string{
		"you are nexus", "system prompt", "## recent context", "recent context",
		"operator profile", "about_me", "memory retrieval", "claude_code", "codex.call",
		"as an ai", "language model", "no relevant context found", "i don't have", "i do not have",
		"automated system message", "how's it going", "how is it going", "anything specific you need",
		"anything i can help", "just catching up", "let me know if", "what's up",
	} {
		if strings.Contains(c, bad) {
			return true
		}
	}
	return false
}

func cleanupTruncate(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func (s *Server) handleCleanupQuestion(w http.ResponseWriter, r *http.Request) {
	ns := s.defaultNamespace()
	entries, err := s.store.ListMemoryEntries(ns, 60)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(entries) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"done": true})
		return
	}

	var sb strings.Builder
	candidates := 0
	for _, e := range entries {
		if isJunkMemory(e.Content) {
			continue // skip AI/system scaffolding & trivial entries
		}
		sb.WriteString(fmt.Sprintf("- id=%s :: %s\n", e.ID, cleanupTruncate(e.Content, 240)))
		candidates++
	}
	if candidates == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"done": true})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	resp, _, _, err := s.cleanupSession(cleanupQuestionPrompt).Chat(ctx, "Memory entries:\n"+sb.String())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	var out struct {
		MemoryID string   `json:"memory_id"`
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}
	_ = json.Unmarshal([]byte(extractJSON(resp)), &out)
	if strings.TrimSpace(out.MemoryID) == "" || strings.TrimSpace(out.Question) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": true})
		return
	}

	content := ""
	for _, e := range entries {
		if e.ID == out.MemoryID {
			content = e.Content
			break
		}
	}
	if content == "" {
		// model referenced an unknown id — treat as done rather than error.
		writeJSON(w, http.StatusOK, map[string]any{"done": true})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"memory_id": out.MemoryID,
		"memory":    content,
		"question":  out.Question,
		"options":   out.Options,
	})
}

func (s *Server) handleCleanupAnswer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MemoryID string `json:"memory_id"`
		Memory   string `json:"memory"`
		Question string `json:"question"`
		Answer   string `json:"answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(req.MemoryID) == "" || strings.TrimSpace(req.Answer) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "memory_id and answer are required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	prompt := fmt.Sprintf("Original memory: %s\n\nQuestion asked: %s\n\nOperator's answer: %s", req.Memory, req.Question, req.Answer)
	resp, _, _, err := s.cleanupSession(cleanupAnswerPrompt).Chat(ctx, prompt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	var out struct {
		Action  string `json:"action"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal([]byte(extractJSON(resp)), &out)

	switch strings.ToLower(strings.TrimSpace(out.Action)) {
	case "delete":
		if err := s.store.DeleteMemoryEntry(req.MemoryID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"action": "delete", "memory_id": req.MemoryID})
	default:
		if strings.TrimSpace(out.Content) == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "model returned no content"})
			return
		}
		if err := s.store.UpdateMemoryEntry(req.MemoryID, out.Content); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"action": "update", "memory_id": req.MemoryID, "content": out.Content})
	}
}
