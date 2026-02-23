package reviewer

import "testing"

func TestExtractPRNumber_Valid(t *testing.T) {
	n, err := ExtractPRNumber("https://github.com/owner/repo/pull/42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 42 {
		t.Errorf("expected 42, got %d", n)
	}
}

func TestExtractPRNumber_TrailingSlash(t *testing.T) {
	n, err := ExtractPRNumber("https://github.com/owner/repo/pull/123/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 123 {
		t.Errorf("expected 123, got %d", n)
	}
}

func TestExtractPRNumber_LargeNumber(t *testing.T) {
	n, err := ExtractPRNumber("https://github.com/scaler-tech/scaler-mono/pull/9224")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 9224 {
		t.Errorf("expected 9224, got %d", n)
	}
}

func TestExtractPRNumber_NotAPRURL(t *testing.T) {
	_, err := ExtractPRNumber("https://github.com/owner/repo/issues/42")
	if err == nil {
		t.Error("expected error for non-PR URL")
	}
}

func TestExtractPRNumber_TooShort(t *testing.T) {
	_, err := ExtractPRNumber("https://github.com")
	if err == nil {
		t.Error("expected error for short URL")
	}
}

func TestExtractPRNumber_InvalidNumber(t *testing.T) {
	_, err := ExtractPRNumber("https://github.com/owner/repo/pull/abc")
	if err == nil {
		t.Error("expected error for non-numeric PR number")
	}
}

func TestExtractPRNumber_EmptyString(t *testing.T) {
	_, err := ExtractPRNumber("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}
