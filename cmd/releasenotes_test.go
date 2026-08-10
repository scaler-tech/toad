package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaler-tech/toad/internal/agent"
	"github.com/scaler-tech/toad/internal/config"
)

// --- decideReleaseAnnouncement (pure decision logic) ---

func TestDecideReleaseAnnouncement(t *testing.T) {
	tests := []struct {
		name          string
		lastAnnounced string
		current       string
		want          releaseAnnounceAction
	}{
		{"same version already announced", "v1.2.3", "v1.2.3", announceActionNone},
		{"empty lastAnnounced stores silently", "", "v1.2.3", announceActionStoreOnly},
		{"version changed announces", "v1.2.2", "v1.2.3", announceActionAnnounce},
		{"downgrade still announces", "v2.0.0", "v1.9.0", announceActionAnnounce},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideReleaseAnnouncement(tt.lastAnnounced, tt.current)
			if got != tt.want {
				t.Errorf("decideReleaseAnnouncement(%q, %q) = %v, want %v", tt.lastAnnounced, tt.current, got, tt.want)
			}
		})
	}
}

// --- announceReleaseIfNeeded orchestration ---

func TestAnnounceReleaseIfNeeded_EmptyChannelDisabled(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{} // ReleaseNotes.Channel is empty
	posted := false
	post := func(channel, text string) (string, error) {
		posted = true
		return "", nil
	}
	announceReleaseIfNeeded(context.Background(), cfg, db, nil, nil, post, "v1.0.0")
	if posted {
		t.Error("expected no post when release_notes.channel is empty")
	}
	if v, _ := db.GetSetting(releaseNotesSettingKey); v != "" {
		t.Errorf("expected no setting written when disabled, got %q", v)
	}
}

func TestAnnounceReleaseIfNeeded_FirstRunStoresSilently(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{ReleaseNotes: config.ReleaseNotesConfig{Channel: "toad-dev"}}
	posted := false
	post := func(channel, text string) (string, error) {
		posted = true
		return "", nil
	}
	announceReleaseIfNeeded(context.Background(), cfg, db, nil, nil, post, "v1.0.0")
	if posted {
		t.Error("expected no post on the first run that introduces the feature")
	}
	if v, _ := db.GetSetting(releaseNotesSettingKey); v != "v1.0.0" {
		t.Errorf("expected last_announced_version to be recorded as v1.0.0, got %q", v)
	}
}

func TestAnnounceReleaseIfNeeded_SameVersionNoPost(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting(releaseNotesSettingKey, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ReleaseNotes: config.ReleaseNotesConfig{Channel: "toad-dev"}}
	posted := false
	post := func(channel, text string) (string, error) {
		posted = true
		return "", nil
	}
	announceReleaseIfNeeded(context.Background(), cfg, db, nil, nil, post, "v1.0.0")
	if posted {
		t.Error("expected no post when current version matches last announced")
	}
}

func TestAnnounceReleaseIfNeeded_VersionChangePostsAndRecords(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting(releaseNotesSettingKey, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ReleaseNotes: config.ReleaseNotesConfig{Channel: "toad-dev"}}
	var gotChannel, gotText string
	post := func(channel, text string) (string, error) {
		gotChannel, gotText = channel, text
		return "123.456", nil
	}
	// No repos configured, so composeReleaseNotesMessage degrades to the
	// header-only message — exercises the full announce path without
	// needing a real repo or agent provider.
	announceReleaseIfNeeded(context.Background(), cfg, db, nil, nil, post, "v1.1.0")
	if gotChannel != "toad-dev" {
		t.Errorf("expected post to toad-dev, got %q", gotChannel)
	}
	if !strings.Contains(gotText, "v1.1.0") || !strings.Contains(gotText, "v1.0.0") {
		t.Errorf("expected posted text to mention both versions, got %q", gotText)
	}
	if v, _ := db.GetSetting(releaseNotesSettingKey); v != "v1.1.0" {
		t.Errorf("expected last_announced_version updated to v1.1.0, got %q", v)
	}
}

