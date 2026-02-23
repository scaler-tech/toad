package slack

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/hergen/toad/internal/config"
)

// IncomingMessage represents a parsed Slack message ready for processing.
type IncomingMessage struct {
	Text           string
	Channel        string
	ChannelName    string
	User           string
	UserName       string
	Timestamp      string // message ts
	ThreadTimestamp string // parent thread ts (if reply)
	IsMention        bool
	IsTriggered      bool
	IsBot            bool
	IsTadpoleRequest bool // :frog: reaction on a toad reply — means "spawn tadpole"
	ThreadContext    []string
}

// ThreadTS returns the thread timestamp to use for replies.
// If the message is already in a thread, use the parent; otherwise use the message ts.
func (m *IncomingMessage) ThreadTS() string {
	if m.ThreadTimestamp != "" {
		return m.ThreadTimestamp
	}
	return m.Timestamp
}

// MessageHandler is called for each incoming message.
type MessageHandler func(ctx context.Context, msg *IncomingMessage)

// Client manages the Slack Socket Mode connection and event routing.
type Client struct {
	api        *slack.Client
	socket     *socketmode.Client
	channels   map[string]bool
	triggers   config.Triggers
	handler    MessageHandler
	botUserID  string
	seen       map[string]time.Time // dedup: key → first-seen time
	seenMu     sync.Mutex
	replies    map[string]time.Time // toad's own reply timestamps (channel:ts → sent time)
	repliesMu  sync.Mutex
}

// NewClient creates a new Slack client configured for Socket Mode.
func NewClient(cfg config.SlackConfig) *Client {
	api := slack.New(
		cfg.BotToken,
		slack.OptionAppLevelToken(cfg.AppToken),
	)

	socket := socketmode.New(api)

	channels := make(map[string]bool, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		channels[ch] = true
	}

	return &Client{
		api:      api,
		socket:   socket,
		channels: channels,
		triggers: cfg.Triggers,
		seen:     make(map[string]time.Time),
		replies:  make(map[string]time.Time),
	}
}

// OnMessage sets the handler for incoming messages.
func (c *Client) OnMessage(handler MessageHandler) {
	c.handler = handler
}

// Run starts the Socket Mode event loop. Blocks until context is cancelled.
func (c *Client) Run(ctx context.Context) error {
	// Identify ourselves so we can filter our own messages
	authResp, err := c.api.AuthTest()
	if err != nil {
		return fmt.Errorf("slack auth test: %w", err)
	}
	c.botUserID = authResp.UserID
	slog.Info("authenticated with Slack", "bot_user_id", c.botUserID, "team", authResp.Team)

	// Auto-join public channels the bot isn't already in
	c.autoJoinPublicChannels()

	go c.handleEvents(ctx)
	return c.socket.RunContext(ctx)
}

// autoJoinPublicChannels discovers all public channels and joins any the bot isn't in yet.
// Respects Slack rate limits by throttling join calls (~1/sec).
func (c *Client) autoJoinPublicChannels() {
	var joined, skipped int
	cursor := ""
	for {
		params := &slack.GetConversationsParameters{
			Types:           []string{"public_channel"},
			Limit:           200,
			Cursor:          cursor,
			ExcludeArchived: true,
		}
		channels, nextCursor, err := c.api.GetConversations(params)
		if err != nil {
			slog.Warn("failed to list public channels for auto-join", "error", err)
			return
		}
		for _, ch := range channels {
			if ch.IsMember {
				skipped++
				continue
			}
			if _, _, _, err := c.api.JoinConversation(ch.ID); err != nil {
				slog.Warn("failed to join channel", "channel", ch.Name, "error", err)
			} else {
				joined++
				slog.Debug("auto-joined channel", "channel", ch.Name)
			}
			// Slack Tier 3 rate limit: ~50 req/min for conversations.join
			time.Sleep(time.Second)
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	slog.Info("auto-join complete", "joined", joined, "already_member", skipped)
}

// inChannel checks if a channel should be monitored.
// If no explicit channels are configured, all channels are monitored.
func (c *Client) inChannel(channelID string) bool {
	if len(c.channels) == 0 {
		return true
	}
	return c.channels[channelID]
}

// API returns the underlying Slack API client for direct access.
func (c *Client) API() *slack.Client {
	return c.api
}

// FetchThreadMessages retrieves all messages in a thread.
func (c *Client) FetchThreadMessages(channel, threadTS string) ([]string, error) {
	msgs, _, _, err := c.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching thread: %w", err)
	}
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.Text)
	}
	return texts, nil
}

