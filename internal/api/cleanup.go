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

const cleanupQuestionPrompt = `You help the operator correct and enrich the AI's long-term memory. You are given stored memory entries (id + content). Pick the SINGLE entry that is most ambiguous, low-confidence, missing context, or possibly wrong — where a clarifying question would most improve accuracy. Write one short, specific question about it, plus 2-4 plausible short answer options (the operator can also answer in their own words).

Respond with ONLY a JSON object, no prose:
{"memory_id":"<id>","question":"<question>","options":["opt1","opt2"]}
If every entry is already clear and well-contextualized, respond with exactly: {"memory_id":"","question":"","options":[]}`

const cleanupAnswerPrompt = `You correct ONE memory entry using the operator's answer to a clarifying question. Given the original memory, the question, and the operator's answer, produce the corrected/enriched memory content. Keep the same compact, factual style and preserve any leading [category][importance] tag. If the answer shows the memory is wrong and should be removed entirely, use action "delete".

Respond with ONLY JSON: {"action":"update"|"delete","content":"<new memory content>"}`

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
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("- id=%s :: %s\n", e.ID, cleanupTruncate(e.Content, 220)))
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
