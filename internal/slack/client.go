// Package slack provides the Socket Mode client and event routing.
package slack

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/scaler-tech/toad/internal/config"
)

// IncomingMessage represents a parsed Slack message ready for processing.
type IncomingMessage struct {
	Text             string
	Channel          string
	ChannelName      string
	User             string
	UserName         string
	Timestamp        string // message ts
	ThreadTimestamp  string // parent thread ts (if reply)
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
	api          *slack.Client
	socket       *socketmode.Client
	cfgChannels  map[string]bool // channel names from config (empty = all)
	channels     map[string]bool // resolved channel IDs to monitor (empty when cfgChannels is empty)
	triggers     config.Triggers
	handler      MessageHandler
	botUserID    string
	seen         map[string]time.Time // dedup: key → first-seen time
	seenMu       sync.Mutex
	replies      map[string]time.Time // toad's own reply timestamps (channel:ts → sent time)
	repliesMu    sync.Mutex
	pathScrubber func(string) string  // replaces absolute paths with repo-relative
	mcpHandler   *SlashCommandHandler // handles /toad slash commands
}

// NewClient creates a new Slack client configured for Socket Mode.
func NewClient(cfg config.SlackConfig) *Client {
	api := slack.New(
		cfg.BotToken,
		slack.OptionAppLevelToken(cfg.AppToken),
	)

	socket := socketmode.New(api)

	// Store configured channel names; resolved to IDs during Run().
	cfgChannels := make(map[string]bool, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		cfgChannels[ch] = true
	}

	return &Client{
		api:         api,
		socket:      socket,
		cfgChannels: cfgChannels,
		channels:    make(map[string]bool),
		triggers:    cfg.Triggers,
		seen:        make(map[string]time.Time),
		replies:     make(map[string]time.Time),
	}
}

// OnMessage sets the handler for incoming messages.
func (c *Client) OnMessage(handler MessageHandler) {
	c.handler = handler
}

// Run starts the Socket Mode event loop. Blocks until context is canceled.
func (c *Client) Run(ctx context.Context) error {
	// Identify ourselves so we can filter our own messages
	authResp, err := c.api.AuthTest()
	if err != nil {
		return fmt.Errorf("slack auth test: %w", err)
	}
	c.botUserID = authResp.UserID
	slog.Info("authenticated with Slack", "bot_user_id", c.botUserID, "team", authResp.Team)

	// Join channels: if specific channels are configured, only join those;
	// otherwise join all public channels.
	if len(c.cfgChannels) > 0 {
		c.joinConfiguredChannels()
	} else {
		c.joinAllPublicChannels()
	}

	go c.handleEvents(ctx)
	return c.socket.RunContext(ctx)
}

// joinConfiguredChannels resolves configured channel names to IDs and joins only those.
// Lists both public and private channels so private channels the bot was invited to
// are also resolved and monitored.
func (c *Client) joinConfiguredChannels() {
	var joined, alreadyMember int
	resolved := make(map[string]bool) // track which config names were found
	for _, chType := range []string{"public_channel", "private_channel"} {
		cursor := ""
		for {
			params := &slack.GetConversationsParameters{
				Types:           []string{chType},
				Limit:           200,
				Cursor:          cursor,
				ExcludeArchived: true,
			}
			channels, nextCursor, err := c.api.GetConversations(params)
			if err != nil {
				slog.Warn("failed to list channels", "type", chType, "error", err)
				break
			}
			for _, ch := range channels {
				if !c.cfgChannels[ch.Name] {
					continue
				}
				c.channels[ch.ID] = true
				resolved[ch.Name] = true
				if ch.IsMember {
					alreadyMember++
					continue
				}
				// Can only join public channels; private channels require an invite.
				if chType == "public_channel" {
					if _, _, _, err := c.api.JoinConversation(ch.ID); err != nil {
						slog.Warn("failed to join channel", "channel", ch.Name, "error", err)
					} else {
						joined++
						slog.Debug("joined channel", "channel", ch.Name)
					}
					time.Sleep(time.Second)
				}
			}
			if nextCursor == "" {
				break
			}
			cursor = nextCursor
		}
	}
	for name := range c.cfgChannels {
		if !resolved[name] {
			slog.Warn("configured channel not found — check name or invite toad for private channels", "channel", name)
		}
	}
	slog.Info("channel join complete", "joined", joined, "already_member", alreadyMember)
}

// joinAllPublicChannels discovers all public channels and joins any the bot isn't in yet.
func (c *Client) joinAllPublicChannels() {
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
// If channels are configured, only resolved channel IDs pass.
func (c *Client) inChannel(channelID string) bool {
	if len(c.cfgChannels) == 0 {
		return true
	}
	return c.channels[channelID]
}

// API returns the underlying Slack API client for direct access.
func (c *Client) API() *slack.Client {
	return c.api
}

// SetMCPHandler configures the handler for /toad slash commands.
func (c *Client) SetMCPHandler(h *SlashCommandHandler) {
	c.mcpHandler = h
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
		texts = append(texts, extractFullText(m.Msg))
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
		texts[len(resp.Messages)-1-i] = extractFullText(m.Msg)
	}
	return texts, nil
}

