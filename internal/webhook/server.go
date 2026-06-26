package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type WebhookServer struct {
	router *chi.Mux
	bus    *bus.Bus
	routes map[string]WebhookRoute
	store  *store.Store
	log    *zap.Logger
	addr   string
	server *http.Server
	mu     sync.RWMutex
}

func New(addr string, b *bus.Bus, s *store.Store, log *zap.Logger) *WebhookServer {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	return &WebhookServer{
		router: r,
		bus:    b,
		routes: make(map[string]WebhookRoute),
		store:  s,
		log:    log,
		addr:   addr,
	}
}

func (s *WebhookServer) AddRoute(route WebhookRoute) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if route.Path == "" {
		return fmt.Errorf("route path is required")
	}
	if route.Method == "" {
		route.Method = "POST"
	}

	s.routes[route.Path] = route

	s.router.HandleFunc(route.Path, s.handleWebhook)
	s.log.Info("webhook route registered", zap.String("path", route.Path), zap.String("method", route.Method))
	return nil
}

func (s *WebhookServer) Start(ctx context.Context) error {
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

	s.log.Info("webhook server starting", zap.String("addr", s.addr))
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *WebhookServer) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(ctx)
	}
}

func (s *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	s.mu.RLock()
	route, ok := s.routes[path]
	s.mu.RUnlock()

	if !ok {
		http.NotFound(w, r)
		return
	}

	if route.Method != "*" && !strings.EqualFold(route.Method, r.Method) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		s.log.Error("read webhook body", zap.Error(err))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if route.Secret != "" && route.SignatureHeader != "" {
		sig := r.Header.Get(route.SignatureHeader)
		if !verifyHMAC(body, route.Secret, sig) {
			s.log.Warn("webhook signature verification failed", zap.String("path", path))
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	eventID := uuid.New().String()

	s.store.SaveWebhookEvent(store.StoredWebhookEvent{
		ID:         eventID,
		Route:      path,
		Method:     r.Method,
		Headers:    fmt.Sprintf("%v", headers),
		Body:       string(body),
		Dispatched: true,
	})

	evt := bus.NewEvent(bus.EventWebhookFired, route.AgentID, map[string]any{
		"webhook_id": eventID,
		"route":      path,
		"method":     r.Method,
		"headers":    headers,
		"body":       string(body),
	})

	if route.BusEvent != "" {
		evt.Kind = bus.EventKind(route.BusEvent)
	}

	s.bus.Publish(evt)

	s.log.Info("webhook received", zap.String("path", path), zap.String("method", r.Method))

	if route.Response != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(route.Response)
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"id":     eventID,
		})
	}
}

func verifyHMAC(body []byte, secret, signature string) bool {
	if signature == "" {
		return false
	}

	signature = strings.TrimPrefix(signature, "sha256=")

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expected), []byte(signature))
}
