package vcs

import "testing"

func TestGitLabExtractPRNumber(t *testing.T) {
	g := &GitLabProvider{}
	tests := []struct {
		url     string
		want    int
		wantErr bool
	}{
		{"https://gitlab.com/owner/repo/-/merge_requests/42", 42, false},
		{"https://gitlab.com/group/subgroup/repo/-/merge_requests/123", 123, false},
		{"https://gitlab.com/owner/repo/-/merge_requests/99/", 99, false},
		{"https://gitlab.example.com/org/project/-/merge_requests/7", 7, false},
		{"https://gitlab.com/owner/repo/-/issues/42", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		got, err := g.ExtractPRNumber(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ExtractPRNumber(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ExtractPRNumber(%q) = %d, want %d", tt.url, got, tt.want)
		}
	}
}

func TestGitLabExtractRunID(t *testing.T) {
	g := &GitLabProvider{}
	tests := []struct {
		url    string
		expect string
	}{
		{"https://gitlab.com/owner/repo/-/jobs/12345", "12345"},
		{"https://gitlab.com/owner/repo/-/jobs/99999/artifacts/browse", "99999"},
		{"https://gitlab.com/group/sub/repo/-/jobs/42", "42"},
		{"https://gitlab.com/owner/repo/-/pipelines/12345", ""},
		{"https://gitlab.com/owner/repo/-/merge_requests/1", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := g.ExtractRunID(tt.url)
		if got != tt.expect {
			t.Errorf("ExtractRunID(%q) = %q, want %q", tt.url, got, tt.expect)
		}
	}
}

func TestMapGitLabState(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"opened", "OPEN"},
		{"closed", "CLOSED"},
		{"merged", "MERGED"},
		{"locked", "CLOSED"},
	}

	for _, tt := range tests {
		got := mapGitLabState(tt.input)
		if got != tt.expect {
			t.Errorf("mapGitLabState(%q) = %q, want %q", tt.input, got, tt.expect)
		}
	}
}

func TestGitLabIsBotUsername(t *testing.T) {
	g := &GitLabProvider{BotUsernames: []string{"renovate-bot", "dependabot"}}

	if !g.isBotUsername("renovate-bot") {
		t.Error("expected renovate-bot to be identified as bot")
	}
	if !g.isBotUsername("Renovate-Bot") {
		t.Error("expected case-insensitive match")
	}
	if g.isBotUsername("human-user") {
		t.Error("expected human-user to not be identified as bot")
	}
}
