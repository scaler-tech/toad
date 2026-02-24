package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildProfiles_GoRepo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/test/myapp\n"), 0o644)
	os.Mkdir(filepath.Join(dir, "cmd"), 0o755)
	os.Mkdir(filepath.Join(dir, "internal"), 0o755)

	repos := []RepoConfig{{Name: "myapp", Path: dir}}
	profiles := BuildProfiles(repos)

	if len(profiles) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(profiles))
	}

	p := profiles[0]
	if p.Stack != "Go" {
		t.Errorf("expected stack 'Go', got %q", p.Stack)
	}
	if p.Module != "github.com/test/myapp" {
		t.Errorf("expected module 'github.com/test/myapp', got %q", p.Module)
	}
	if !contains(p.TopDirs, "cmd") || !contains(p.TopDirs, "internal") {
		t.Errorf("expected top dirs to include cmd and internal, got %v", p.TopDirs)
	}
	if !strings.Contains(p.Summary, "[myapp]") {
		t.Errorf("summary should contain repo name, got %q", p.Summary)
	}
	if !strings.Contains(p.Summary, "Go") {
		t.Errorf("summary should contain stack, got %q", p.Summary)
	}
}

func TestBuildProfiles_NodeRepo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name": "@company/frontend", "dependencies": {"react": "^18"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{}`), 0o644)
	os.Mkdir(filepath.Join(dir, "src"), 0o755)

	repos := []RepoConfig{{Name: "frontend", Path: dir}}
	profiles := BuildProfiles(repos)

	p := profiles[0]
	if p.Stack != "TypeScript/React" {
		t.Errorf("expected stack 'TypeScript/React', got %q", p.Stack)
	}
	if p.Module != "@company/frontend" {
		t.Errorf("expected module '@company/frontend', got %q", p.Module)
	}
}

func TestBuildProfiles_PythonRepo(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`[project]
name = "myservice"
`), 0o644)

	repos := []RepoConfig{{Name: "svc", Path: dir}}
	profiles := BuildProfiles(repos)

	p := profiles[0]
	if p.Stack != "Python" {
		t.Errorf("expected stack 'Python', got %q", p.Stack)
	}
	if p.Module != "myservice" {
		t.Errorf("expected module 'myservice', got %q", p.Module)
	}
}

func TestBuildProfiles_SkipsDotDirs(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	os.Mkdir(filepath.Join(dir, "node_modules"), 0o755)
	os.Mkdir(filepath.Join(dir, "src"), 0o755)

	repos := []RepoConfig{{Name: "test", Path: dir}}
	profiles := BuildProfiles(repos)

	p := profiles[0]
	if contains(p.TopDirs, ".git") {
		t.Error("should not include .git in top dirs")
	}
	if contains(p.TopDirs, "node_modules") {
		t.Error("should not include node_modules in top dirs")
	}
	if !contains(p.TopDirs, "src") {
		t.Error("should include src in top dirs")
	}
}

func TestBuildProfiles_ConfigDescription(t *testing.T) {
	dir := t.TempDir()

	repos := []RepoConfig{{Name: "test", Path: dir, Description: "My custom description"}}
	profiles := BuildProfiles(repos)

	p := profiles[0]
	if p.Description != "My custom description" {
		t.Errorf("expected config description, got %q", p.Description)
	}
}

func TestBuildProfiles_READMEFallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# My Project\n\nThis is a great project that does things.\n\nMore details here."), 0o644)

	repos := []RepoConfig{{Name: "test", Path: dir}}
	profiles := BuildProfiles(repos)

	p := profiles[0]
	if p.Description != "This is a great project that does things." {
		t.Errorf("expected first paragraph of README, got %q", p.Description)
	}
}

func TestFormatForPrompt(t *testing.T) {
	profiles := []RepoProfile{
		{Name: "frontend", Summary: "[frontend] TypeScript/React — web app"},
		{Name: "backend", Summary: "[backend] Go — API server"},
	}
	text := FormatForPrompt(profiles)
	if !strings.Contains(text, "Available repositories:") {
		t.Error("should contain header")
	}
	if !strings.Contains(text, "[frontend]") || !strings.Contains(text, "[backend]") {
		t.Error("should contain both repo summaries")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
