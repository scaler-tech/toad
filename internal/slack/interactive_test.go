package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	goslack "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

func TestParseTicketAction_TicketButton(t *testing.T) {
	cb := &goslack.InteractionCallback{
		Type: goslack.InteractionTypeBlockActions,
		Channel: goslack.Channel{
			GroupConversation: goslack.GroupConversation{
				Conversation: goslack.Conversation{ID: "C123"},
			},
		},
		User:      goslack.User{ID: "U456", Name: "jamie"},
		MessageTs: "111.222",
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{
					ActionID: actionIDTicket,
					Value:    "999.888",
					BlockID:  "toad_ticket_actions",
				},
			},
		},
	}

	action, threadTS, channel, userID := parseTicketAction(cb)
	if !action {
		t.Fatal("expected action=true")
	}
	if threadTS != "999.888" {
		t.Errorf("expected threadTS '999.888', got %q", threadTS)
	}
	if channel != "C123" {
		t.Errorf("expected channel 'C123', got %q", channel)
	}
	if userID != "U456" {
		t.Errorf("expected userID 'U456', got %q", userID)
	}
}

func TestParseTicketAction_WrongAction(t *testing.T) {
	cb := &goslack.InteractionCallback{
		Type: goslack.InteractionTypeBlockActions,
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{ActionID: "something_else", Value: "999.888"},
			},
		},
	}

	action, _, _, _ := parseTicketAction(cb)
	if action {
		t.Fatal("expected action=false for non-toad action")
	}
}

// TestHandleInteractive_TicketAction drives handleInteractive end-to-end with
// a fake Slack API server, verifying that a block-actions callback on the
// "toad_ticket" button ultimately dispatches a message via c.handler with
// IsTicketRequest == true.
func TestHandleInteractive_TicketAction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.replies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{
						"type": "message",
						"user": "U999",
						"text": "Found a bug in utils/time.go",
						"ts":   "999.888",
					},
				},
			})
		default:
			// Covers both the "response_url" update endpoint and any other
			// incidental API call (e.g. user info lookups) — a bare ok:true
			// is enough for those code paths.
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer server.Close()

	api := goslack.New("test-token", goslack.OptionAPIURL(server.URL+"/"))
	c := &Client{
		api:     api,
		seen:    make(map[string]time.Time),
		replies: make(map[string]time.Time),
	}

	received := make(chan *IncomingMessage, 1)
	c.OnMessage(func(_ context.Context, msg *IncomingMessage) {
		received <- msg
	})

	cb := goslack.InteractionCallback{
		Type: goslack.InteractionTypeBlockActions,
		Channel: goslack.Channel{
			GroupConversation: goslack.GroupConversation{
				Conversation: goslack.Conversation{ID: "C123"},
			},
		},
		User:        goslack.User{ID: "U456", Name: "jamie"},
		ResponseURL: server.URL + "/response-url",
		ActionCallback: goslack.ActionCallbacks{
			BlockActions: []*goslack.BlockAction{
				{ActionID: actionIDTicket, Value: "999.888", BlockID: "toad_ticket_actions"},
			},
		},
	}

	handleInteractive(context.Background(), c, socketmode.Event{Data: cb})

	select {
	case msg := <-received:
		if !msg.IsTicketRequest {
			t.Errorf("expected IsTicketRequest=true, got false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not called within timeout")
	}
}