func TestAnnounceReleaseIfNeeded_PostFailureDoesNotRecord(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting(releaseNotesSettingKey, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ReleaseNotes: config.ReleaseNotesConfig{Channel: "toad-dev"}}
	post := func(channel, text string) (string, error) {
		return "", errors.New("channel not found")
	}
	announceReleaseIfNeeded(context.Background(), cfg, db, nil, nil, post, "v1.1.0")
	// A transient post failure must NOT update the setting, so the
	// announcement retries on the next restart.
	if v, _ := db.GetSetting(releaseNotesSettingKey); v != "v1.0.0" {
		t.Errorf("expected last_announced_version to stay v1.0.0 after a failed post, got %q", v)
	}
}

// --- findToadRepoPath ---

func TestFindToadRepoPath_ByModule(t *testing.T) {
	cfg := &config.Config{Repos: config.ReposConfig{List: []config.RepoConfig{
		{Name: "other", Path: "/other"},
		{Name: "toad-checkout", Path: "/some/toad/path"},
	}}}
	profiles := []config.RepoProfile{
		{Name: "other", Path: "/other", Module: "github.com/example/other"},
		{Name: "toad-checkout", Path: "/some/toad/path", Module: toadModulePath},
	}
	path, found := findToadRepoPath(cfg, profiles)
	if !found || path != "/some/toad/path" {
		t.Errorf("expected to find toad repo by module, got path=%q found=%v", path, found)
	}
}

func TestFindToadRepoPath_FallbackByName(t *testing.T) {
	cfg := &config.Config{Repos: config.ReposConfig{List: []config.RepoConfig{
		{Name: "toad", Path: "/fallback/toad"},
	}}}
	// No profile matches the module (detection failed) — falls back to name == "toad".
	profiles := []config.RepoProfile{{Name: "toad", Path: "/fallback/toad"}}
	path, found := findToadRepoPath(cfg, profiles)
	if !found || path != "/fallback/toad" {
		t.Errorf("expected fallback-by-name match, got path=%q found=%v", path, found)
	}
}

func TestFindToadRepoPath_NotFound(t *testing.T) {
	cfg := &config.Config{Repos: config.ReposConfig{List: []config.RepoConfig{
		{Name: "frontend", Path: "/frontend"},
	}}}
	profiles := []config.RepoProfile{{Name: "frontend", Path: "/frontend", Module: "example.com/frontend"}}
	_, found := findToadRepoPath(cfg, profiles)
	if found {
		t.Error("expected no match when no repo is toad's own checkout")
	}
}

// --- gitCommitDelta (real throwaway git repo) ---

func runGitOrFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v (%s)", args, err, out)
	}
}

func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitOrFail(t, dir, "init", "-q", "-b", "main")
	runGitOrFail(t, dir, "config", "user.email", "test@example.com")
	runGitOrFail(t, dir, "config", "user.name", "test")
	return dir
}

func commitFile(t *testing.T, dir, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(msg), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOrFail(t, dir, "add", name)
	runGitOrFail(t, dir, "commit", "-q", "-m", msg)
}

func TestGitCommitDelta_ExactRange(t *testing.T) {
	dir := setupTestRepo(t)
	commitFile(t, dir, "a.txt", "feat: first")
	runGitOrFail(t, dir, "tag", "v1.0.0")
	commitFile(t, dir, "b.txt", "fix: second")
	commitFile(t, dir, "c.txt", "feat: third")
	runGitOrFail(t, dir, "tag", "v1.1.0")

	delta, err := gitCommitDelta(context.Background(), dir, "1.0.0", "1.1.0")
	if err != nil {
		t.Fatalf("gitCommitDelta failed: %v", err)
	}
	if len(delta.Subjects) != 2 {
		t.Fatalf("expected 2 commit subjects, got %d: %v", len(delta.Subjects), delta.Subjects)
	}
	joined := strings.Join(delta.Subjects, " ")
	if !strings.Contains(joined, "fix: second") || !strings.Contains(joined, "feat: third") {
		t.Errorf("expected both commit subjects present, got %v", delta.Subjects)
	}
}

