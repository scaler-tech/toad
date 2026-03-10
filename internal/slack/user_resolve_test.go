package slack

import (
	"testing"
)

func TestFuzzyMatchSlackUser_ExactDisplayName(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "johndoe", RealName: "John Doe"},
		{ID: "U2", DisplayName: "janedoe", RealName: "Jane Doe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_CaseInsensitive(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "JohnDoe", RealName: "John Doe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_RealNameMatch(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "jd", RealName: "johndoe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_NameComponentMatch(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "jd", RealName: "John Doe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_NoMatch(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "alice", RealName: "Alice Smith"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_MultipleMatches_ReturnsEmpty(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "john", RealName: "John Smith"},
		{ID: "U2", DisplayName: "john", RealName: "John Brown"},
	}
	id := fuzzyMatchSlackUser("john", users)
	if id != "" {
		t.Errorf("expected empty for ambiguous match, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_FirstLastConcat(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "hergen", RealName: "Hergen Dillema"},
	}
	id := fuzzyMatchSlackUser("hergendillema", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}
