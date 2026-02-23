package tadpole

import (
	"testing"

	"github.com/hergen/toad/internal/config"
)

func TestTruncate_UnderLimit(t *testing.T) {
	got := truncate("short", 100)
	if got != "short" {
		t.Errorf("expected 'short', got %q", got)
	}
}

func TestTruncate_AtLimit(t *testing.T) {
	input := "12345"
	got := truncate(input, 5)
	if got != "12345" {
		t.Errorf("expected '12345', got %q", got)
	}
}

func TestTruncate_OverLimit(t *testing.T) {
	input := "1234567890"
	got := truncate(input, 5)
	if got != "12345\n... (truncated)" {
		t.Errorf("expected truncated string, got %q", got)
	}
}

func TestTruncate_Empty(t *testing.T) {
	got := truncate("", 10)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestResolveChecks_NoServices(t *testing.T) {
	cfg := ValidateConfig{
		TestCommand: "go test ./...",
		LintCommand: "golangci-lint run",
	}
	checks := resolveChecks([]string{"main.go", "internal/foo.go"}, cfg)
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].testCommand != "go test ./..." {
		t.Errorf("expected root test command, got %q", checks[0].testCommand)
	}
}

func TestResolveChecks_SingleService(t *testing.T) {
	cfg := ValidateConfig{
		Services: []config.ServiceConfig{
			{Path: "web-app", TestCommand: "make test", LintCommand: "make stan"},
			{Path: "esg-api", TestCommand: "make tests", LintCommand: "make lint"},
		},
	}
	checks := resolveChecks([]string{"web-app/routes/api.php", "web-app/app/Models/User.php"}, cfg)
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].path != "web-app" {
		t.Errorf("expected web-app, got %q", checks[0].path)
	}
	if checks[0].lintCommand != "make stan" {
		t.Errorf("expected 'make stan', got %q", checks[0].lintCommand)
	}
}

func TestResolveChecks_MultipleServices(t *testing.T) {
	cfg := ValidateConfig{
		Services: []config.ServiceConfig{
			{Path: "web-app", LintCommand: "make stan"},
			{Path: "esg-api", LintCommand: "make lint"},
		},
	}
	checks := resolveChecks([]string{"web-app/app/Foo.php", "esg-api/app/main.py"}, cfg)
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %d", len(checks))
	}
	// Both services should be present
	paths := map[string]bool{}
	for _, c := range checks {
		paths[c.path] = true
	}
	if !paths["web-app"] || !paths["esg-api"] {
		t.Errorf("expected both web-app and esg-api, got %v", paths)
	}
}

func TestResolveChecks_UnmatchedFallsToRoot(t *testing.T) {
	cfg := ValidateConfig{
		TestCommand: "root-test",
		LintCommand: "root-lint",
		Services: []config.ServiceConfig{
			{Path: "web-app", LintCommand: "make stan"},
		},
	}
	// One file matches web-app, one doesn't
	checks := resolveChecks([]string{"web-app/app/Foo.php", "README.md"}, cfg)
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks (service + root), got %d", len(checks))
	}
}

func TestResolveChecks_NoMatchFallsToRoot(t *testing.T) {
	cfg := ValidateConfig{
		TestCommand: "root-test",
		Services: []config.ServiceConfig{
			{Path: "web-app", LintCommand: "make stan"},
		},
	}
	checks := resolveChecks([]string{"docs/README.md"}, cfg)
	if len(checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(checks))
	}
	if checks[0].testCommand != "root-test" {
		t.Errorf("expected root test command, got %q", checks[0].testCommand)
	}
}
