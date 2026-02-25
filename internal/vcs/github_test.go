package vcs

import "testing"

func TestExtractPRNumber_Valid(t *testing.T) {
	g := &GitHubProvider{}
	n, err := g.ExtractPRNumber("https://github.com/owner/repo/pull/42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 42 {
		t.Errorf("expected 42, got %d", n)
	}
}

func TestExtractPRNumber_TrailingSlash(t *testing.T) {
	g := &GitHubProvider{}
	n, err := g.ExtractPRNumber("https://github.com/owner/repo/pull/123/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 123 {
		t.Errorf("expected 123, got %d", n)
	}
}

func TestExtractPRNumber_LargeNumber(t *testing.T) {
	g := &GitHubProvider{}
	n, err := g.ExtractPRNumber("https://github.com/scaler-tech/scaler-mono/pull/9224")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 9224 {
		t.Errorf("expected 9224, got %d", n)
	}
}

func TestExtractPRNumber_NotAPRURL(t *testing.T) {
	g := &GitHubProvider{}
	_, err := g.ExtractPRNumber("https://github.com/owner/repo/issues/42")
	if err == nil {
		t.Error("expected error for non-PR URL")
	}
}

func TestExtractPRNumber_TooShort(t *testing.T) {
	g := &GitHubProvider{}
	_, err := g.ExtractPRNumber("https://github.com")
	if err == nil {
		t.Error("expected error for short URL")
	}
}

func TestExtractPRNumber_InvalidNumber(t *testing.T) {
	g := &GitHubProvider{}
	_, err := g.ExtractPRNumber("https://github.com/owner/repo/pull/abc")
	if err == nil {
		t.Error("expected error for non-numeric PR number")
	}
}

func TestExtractPRNumber_EmptyString(t *testing.T) {
	g := &GitHubProvider{}
	_, err := g.ExtractPRNumber("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}

func TestExtractRunID(t *testing.T) {
	g := &GitHubProvider{}
	tests := []struct {
		url    string
		expect string
	}{
		{"https://github.com/owner/repo/actions/runs/12345/job/67890", "12345"},
		{"https://github.com/owner/repo/actions/runs/99999", "99999"},
		{"https://github.com/owner/repo/actions/runs/12345/", "12345"},
		{"https://github.com/owner/repo/pull/42", ""},
		{"https://example.com/not-github", ""},
		{"", ""},
	}

	for _, tt := range tests {
		got := g.ExtractRunID(tt.url)
		if got != tt.expect {
			t.Errorf("ExtractRunID(%q) = %q, want %q", tt.url, got, tt.expect)
		}
	}
}
