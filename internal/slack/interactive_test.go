package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// TestHandleInteractive_FetchFailsThenSucceedsOnRetry exercises the Critical
// fix (C1): a FetchMessage failure must not be treated as fatal on the first
// attempt — handleInteractive retries once (with a brief sleep) and, when the
// retry succeeds, proceeds exactly as the happy path would: the handler is
// still dispatched with IsTicketRequest=true, and no error reply is posted.
func TestHandleInteractive_FetchFailsThenSucceedsOnRetry(t *testing.T) {
	var repliesCalls atomic.Int32
	var postMessages []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.replies":
			if repliesCalls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "internal_error"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"type": "message", "user": "U999", "text": "Found a bug in utils/time.go", "ts": "999.888"},
				},
			})
		case "/chat.postMessage":
			body, _ := io.ReadAll(r.Body)
			// chat.postMessage bodies are form-url-encoded — decode so the
			// test's substring checks can match the plain-text message.
			decoded, err := url.QueryUnescape(string(body))
			if err != nil {
				decoded = string(body)
			}
			mu.Lock()
			postMessages = append(postMessages, decoded)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "111.111"})
		default:
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
	case <-time.After(4 * time.Second):
		t.Fatal("handler was not called within timeout after a retried fetch succeeded")
	}

	if repliesCalls.Load() != 2 {
		t.Errorf("expected exactly 2 conversations.replies calls (initial + 1 retry), got %d", repliesCalls.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range postMessages {
		if strings.Contains(m, "couldn't start the ticket flow") {
			t.Errorf("expected no error reply to be posted on a retry-succeeds path, got %q", m)
		}
	}
}

// TestHandleInteractive_FetchFailsTwice_PostsErrorAndRestoresButton exercises
// the Critical fix (C1)'s failure path: when FetchMessage fails on both the
// initial attempt and the retry, handleInteractive must never silently drop
// the click. It must (a) post a visible error reply in the thread, and (b)
// rewrite the button back to a clickable (non-final) state via response_url
// so the click can be repeated, rather than leaving the button claiming
// success (the prior, misleading behavior) or stuck showing "in progress"
// forever.
func TestHandleInteractive_FetchFailsTwice_PostsErrorAndRestoresButton(t *testing.T) {
	var repliesCalls atomic.Int32
	var postMessages []string
	var responseURLBodies []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.replies":
			repliesCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "internal_error"})
		case "/chat.postMessage":
			body, _ := io.ReadAll(r.Body)
			// chat.postMessage bodies are form-url-encoded — decode so the
			// test's substring checks can match the plain-text message.
			decoded, err := url.QueryUnescape(string(body))
			if err != nil {
				decoded = string(body)
			}
			mu.Lock()
			postMessages = append(postMessages, decoded)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "111.111"})
		case "/response-url":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			responseURLBodies = append(responseURLBodies, string(body))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
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

	handlerCalled := make(chan struct{}, 1)
	c.OnMessage(func(_ context.Context, msg *IncomingMessage) {
		handlerCalled <- struct{}{}
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

	// Give the background goroutine time to run through both fetch attempts
	// (including the ~1s retry sleep) plus the error reply/button restore.
	deadline := time.After(4 * time.Second)
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	gotErrorReply := false
waitLoop:
	for {
		select {
		case <-handlerCalled:
			t.Fatal("handler must not be dispatched when FetchMessage fails on both attempts")
		case <-poll.C:
			mu.Lock()
			for _, m := range postMessages {
				if strings.Contains(m, "couldn't start the ticket flow") {
					gotErrorReply = true
				}
			}
			mu.Unlock()
			if gotErrorReply && repliesCalls.Load() >= 2 {
				break waitLoop
			}
		case <-deadline:
			break waitLoop
		}
	}

	if repliesCalls.Load() != 2 {
		t.Errorf("expected exactly 2 conversations.replies calls (initial + 1 retry), got %d", repliesCalls.Load())
	}
	if !gotErrorReply {
		t.Fatal("expected an error reply containing 'couldn't start the ticket flow' to be posted")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(responseURLBodies) == 0 {
		t.Fatal("expected at least one response_url call")
	}
	last := responseURLBodies[len(responseURLBodies)-1]
	if strings.Contains(last, "Ticket requested by") {
		t.Errorf("expected the final response_url call to restore the button (not leave 'Ticket requested by' final/in-progress wording), got %q", last)
	}
}
