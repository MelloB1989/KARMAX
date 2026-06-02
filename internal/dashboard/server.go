package dashboard

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/internal/webhook"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	router  *chi.Mux
	wsHub   *WSHub
	api     *APIHandler
	addr    string
	log     *zap.Logger
	server  *http.Server
}

func NewServer(
	addr string,
	agents *agent.Registry,
	sched *scheduler.Scheduler,
	wh *webhook.WebhookServer,
	mem *memory.ManagerFactory,
	toolReg *tools.Registry,
	s *store.Store,
	b *bus.Bus,
	log *zap.Logger,
) *Server {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(corsMiddleware)

	wsHub := NewWSHub()
	api := NewAPIHandler(agents, sched, wh, mem, toolReg, s, b)

	srv := &Server{
		router: r,
		wsHub:  wsHub,
		api:    api,
		addr:   addr,
		log:    log,
	}

	api.RegisterRoutes(r)
	r.HandleFunc("/ws", wsHub.HandleWS)

	uiContent, err := fs.Sub(uiFS, "ui")
	if err == nil {
		fileServer := http.FileServer(http.FS(uiContent))
		r.Handle("/*", fileServer)
	}

	return srv
}

func (s *Server) Start(ctx context.Context, b *bus.Bus) error {
	go s.wsHub.Run(ctx, b)

	s.server = &http.Server{
		Addr:    s.addr,
		Handler: s.router,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(shutdownCtx)
	}()

	s.log.Info("dashboard starting", zap.String("addr", fmt.Sprintf("http://%s", s.addr)))
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(ctx)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