// FetchRecentMessages retrieves recent channel messages before the given timestamp.
// Messages are returned in chronological order (oldest first).
func (c *Client) FetchRecentMessages(channel, beforeTS string, limit int) ([]string, error) {
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channel,
		Latest:    beforeTS,
		Limit:     limit,
		Inclusive: false,
	}
	resp, err := c.api.GetConversationHistory(params)
	if err != nil {
		return nil, fmt.Errorf("fetching channel history: %w", err)
	}
	// Slack returns newest-first; reverse to chronological order
	texts := make([]string, len(resp.Messages))
	for i, m := range resp.Messages {
		texts[len(resp.Messages)-1-i] = m.Text
	}
	return texts, nil
}

// FetchMessage retrieves a single message by channel and timestamp.
func (c *Client) FetchMessage(channel, ts string) (*IncomingMessage, error) {
	msgs, _, _, err := c.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: ts,
		Limit:     1,
		Inclusive:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching message: %w", err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("message not found: %s/%s", channel, ts)
	}
	m := msgs[0]
	return &IncomingMessage{
		Text:           m.Text,
		Channel:        channel,
		User:           m.User,
		Timestamp:      m.Timestamp,
		ThreadTimestamp: m.ThreadTimestamp,
		IsBot:          m.BotID != "",
		IsTriggered:    true,
	}, nil
}

const seenTTL = 5 * time.Minute

// markSeen returns true if this message timestamp was already seen (duplicate).
// Uses channel+ts as key since ts is unique per channel.
// Entries expire after 5 minutes to bound memory without losing dedup protection.
func (c *Client) markSeen(channel, ts string) bool {
	key := channel + ":" + ts
	now := time.Now()

	c.seenMu.Lock()
	defer c.seenMu.Unlock()

	if _, exists := c.seen[key]; exists {
		return true
	}
	c.seen[key] = now

	// Prune expired entries periodically
	if len(c.seen) > 500 {
		for k, t := range c.seen {
			if now.Sub(t) > seenTTL {
				delete(c.seen, k)
			}
		}
	}
	return false
}

const replyTTL = 24 * time.Hour

// trackReply records a message timestamp as a toad reply.
func (c *Client) trackReply(channel, ts string) {
	key := channel + ":" + ts
	now := time.Now()

	c.repliesMu.Lock()
	defer c.repliesMu.Unlock()

	c.replies[key] = now

	// Prune old entries
	if len(c.replies) > 500 {
		for k, t := range c.replies {
			if now.Sub(t) > replyTTL {
				delete(c.replies, k)
			}
		}
	}
}

// IsToadReply checks if a message at channel+ts was sent by toad.
func (c *Client) IsToadReply(channel, ts string) bool {
	key := channel + ":" + ts
	c.repliesMu.Lock()
	defer c.repliesMu.Unlock()
	_, exists := c.replies[key]
	return exists
}

func (c *Client) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-c.socket.Events:
			if !ok {
				return
			}
			c.routeEvent(ctx, evt)
		}
	}
}

func (c *Client) routeEvent(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeEventsAPI:
		c.socket.Ack(*evt.Request)
		handleEventsAPI(ctx, c, evt)
	case socketmode.EventTypeConnecting:
		slog.Info("connecting to Slack...")
	case socketmode.EventTypeConnected:
		slog.Info("connected to Slack")
	case socketmode.EventTypeConnectionError:
		slog.Error("Slack connection error", "data", evt.Data)
	}
}
