package state

import (
	"testing"
	"time"
)

func TestMCPToken_SaveAndValidate(t *testing.T) {
	db := openTestDB(t)

	tok := &MCPToken{
		Token:       "tok-abc123",
		SlackUserID: "U12345",
		SlackUser:   "alice",
		Role:        "dev",
		CreatedAt:   time.Now().Truncate(time.Second),
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	got, err := db.ValidateMCPToken("tok-abc123")
	if err != nil {
		t.Fatalf("validate token: %v", err)
	}
	if got == nil {
		t.Fatal("expected to find token")
	}
	if got.SlackUserID != "U12345" {
		t.Errorf("got SlackUserID %q, want %q", got.SlackUserID, "U12345")
	}
	if got.SlackUser != "alice" {
		t.Errorf("got SlackUser %q, want %q", got.SlackUser, "alice")
	}
	if got.Role != "dev" {
		t.Errorf("got Role %q, want %q", got.Role, "dev")
	}
	if got.LastUsedAt.IsZero() {
		t.Error("expected LastUsedAt to be set after validation")
	}
}

func TestMCPToken_ValidateInvalid(t *testing.T) {
	db := openTestDB(t)

	got, err := db.ValidateMCPToken("nonexistent-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for invalid token, got %+v", got)
	}
}

func TestMCPToken_Revoke(t *testing.T) {
	db := openTestDB(t)

	tok := &MCPToken{
		Token:       "tok-revoke",
		SlackUserID: "U99999",
		SlackUser:   "bob",
		Role:        "user",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	// Verify it exists
	got, err := db.ValidateMCPToken("tok-revoke")
	if err != nil {
		t.Fatalf("validate before revoke: %v", err)
	}
	if got == nil {
		t.Fatal("expected token to exist before revoke")
	}

	// Revoke
	if err := db.RevokeMCPToken("U99999"); err != nil {
		t.Fatalf("revoke token: %v", err)
	}

	// Verify it's gone
	got, err = db.ValidateMCPToken("tok-revoke")
	if err != nil {
		t.Fatalf("validate after revoke: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after revoke, got %+v", got)
	}
}

// TestMCPToken_HashedAtRest confirms the token is stored as a SHA-256 hash,
// never in plaintext, while still validating against the original
// plaintext bearer value.
func TestMCPToken_HashedAtRest(t *testing.T) {
	db := openTestDB(t)

	plain := "toad_supersecretbearervalue"
	tok := &MCPToken{
		Token:       plain,
		SlackUserID: "U4",
		SlackUser:   "dave",
		Role:        "user",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	got, err := db.ValidateMCPToken(plain)
	if err != nil {
		t.Fatalf("validate with plaintext: %v", err)
	}
	if got == nil {
		t.Fatal("expected token to validate against the plaintext bearer")
	}

	var stored string
	if err := db.db.QueryRow(`SELECT token FROM mcp_tokens WHERE slack_user_id = ?`, "U4").Scan(&stored); err != nil {
		t.Fatalf("querying stored token row: %v", err)
	}
	if stored == plain {
		t.Fatal("expected stored token column to be hashed, found plaintext")
	}
	if len(stored) != 64 {
		t.Errorf("expected a 64-char SHA-256 hex digest, got %d chars: %q", len(stored), stored)
	}
}

// TestMCPToken_ExpiredRejected confirms an expired token is treated as not
// found by ValidateMCPToken.
func TestMCPToken_ExpiredRejected(t *testing.T) {
	db := openTestDB(t)

	tok := &MCPToken{
		Token:       "tok-expired",
		SlackUserID: "U5",
		SlackUser:   "erin",
		Role:        "user",
		CreatedAt:   time.Now().Add(-48 * time.Hour),
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	got, err := db.ValidateMCPToken("tok-expired")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected expired token to be rejected, got %+v", got)
	}
}

// TestMCPToken_UnexpiredAccepted confirms a token with a future expiry
// still validates normally.
func TestMCPToken_UnexpiredAccepted(t *testing.T) {
	db := openTestDB(t)

	tok := &MCPToken{
		Token:       "tok-unexpired",
		SlackUserID: "U6",
		SlackUser:   "frank",
		Role:        "user",
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	got, err := db.ValidateMCPToken("tok-unexpired")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected unexpired token to validate")
	}
}

// TestMCPToken_NilExpiresAtAccepted confirms a NULL expires_at (token
// issued with token_ttl_days=0, or issued before expiry existed) is
// treated as "never expires" rather than immediately expired.
func TestMCPToken_NilExpiresAtAccepted(t *testing.T) {
	db := openTestDB(t)

	tok := &MCPToken{
		Token:       "tok-no-expiry",
		SlackUserID: "U7",
		SlackUser:   "grace",
		Role:        "user",
		CreatedAt:   time.Now(),
		// ExpiresAt intentionally left zero.
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	got, err := db.ValidateMCPToken("tok-no-expiry")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected token with no expiry to validate")
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("expected ExpiresAt to remain zero, got %v", got.ExpiresAt)
	}
}

func TestMCPToken_SaveReplace(t *testing.T) {
	db := openTestDB(t)

	tok := &MCPToken{
		Token:       "tok-replace",
		SlackUserID: "U111",
		SlackUser:   "charlie",
		Role:        "user",
		CreatedAt:   time.Now(),
	}
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token: %v", err)
	}

	// Save again with updated role
	tok.Role = "dev"
	if err := db.SaveMCPToken(tok); err != nil {
		t.Fatalf("save token replace: %v", err)
	}

	got, err := db.ValidateMCPToken("tok-replace")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got.Role != "dev" {
		t.Errorf("got Role %q after replace, want %q", got.Role, "dev")
	}
}