// extractFullText builds a complete text representation of a Slack message,
// including content from blocks and attachments that bots (Sentry, CI, etc.)
// typically use for rich formatting like stack traces and error details.
func extractFullText(m slack.Msg) string {
	var parts []string

	// Start with the plain text field
	if m.Text != "" {
		parts = append(parts, m.Text)
	}

	// Extract text from Block Kit blocks
	for _, block := range m.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.SectionBlock:
			if b.Text != nil && b.Text.Text != "" {
				parts = appendUnique(parts, b.Text.Text)
			}
			for _, f := range b.Fields {
				if f != nil && f.Text != "" {
					parts = appendUnique(parts, f.Text)
				}
			}
		case *slack.HeaderBlock:
			if b.Text != nil && b.Text.Text != "" {
				parts = appendUnique(parts, b.Text.Text)
			}
		case *slack.RichTextBlock:
			if text := extractRichText(b); text != "" {
				parts = appendUnique(parts, text)
			}
		case *slack.ContextBlock:
			for _, elem := range b.ContextElements.Elements {
				if te, ok := elem.(*slack.TextBlockObject); ok && te.Text != "" {
					parts = appendUnique(parts, te.Text)
				}
			}
		}
	}

	// Extract text from attachments (legacy format — Sentry, GitHub, etc. use these heavily)
	for _, att := range m.Attachments {
		if att.Pretext != "" {
			parts = appendUnique(parts, att.Pretext)
		}
		if att.Title != "" {
			parts = appendUnique(parts, att.Title)
		}
		if att.Text != "" {
			parts = appendUnique(parts, att.Text)
		}
		for _, f := range att.Fields {
			fieldText := f.Title + ": " + f.Value
			if f.Title == "" {
				fieldText = f.Value
			}
			if fieldText != "" {
				parts = appendUnique(parts, fieldText)
			}
		}
		if att.Fallback != "" && len(parts) == 0 {
			// Only use fallback if we got nothing else
			parts = append(parts, att.Fallback)
		}
	}

	return strings.Join(parts, "\n")
}

// extractRichText pulls plain text from a RichTextBlock's nested elements.
func extractRichText(b *slack.RichTextBlock) string {
	var sb strings.Builder
	for _, elem := range b.Elements {
		switch section := elem.(type) {
		case *slack.RichTextSection:
			for _, se := range section.Elements {
				switch te := se.(type) {
				case *slack.RichTextSectionTextElement:
					sb.WriteString(te.Text)
				case *slack.RichTextSectionLinkElement:
					if te.Text != "" {
						sb.WriteString(te.Text)
					} else {
						sb.WriteString(te.URL)
					}
				}
			}
		case *slack.RichTextPreformatted:
			for _, se := range section.Elements {
				if te, ok := se.(*slack.RichTextSectionTextElement); ok {
					sb.WriteString(te.Text)
				}
			}
		case *slack.RichTextQuote:
			for _, se := range section.Elements {
				if te, ok := se.(*slack.RichTextSectionTextElement); ok {
					sb.WriteString(te.Text)
				}
			}
		case *slack.RichTextList:
			for _, listElem := range section.Elements {
				if listSection, ok := listElem.(*slack.RichTextSection); ok {
					for _, se := range listSection.Elements {
						if te, ok := se.(*slack.RichTextSectionTextElement); ok {
							sb.WriteString(te.Text)
						}
					}
					sb.WriteString("\n")
				}
			}
		}
	}
	return strings.TrimSpace(sb.String())
}

// appendUnique appends text only if it's not already a substring of an existing part.
// Prevents duplication since .Text is often a subset of blocks/attachments content.
func appendUnique(parts []string, text string) []string {
	for _, existing := range parts {
		if strings.Contains(existing, text) {
			return parts
		}
	}
	return append(parts, text)
}

// FetchMessage retrieves a single message by channel and timestamp.
func (c *Client) FetchMessage(channel, ts string) (*IncomingMessage, error) {
	msgs, _, _, err := c.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: ts,
		Limit:     1,
		Inclusive: true,
	})
	if err != nil {
		return nil, fmt.Errorf("fetching message: %w", err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("message not found: %s/%s", channel, ts)
	}
	m := msgs[0]
	return &IncomingMessage{
		Text:            extractFullText(m.Msg),
		Channel:         channel,
		User:            m.User,
		Timestamp:       m.Timestamp,
		ThreadTimestamp: m.ThreadTimestamp,
		IsBot:           m.BotID != "",
		IsTriggered:     true,
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
	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			return
		}
		c.socket.Ack(*evt.Request)
		handleSlashCommand(c, cmd)
	case socketmode.EventTypeInteractive:
		c.socket.Ack(*evt.Request)
		handleInteractive(ctx, c, evt)
	case socketmode.EventTypeConnecting:
		slog.Info("connecting to Slack...")
	case socketmode.EventTypeConnected:
		slog.Info("connected to Slack")
	case socketmode.EventTypeConnectionError:
		slog.Error("Slack connection error", "data", evt.Data)
	}
}

// SetPathScrubber configures the client to replace absolute filesystem paths
// in outbound messages with repo names. repoPaths maps absolute path → repo name.
func (c *Client) SetPathScrubber(repoPaths map[string]string) {
	if len(repoPaths) == 0 {
		return
	}
	c.pathScrubber = buildPathScrubber(repoPaths)
}

// buildPathScrubber creates a function that replaces absolute paths with repo names.
// Paths are sorted longest-first to avoid partial matches.
func buildPathScrubber(repoPaths map[string]string) func(string) string {
	type entry struct {
		path string
		name string
	}
	entries := make([]entry, 0, len(repoPaths))
	for p, n := range repoPaths {
		entries = append(entries, entry{path: p, name: n})
	}
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].path) > len(entries[j].path)
	})
	return func(text string) string {
		for _, e := range entries {
			text = strings.ReplaceAll(text, e.path, "<"+e.name+">")
		}
		return text
	}
}
