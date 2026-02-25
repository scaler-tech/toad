package vcs

import (
	"os/exec"
	"testing"
)

func skipWithoutGh(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh CLI not available, skipping")
	}
}

func TestNewResolver_FallbackForUnknownRepo(t *testing.T) {
	skipWithoutGh(t)

	fallback := ProviderConfig{Platform: "github"}
	r, err := NewResolver(nil, fallback)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	p := r("/some/unknown/path")
	if p == nil {
		t.Fatal("expected non-nil provider for unknown path")
	}
	if p.PRNoun() != "PR" {
		t.Errorf("expected PR noun from GitHub fallback, got %q", p.PRNoun())
	}
}

func TestNewResolver_RepoSpecificLookup(t *testing.T) {
	skipWithoutGh(t)

	repoVCS := map[string]ProviderConfig{
		"/repos/frontend": {Platform: "github"},
	}
	fallback := ProviderConfig{Platform: "github"}

	r, err := NewResolver(repoVCS, fallback)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	// Known path returns a provider
	p := r("/repos/frontend")
	if p == nil {
		t.Fatal("expected non-nil provider for known repo path")
	}

	// Unknown path returns fallback
	fb := r("/repos/other")
	if fb == nil {
		t.Fatal("expected non-nil fallback provider")
	}
}

func TestNewResolver_ProviderSharing(t *testing.T) {
	skipWithoutGh(t)

	repoVCS := map[string]ProviderConfig{
		"/repos/a": {Platform: "github"},
		"/repos/b": {Platform: "github"},
	}
	fallback := ProviderConfig{Platform: "github"}

	r, err := NewResolver(repoVCS, fallback)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	pa := r("/repos/a")
	pb := r("/repos/b")
	pfb := r("/repos/unknown")

	// All three should be the same instance since configs are identical
	if pa != pb {
		t.Error("repos with identical config should share a provider instance")
	}
	if pa != pfb {
		t.Error("fallback with identical config should share a provider instance")
	}
}

func TestNewResolver_UnsupportedPlatform(t *testing.T) {
	fallback := ProviderConfig{Platform: "bitbucket"}
	_, err := NewResolver(nil, fallback)
	if err == nil {
		t.Error("expected error for unsupported platform")
	}
}

func TestNewResolver_EmptyRepoPath(t *testing.T) {
	skipWithoutGh(t)

	fallback := ProviderConfig{Platform: "github"}
	r, err := NewResolver(nil, fallback)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	// Empty string should hit fallback
	p := r("")
	if p == nil {
		t.Fatal("expected non-nil provider for empty path")
	}
	if p.PRNoun() != "PR" {
		t.Errorf("expected PR from fallback, got %q", p.PRNoun())
	}
}

func TestConfigKey_DifferentOrder(t *testing.T) {
	// Same bot usernames in different order should produce the same key
	a := ProviderConfig{Platform: "github", BotUsernames: []string{"bot-b", "bot-a"}}
	b := ProviderConfig{Platform: "github", BotUsernames: []string{"bot-a", "bot-b"}}
	if configKey(a) != configKey(b) {
		t.Error("configKey should be order-independent for bot usernames")
	}
}

func TestConfigKey_DifferentConfigs(t *testing.T) {
	a := ProviderConfig{Platform: "github"}
	b := ProviderConfig{Platform: "github", Host: "gh.example.com"}
	if configKey(a) == configKey(b) {
		t.Error("different configs should produce different keys")
	}
}

func TestConfigKey_HostCaseInsensitive(t *testing.T) {
	a := ProviderConfig{Platform: "github", Host: "gitlab.example.com"}
	b := ProviderConfig{Platform: "github", Host: "GITLAB.Example.Com"}
	if configKey(a) != configKey(b) {
		t.Error("configKey should be case-insensitive for host")
	}
}