// A missing tag must ERROR, never summarize unrelated commits: three
// consecutive production announcements confidently described the previous
// releases because a recent-commits fallback fired when tags lagged. The
// caller degrades to a plain version line + GitHub compare link instead.
func TestGitCommitDelta_MissingTagErrors(t *testing.T) {
	dir := setupTestRepo(t)
	commitFile(t, dir, "a.txt", "feat: first")
	commitFile(t, dir, "b.txt", "fix: second")
	runGitOrFail(t, dir, "tag", "v1.1.0")
	// Deliberately no v1.0.0 tag — simulates a clone that lags tags.

	if _, err := gitCommitDelta(context.Background(), dir, "1.0.0", "1.1.0"); err == nil {
		t.Fatal("expected an error when the old tag is missing — summarizing the wrong range is worse than no notes")
	}
}

func TestVersionTag(t *testing.T) {
	if versionTag("0.2.13") != "v0.2.13" || versionTag("v0.2.13") != "v0.2.13" {
		t.Errorf("versionTag normalization broken: %q / %q", versionTag("0.2.13"), versionTag("v0.2.13"))
	}
}

func TestCompareURL(t *testing.T) {
	want := "https://github.com/scaler-tech/toad/compare/v0.2.12...v0.2.13"
	if got := compareURL("0.2.12", "0.2.13"); got != want {
		t.Errorf("compareURL = %q, want %q", got, want)
	}
}

func TestGitCommitDelta_NotAGitRepoErrors(t *testing.T) {
	dir := t.TempDir() // not initialized as a git repo at all
	_, err := gitCommitDelta(context.Background(), dir, "v1.0.0", "v1.1.0")
	if err == nil {
		t.Error("expected an error when the path isn't a git repo (both range and fallback fail)")
	}
}

// --- degradedReleaseNotes (deterministic fallback formatting) ---

func TestDegradedReleaseNotes_Basic(t *testing.T) {
	delta := commitDelta{Subjects: []string{"fix: a bug", "feat: a thing"}}
	got := degradedReleaseNotes(delta)
	want := "• fix: a bug\n• feat: a thing"
	if got != want {
		t.Errorf("degradedReleaseNotes = %q, want %q", got, want)
	}
}

