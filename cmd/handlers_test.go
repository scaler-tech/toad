package cmd

import (
	"testing"

	islack "github.com/scaler-tech/toad/internal/slack"
)

func TestIsDirectInteraction(t *testing.T) {
	tests := []struct {
		name string
		msg  *islack.IncomingMessage
		want bool
	}{
		{"mention", &islack.IncomingMessage{IsMention: true, IsTriggered: true}, true},
		{"keyword trigger", &islack.IncomingMessage{IsTriggered: true}, true},
		{"tadpole request on bot message", &islack.IncomingMessage{IsTadpoleRequest: true, IsTriggered: true, IsBot: true}, true},
		{"bot mention", &islack.IncomingMessage{IsMention: true, IsTriggered: true, IsBot: true}, false},
		{"plain message", &islack.IncomingMessage{}, false},
		{"bot message", &islack.IncomingMessage{IsBot: true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDirectInteraction(tt.msg); got != tt.want {
				t.Errorf("isDirectInteraction() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVacationPhrases(t *testing.T) {
	start := []string{
		"<@U0TOAD> you can now go on vacation",
		"Hey toad, VACATION TIME!",
	}
	for _, text := range start {
		if !isVacationStartPhrase(text) {
			t.Errorf("expected start phrase match: %q", text)
		}
		if isVacationEndPhrase(text) {
			t.Errorf("start phrase must not match end: %q", text)
		}
	}

	end := []string{
		"<@U0TOAD> welcome back from vacation!",
		"toad your vacation is over",
	}
	for _, text := range end {
		if !isVacationEndPhrase(text) {
			t.Errorf("expected end phrase match: %q", text)
		}
		if isVacationStartPhrase(text) {
			t.Errorf("end phrase must not match start: %q", text)
		}
	}

	neutral := "can you fix the export bug?"
	if isVacationStartPhrase(neutral) || isVacationEndPhrase(neutral) {
		t.Errorf("neutral text must not match: %q", neutral)
	}
}

func TestCanToggleVacation(t *testing.T) {
	if !canToggleVacation(nil, "U1") {
		t.Error("empty admin list should allow anyone")
	}
	if !canToggleVacation([]string{"U1", "U2"}, "U2") {
		t.Error("listed admin should be allowed")
	}
	if canToggleVacation([]string{"U1"}, "U3") {
		t.Error("unlisted user should be denied")
	}
}
