// The chat, streamed.
//
// The existing /api/chat stays: the phone-app route and the task runner use
// it and have no reason to move. This is the surface the desktop screen needs,
// which is the same turn with the middle shown rather than swallowed.
//
// Newline-delimited JSON rather than SSE: the client is a desktop app reading
// a stream it opened, not a browser wanting reconnection semantics, and one
// object per line is the least there is to get wrong on either side.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/chatlog"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/google/uuid"
)

// harnessEvent is a local alias for ChatEvent: this file only forwards events
// the runtime adapter already shaped, so it needs no harness import for a type
// it never inspects.
type harnessEvent = ChatEvent

var errTest = errors.New("turn failed")

// lineSink is a writer that reports whole lines, so the stream's shape can be
// asserted without an HTTP server.
type lineSink struct {
	onLine func(string)
	buf    strings.Builder
}

func (l *lineSink) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		s := l.buf.String()
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		l.onLine(s[:i])
		l.buf.Reset()
		l.buf.WriteString(s[i+1:])
	}
	return len(p), nil
}

func (l *lineSink) Flush() {}

// flusher is what a streaming response needs and a test does not.
type flusher interface{ Flush() }

// streamTurn writes one turn's events as NDJSON.
//
// `run` is the turn itself, given the sink to report through. Split out so the
// wire format is testable without a harness, a subprocess or a model.
func streamTurn(w io.Writer, conversationID string, isNew bool,
	run func(sink func(harnessEvent)) (string, error)) {

	enc := json.NewEncoder(w)
	send := func(v any) {
		_ = enc.Encode(v)
		if f, ok := w.(flusher); ok {
			f.Flush()
		}
	}

	if isNew {
		send(map[string]any{"kind": "conversation", "id": conversationID})
	}

	text, err := run(func(e harnessEvent) {
		// Each kind writes only its own fields. A blanket dump would put an
		// empty "tool" on every text delta, and the client's union would have
		// to treat every field as optional to read it.
		obj := map[string]any{"kind": e.Kind}
		switch e.Kind {
		case "text", "error":
			obj["text"] = e.Text
		case "tool":
			obj["tool"], obj["phase"] = e.Tool, e.Phase
		case "ticket":
			// A ticket's title rides in Text: ChatEvent has no title field and
			// giving it one would put a chat's concern in the wire type.
			obj["jobId"], obj["title"] = e.JobID, e.Text
		}
		send(obj)
	})

	if err != nil {
		// The error goes down the stream, not into a status code: the headers
		// left before the first token did.
		send(map[string]any{"kind": "error", "text": err.Error()})
		return
	}
	send(map[string]any{"kind": "done", "text": text})
}

func (s *Server) chatDir() string { return chatlog.Dir(hostpaths.WorkDir()) }

func (s *Server) handleChatConversations(w http.ResponseWriter, r *http.Request) {
	convs, err := chatlog.List(s.chatDir())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// The brain is named so the screen can explain an empty list rather than
	// just showing one: Codex keeps its sessions elsewhere and in another shape.
	writeJSON(w, http.StatusOK, map[string]any{
		"conversations": convs,
		"brain":         s.brainName(),
	})
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	msgs, err := chatlog.Read(s.chatDir(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such conversation"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

func (s *Server) handleChatDelete(w http.ResponseWriter, r *http.Request) {
	if err := chatlog.Delete(s.chatDir(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ConversationID string `json:"conversationId"`
		Message        string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "message is required"})
		return
	}
	if s.chatTurn == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the brain is not running"})
		return
	}

	id, isNew := body.ConversationID, false
	if strings.TrimSpace(id) == "" {
		id, isNew = uuid.New().String(), true
	}

	// Nothing may buffer this. A progress stream that arrives at the end is
	// not a progress stream.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	streamTurn(w, id, isNew, func(sink func(harnessEvent)) (string, error) {
		return s.chatTurn(r.Context(), "chat:"+id, body.Message, sink)
	})
}
