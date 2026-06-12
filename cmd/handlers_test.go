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
