// Package mcp implements the Toad MCP server for Claude Desktop/Code integration.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/state"
)

type tokenKey struct{}

// TokenValidator abstracts token validation for testability.
type TokenValidator interface {
	ValidateMCPToken(token string) (*state.MCPToken, error)
}

// HealthInfo holds live daemon status for the health endpoint.
type HealthInfo struct {
	mu             sync.RWMutex
	StartedAt      time.Time
	Version        string
	ActiveTadpoles int
	ActiveRibbits  int
	SlackConnected bool
}

// Server wraps the MCP server and its dependencies.
type Server struct {
	mcpServer *gomcp.Server
	cfg       config.MCPConfig
	db        TokenValidator
	httpSrv   *http.Server
	health    *HealthInfo
}

// New creates a new MCP server.
func New(cfg config.MCPConfig, db TokenValidator) *Server {
	mcpSrv := gomcp.NewServer(&gomcp.Implementation{
		Name:    "toad",
		Version: "1.0.0",
	}, nil)

	return &Server{
		mcpServer: mcpSrv,
		cfg:       cfg,
		db:        db,
		health:    &HealthInfo{StartedAt: time.Now()},
	}
}

// Start begins listening. Blocks until ctx is canceled.
func (s *Server) Start(ctx context.Context) error {
	handler := gomcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *gomcp.Server {
			return s.mcpServer
		},
		&gomcp.StreamableHTTPOptions{
			Stateless: true,
		},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.Handle("/mcp", authMiddleware(s.db, handler))

	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("MCP server shutdown error", "error", err)
		}
	}()

	slog.Info("MCP server listening", "port", s.cfg.Port)
	err := s.httpSrv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// MCPServer returns the underlying MCP server for tool registration.
func (s *Server) MCPServer() *gomcp.Server {
	return s.mcpServer
}

// Health returns the server's health info for external inspection.
func (s *Server) Health() *HealthInfo {
	return s.health
}

// SetHealth atomically updates health info fields via a callback.
func (s *Server) SetHealth(fn func(*HealthInfo)) {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	fn(s.health)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.health.mu.RLock()
	defer s.health.mu.RUnlock()

	resp := map[string]any{
		"status":          "ok",
		"version":         s.health.Version,
		"uptime_seconds":  int(time.Since(s.health.StartedAt).Seconds()),
		"active_tadpoles": s.health.ActiveTadpoles,
		"active_ribbits":  s.health.ActiveRibbits,
		"slack_connected": s.health.SlackConnected,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func authMiddleware(db TokenValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing authorization", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		tok, err := db.ValidateMCPToken(token)
		if err != nil {
			slog.Error("token validation error", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if tok == nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), tokenKey{}, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tokenFromContext(ctx context.Context) *state.MCPToken {
	tok, _ := ctx.Value(tokenKey{}).(*state.MCPToken)
	return tok
}
