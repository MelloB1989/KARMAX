package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/setupagent"
)

// Connecting a service, narrated line by line.
//
// Newline-delimited JSON rather than SSE: the client for this is a desktop app
// reading a stream it opened, not a browser wanting reconnection semantics, and
// one object per line is the least there is to get wrong on either side.
//
// The stream IS the feature. A person watching an agent create a Google Cloud
// project needs to know it is still going, what it is doing, and — the part
// that matters most — the moment it is their turn to click something.

func (s *Server) handleConnectList(w http.ResponseWriter, r *http.Request) {
	type entry struct {
		ID    string   `json:"id"`
		Name  string   `json:"name"`
		Needs []string `json:"needs"`
		Lede  string   `json:"lede"`
	}
	out := []entry{}
	for _, id := range setupagent.IDs() {
		rec, _ := setupagent.Lookup(id)
		needs := rec.Needs
		if needs == nil {
			needs = []string{}
		}
		out = append(out, entry{ID: rec.ID, Name: rec.Name, Needs: needs, Lede: rec.Lede})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectable": out})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	recipe, ok := setupagent.Lookup(r.PathValue("id"))
	if !ok {
		http.Error(w, "nothing here knows how to connect that", http.StatusNotFound)
		return
	}

	var body struct {
		Account string `json:"account"`
	}
	// A body is optional: most recipes do not need one.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "this connection cannot be streamed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	// Nothing may buffer this. A progress stream that arrives all at once at the
	// end is not a progress stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	send := func(p setupagent.Progress) {
		_ = enc.Encode(p)
		flusher.Flush()
	}

	err := setupagent.Run(r.Context(), recipe, setupagent.Options{
		DataDir: s.dataDir(),
		Browser: s.session(),
		Account: strings.TrimSpace(body.Account),
		Timeout: 20 * time.Minute,
	}, send)
	if err != nil {
		// The error goes down the stream, not into a status code: the headers
		// left five minutes ago.
		send(setupagent.Progress{Kind: "failed", Text: err.Error()})
	}
}
