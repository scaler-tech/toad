package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

type mockDB struct {
	tokens map[string]*state.MCPToken
}

func (m *mockDB) ValidateMCPToken(token string) (*state.MCPToken, error) {
	return m.tokens[token], nil
}

// TestIsLoopbackHost covers the bind-warning classification: loopback
// values suppress the plaintext-bearer-over-the-network warning, everything
// else (including "" — which net/http binds as all interfaces) does not.
func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"", false}, // empty host binds all interfaces — treat as non-loopback
		{"0.0.0.0", false},
		{"192.168.1.5", false},
		{"my-host.internal", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isLoopbackHost(tt.host); got != tt.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestAuthMiddleware(t *testing.T) {
	db := &mockDB{tokens: map[string]*state.MCPToken{
		"toad_valid": {
			Token:       "toad_valid",
			SlackUserID: "U123",
			SlackUser:   "alice",
			Role:        "dev",
			CreatedAt:   time.Now(),
		},
	}}

	handler := authMiddleware(db, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := tokenFromContext(r.Context())
		if tok == nil {
			t.Fatal("expected token in context")
		}
		if tok.SlackUserID != "U123" {
			t.Errorf("got user %q, want U123", tok.SlackUserID)
		}
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name string
		auth string
		want int
	}{
		{"valid token", "Bearer toad_valid", http.StatusOK},
		{"missing header", "", http.StatusUnauthorized},
		{"invalid token", "Bearer toad_bad", http.StatusUnauthorized},
		{"wrong scheme", "Basic toad_valid", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/mcp", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("got %d, want %d", w.Code, tt.want)
			}
		})
	}
}
