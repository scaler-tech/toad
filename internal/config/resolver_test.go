package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolver_SingleRepo(t *testing.T) {
	dir := t.TempDir()
	repos := []RepoConfig{{Name: "only", Path: dir}}
	profiles := BuildProfiles(repos)
	r := NewResolver(profiles, repos)

	// Single repo always returns it, regardless of triage or hints.
	got := r.Resolve("", nil)
	if got == nil || got.Name != "only" {
		t.Fatalf("expected 'only' repo, got %v", got)
	}

	got = r.Resolve("nonexistent", []string{"foo.go"})
	if got == nil || got.Name != "only" {
		t.Fatalf("single repo should always resolve, got %v", got)
	}
}

func TestResolver_FileVerification(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Create a file only in dir2
	os.MkdirAll(filepath.Join(dir2, "internal"), 0o755)
	os.WriteFile(filepath.Join(dir2, "internal", "handler.go"), []byte("package handler"), 0o644)

	repos := []RepoConfig{
		{Name: "frontend", Path: dir1},
		{Name: "backend", Path: dir2},
	}
	profiles := BuildProfiles(repos)
	r := NewResolver(profiles, repos)

	// File hint exists in backend — should resolve to backend even if triage says frontend
	got := r.Resolve("frontend", []string{"internal/handler.go"})
	if got == nil || got.Name != "backend" {
		t.Errorf("expected 'backend' (file verification), got %v", got)
	}
}

func TestResolver_TriageFallback(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	repos := []RepoConfig{
		{Name: "frontend", Path: dir1},
		{Name: "backend", Path: dir2},
	}
	profiles := BuildProfiles(repos)
	r := NewResolver(profiles, repos)

	// No file hints, triage says backend — trust triage
	got := r.Resolve("backend", nil)
	if got == nil || got.Name != "backend" {
		t.Errorf("expected 'backend' (triage fallback), got %v", got)
	}
}

func TestResolver_PrimaryFallback(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	repos := []RepoConfig{
		{Name: "frontend", Path: dir1},
		{Name: "backend", Path: dir2, Primary: true},
	}
	profiles := BuildProfiles(repos)
	r := NewResolver(profiles, repos)

	// No triage repo, no file hints — fall back to primary
	got := r.Resolve("", nil)
	if got == nil || got.Name != "backend" {
		t.Errorf("expected 'backend' (primary fallback), got %v", got)
	}
}

func TestResolver_NoPrimary_ReturnsNil(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	repos := []RepoConfig{
		{Name: "frontend", Path: dir1},
		{Name: "backend", Path: dir2},
	}
	profiles := BuildProfiles(repos)
	r := NewResolver(profiles, repos)

	// No triage repo, no file hints, no primary — nil
	got := r.Resolve("", nil)
	if got != nil {
		t.Errorf("expected nil when no match and no primary, got %v", got)
	}
}

func TestResolver_FileConfirmsTriage(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// File exists in both repos
	os.WriteFile(filepath.Join(dir1, "README.md"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir2, "README.md"), []byte("hello"), 0o644)

	// But also a unique file in dir1
	os.WriteFile(filepath.Join(dir1, "app.tsx"), []byte("export default {}"), 0o644)

	repos := []RepoConfig{
		{Name: "frontend", Path: dir1},
		{Name: "backend", Path: dir2},
	}
	profiles := BuildProfiles(repos)
	r := NewResolver(profiles, repos)

	// File hints match frontend, triage agrees
	got := r.Resolve("frontend", []string{"app.tsx"})
	if got == nil || got.Name != "frontend" {
		t.Errorf("expected 'frontend' (file + triage agree), got %v", got)
	}
}

func TestResolver_UnknownTriageRepo(t *testing.T) {
	dir1 := t.TempDir()
	repos := []RepoConfig{{Name: "myrepo", Path: dir1, Primary: true}}
	profiles := BuildProfiles(repos)
	// Use single repo slice — but create resolver with 2 repos to avoid single-repo shortcut
	dir2 := t.TempDir()
	repos2 := []RepoConfig{
		{Name: "myrepo", Path: dir1, Primary: true},
		{Name: "other", Path: dir2},
	}
	profiles2 := BuildProfiles(repos2)
	r := NewResolver(profiles2, repos2)

	// Triage returns an unknown repo name — falls back to primary
	got := r.Resolve("nonexistent", nil)
	if got == nil || got.Name != "myrepo" {
		t.Errorf("expected 'myrepo' (primary fallback for unknown repo), got %v", got)
	}

	_ = profiles
}
