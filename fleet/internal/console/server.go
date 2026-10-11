package console

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Version is set at build time (-ldflags "-X .../console.Version=…").
var Version = "dev"

// Handler is the console's whole HTTP surface.
func Handler(h *Hub, t *Terms, clientToken string, fleetd *FleetdClient) http.Handler {
	return newHandler(h, t, clientToken, fleetd, embeddedAssets())
}

func newHandler(h *Hub, t *Terms, clientToken string, fleetd *FleetdClient, assets fs.FS) http.Handler {
	a := authn{token: clientToken}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sources": h.Snapshot().Sources, "version": Version})
	})
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, h.Snapshot())
	})
	mux.HandleFunc("GET /v1/watch", func(w http.ResponseWriter, r *http.Request) { watch(h, w, r) })
	mux.HandleFunc("POST /v1/prompt", proxy(fleetd, "/v1/prompt"))
	mux.HandleFunc("POST /v1/rotate", proxy(fleetd, "/v1/rotate"))
	mux.HandleFunc("GET /v1/term/{agent}", term(t))
	spa := static(assets)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only non-API paths fall back to the panel; an unknown API path
		// must fail as one (a WebSocket upgrade cannot use a 200 page).
		if p := r.URL.Path; p == "/healthz" || p == "/v1" || strings.HasPrefix(p, "/v1/") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such route: " + r.Method + " " + p})
			return
		}
		spa.ServeHTTP(w, r)
	})

	// Everything under /v1 needs the token, including what does not exist.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") && !a.ok(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: send the console token as a bearer header or log in at /login"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func watch(h *Hub, w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context()) // ends when the client goes away
	ch, cancel := h.Subscribe(ctx)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				c.Close(websocket.StatusTryAgainLater, "too slow: resubscribe")
				return
			}
			b, err := json.Marshal(m)
			if err != nil {
				continue
			}
			wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
			err = c.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err != nil {
				return
			}
		}
	}
}

// proxy forwards a body to fleetd and returns fleetd's status and body as
// they came.
func proxy(f *FleetdClient, route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		code, out, err := f.Do(r.Context(), http.MethodPost, route, body)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		w.Write(out)
	}
}

// static serves the panel, falling back to index.html for client-side
// routes, or to a built-in notice when the panel was never built.
func static(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)
	_, err := fs.Stat(assets, "index.html")
	built := err == nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, notBuiltPage)
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			files.ServeHTTP(w, r)
			return
		}
		if fi, err := fs.Stat(assets, p); err != nil || fi.IsDir() {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			files.ServeHTTP(w, r2)
			return
		}
		files.ServeHTTP(w, r)
	})
}