func TestDegradedReleaseNotes_TruncatesToTen(t *testing.T) {
	subs := make([]string, 15)
	for i := range subs {
		subs[i] = "commit " + string(rune('a'+i))
	}
	delta := commitDelta{Subjects: subs}
	got := degradedReleaseNotes(delta)
	lines := strings.Split(got, "\n")
	if len(lines) != 10 {
		t.Errorf("expected exactly 10 lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "commit a") {
		t.Errorf("expected first line to be the first commit, got %q", lines[0])
	}
	if !strings.Contains(lines[9], "commit j") {
		t.Errorf("expected 10th line to be the 10th commit, got %q", lines[9])
	}
}

func TestDegradedReleaseNotes_Empty(t *testing.T) {
	got := degradedReleaseNotes(commitDelta{})
	if got != "" {
		t.Errorf("expected empty string for no subjects, got %q", got)
	}
}

// --- generateReleaseNotesText (agent success/failure/fallback) ---

func TestGenerateReleaseNotesText_Success(t *testing.T) {
	provider := &agent.MockProvider{RunResult: &agent.RunResult{Result: "  *bold* notes here  "}}
	delta := commitDelta{Subjects: []string{"fix: bug"}}
	got := generateReleaseNotesText(context.Background(), provider, "haiku", "v1.0.0", "v1.1.0", delta)
	if got != "*bold* notes here" {
		t.Errorf("expected trimmed model output, got %q", got)
	}
	if len(provider.RunCalls) != 1 {
		t.Fatalf("expected exactly one Run call, got %d", len(provider.RunCalls))
	}
	opts := provider.RunCalls[0]
	if opts.Model != "haiku" {
		t.Errorf("expected model=haiku, got %q", opts.Model)
	}
	if opts.Permissions != agent.PermissionNone {
		t.Errorf("expected PermissionNone, got %v", opts.Permissions)
	}
	if opts.Timeout.Seconds() != 60 {
		t.Errorf("expected 60s timeout, got %v", opts.Timeout)
	}
	if !strings.Contains(opts.Prompt, "fix: bug") {
		t.Errorf("expected prompt to include commit subject, got %q", opts.Prompt)
	}
	if !strings.Contains(opts.Prompt, "UNTRUSTED DATA") {
		t.Errorf("expected prompt to warn commit subjects are untrusted, got %q", opts.Prompt)
	}
}

func TestPrompt_IncludesProseStyleRules(t *testing.T) {
	p := buildReleaseNotesPrompt("v1.0.0", "v1.1.0", commitDelta{Subjects: []string{"fix: bug"}})
	if !strings.Contains(p, agent.ProseStyleRules) {
		t.Error("prompt missing the shared prose style rules")
	}
}

func TestGenerateReleaseNotesText_FailureFallsBackToDeterministic(t *testing.T) {
	provider := &agent.MockProvider{RunErr: errors.New("agent unavailable")}
	delta := commitDelta{Subjects: []string{"fix: bug", "feat: thing"}}
	got := generateReleaseNotesText(context.Background(), provider, "haiku", "v1.0.0", "v1.1.0", delta)
	want := degradedReleaseNotes(delta)
	if got != want {
		t.Errorf("expected deterministic fallback on failure, got %q, want %q", got, want)
	}
}

func TestGenerateReleaseNotesText_EmptyResultFallsBack(t *testing.T) {
	provider := &agent.MockProvider{RunResult: &agent.RunResult{Result: "   "}}
	delta := commitDelta{Subjects: []string{"fix: bug"}}
	got := generateReleaseNotesText(context.Background(), provider, "haiku", "v1.0.0", "v1.1.0", delta)
	want := degradedReleaseNotes(delta)
	if got != want {
		t.Errorf("expected deterministic fallback on empty model output, got %q, want %q", got, want)
	}
}

func TestGenerateReleaseNotesText_NilProviderFallsBack(t *testing.T) {
	delta := commitDelta{Subjects: []string{"fix: bug"}}
	got := generateReleaseNotesText(context.Background(), nil, "haiku", "v1.0.0", "v1.1.0", delta)
	want := degradedReleaseNotes(delta)
	if got != want {
		t.Errorf("expected deterministic fallback with nil provider, got %q, want %q", got, want)
	}
}

// --- composeReleaseNotesMessage (integration of the pieces above) ---

func TestComposeReleaseNotesMessage_NoRepoFoundHeaderOnly(t *testing.T) {
	cfg := &config.Config{Triage: config.TriageConfig{Model: "haiku"}}
	got := composeReleaseNotesMessage(context.Background(), cfg, nil, nil, "v1.0.0", "v1.1.0")
	want := ":frog: *Toad v1.1.0 is live* (from v1.0.0)"
	if got != want {
		t.Errorf("composeReleaseNotesMessage = %q, want %q", got, want)
	}
}

func TestComposeReleaseNotesMessage_WithRepoAndAgent(t *testing.T) {
	dir := setupTestRepo(t)
	commitFile(t, dir, "a.txt", "feat: first")
	runGitOrFail(t, dir, "tag", "v1.0.0")
	commitFile(t, dir, "b.txt", "fix: second")
	runGitOrFail(t, dir, "tag", "v1.1.0")

	cfg := &config.Config{
		Triage: config.TriageConfig{Model: "haiku"},
		Repos:  config.ReposConfig{List: []config.RepoConfig{{Name: "toad", Path: dir}}},
	}
	profiles := []config.RepoProfile{{Name: "toad", Path: dir, Module: toadModulePath}}
	provider := &agent.MockProvider{RunResult: &agent.RunResult{Result: "*Notes*: fixed a thing"}}

	got := composeReleaseNotesMessage(context.Background(), cfg, profiles, provider, "v1.0.0", "v1.1.0")
	if !strings.Contains(got, ":frog: *Toad v1.1.0 is live* (from v1.0.0)") {
		t.Errorf("expected header in message, got %q", got)
	}
	if !strings.Contains(got, "*Notes*: fixed a thing") {
		t.Errorf("expected generated notes in message, got %q", got)
	}
}
