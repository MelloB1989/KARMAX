package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MelloB1989/karmax/internal/browser"
	"github.com/MelloB1989/karmax/internal/hostpaths"
)

// The browser, over HTTP, so a desktop app can drive it.
//
// The app's job here is narration: it starts the window, sends the person to a
// page, and tells them what to do on it. Everything it needs to say that
// truthfully — is it open, what is on screen, where is the profile — comes from
// these three calls.

type browserView struct {
	Running   bool         `json:"running"`
	Available bool         `json:"available"`
	Binary    string       `json:"binary,omitempty"`
	Profile   string       `json:"profile"`
	Tabs      []browserTab `json:"tabs"`
	Reason    string       `json:"reason,omitempty"`
}

type browserTab struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (s *Server) session() *browser.Session {
	dir := ""
	if s.cfg != nil {
		dir = s.cfg.Karmax.DataDir
	}
	return browser.Shared(dir)
}

func (s *Server) handleBrowserStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.browserView(r))
}

func (s *Server) browserView(r *http.Request) browserView {
	sess := s.session()
	view := browserView{
		Profile:   sess.Profile(),
		Binary:    hostpaths.Browser(),
		Available: hostpaths.Browser() != "",
		Running:   sess.Running(r.Context()),
		Tabs:      []browserTab{},
	}
	if !view.Available {
		view.Reason = "No Chrome, Chromium or Edge on this machine."
	}
	if view.Running {
		if tabs, err := sess.Tabs(r.Context()); err == nil {
			for _, t := range tabs {
				view.Tabs = append(view.Tabs, browserTab{Title: t.Title, URL: t.URL})
			}
		}
	}
	return view
}

func (s *Server) handleBrowserStart(w http.ResponseWriter, r *http.Request) {
	if err := s.session().Start(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.browserView(r))
}

func (s *Server) handleBrowserOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// http(s) only. The browser will happily open file:// and chrome://, and
	// this endpoint is reachable by anything holding the API token — which is
	// the desktop app, but the rule should not depend on that staying true.
	url := strings.TrimSpace(body.URL)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		http.Error(w, "only http and https URLs can be opened", http.StatusBadRequest)
		return
	}
	tab, err := s.session().Open(r.Context(), url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"opened": tab.URL, "title": tab.Title})
}

func (s *Server) handleBrowserStop(w http.ResponseWriter, r *http.Request) {
	if err := s.session().Stop(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"running": false})
}
