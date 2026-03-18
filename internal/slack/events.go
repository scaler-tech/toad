package slack

import (
	"context"
	"log/slog"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

func handleEventsAPI(ctx context.Context, c *Client, evt socketmode.Event) {
	eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}

	eventType := eventsAPI.InnerEvent.Type

	// Fast-path: reject our own events before entering individual handlers
	if userID := extractUserID(eventsAPI.InnerEvent.Data); userID == c.botUserID {
		slog.Debug("skipping: self-event", "type", eventType)
		return
	}

	slog.Debug("event received", "type", eventType)

	switch ev := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		handleAppMention(ctx, c, ev)
	case *slackevents.MessageEvent:
		handleMessage(ctx, c, ev)
	case *slackevents.ReactionAddedEvent:
		handleReaction(ctx, c, ev)
	default:
		slog.Debug("unhandled event type", "type", eventType)
	}
}

// extractUserID pulls the User field from any event type we care about.
func extractUserID(ev interface{}) string {
	switch e := ev.(type) {
	case *slackevents.AppMentionEvent:
		return e.User
	case *slackevents.MessageEvent:
		return e.User
	case *slackevents.ReactionAddedEvent:
		return e.User
	}
	return ""
}

func handleAppMention(ctx context.Context, c *Client, ev *slackevents.AppMentionEvent) {
	slog.Debug("app_mention event", "channel", ev.Channel, "user", ev.User, "bot_id", ev.BotID)

	if !c.inChannel(ev.Channel) {
		slog.Debug("skipping: unmonitored channel", "channel", ev.Channel)
		return
	}
	if c.markSeen(ev.Channel, ev.TimeStamp) {
		slog.Debug("skipping: duplicate message", "ts", ev.TimeStamp)
		return
	}

	msg := &IncomingMessage{
		Text:            ev.Text,
		Channel:         ev.Channel,
		User:            ev.User,
		Timestamp:       ev.TimeStamp,
		ThreadTimestamp: ev.ThreadTimeStamp,
		IsMention:       true,
		IsTriggered:     true,
		IsBot:           ev.BotID != "",
		BotID:           ev.BotID,
	}

	slog.Info("app mention received", "channel", ev.Channel, "user", ev.User)
	slog.Debug("dispatching message", "mention", msg.IsMention, "triggered", msg.IsTriggered, "bot", msg.IsBot)
	if c.handler != nil {
		c.handler(ctx, msg)
	}
}

func handleMessage(ctx context.Context, c *Client, ev *slackevents.MessageEvent) {
	slog.Debug("message event", "channel", ev.Channel, "user", ev.User, "bot_id", ev.BotID, "subtype", ev.SubType)

	if !c.inChannel(ev.Channel) {
		slog.Debug("skipping: unmonitored channel", "channel", ev.Channel)
		return
	}
	// Ignore message edits/deletes (but let bot messages through for digest)
	if ev.SubType != "" && ev.SubType != "bot_message" {
		slog.Debug("skipping: message subtype", "subtype", ev.SubType)
		return
	}

	// Extract full text including blocks/attachments. The custom MessageEvent
	// unmarshaler always populates ev.Message with a full slack.Msg, so we get
	// rich content from bot messages (Sentry alerts, CI, etc.) instead of just
	// the bare fallback text in ev.Text.
	fullText := ev.Text
	if ev.Message != nil {
		fullText = extractFullText(*ev.Message)
	}

	// Skip @mentions — these are handled by handleAppMention
	if c.botUserID != "" && strings.Contains(fullText, "<@"+c.botUserID+">") {
		slog.Debug("skipping: mention handled by app_mention", "user", ev.User)
		return
	}
	if c.markSeen(ev.Channel, ev.TimeStamp) {
		slog.Debug("skipping: duplicate message", "ts", ev.TimeStamp)
		return
	}

	isBot := ev.BotID != ""

	// Skip toad's own bot_messages. The self-event filter in handleEventsAPI
	// checks User, but bot_messages have an empty User field and use BotID
	// instead. Check tracked reply timestamps to identify our own messages.
	if isBot && c.IsToadReply(ev.Channel, ev.TimeStamp) {
		slog.Debug("skipping: self bot_message", "ts", ev.TimeStamp)
		return
	}

	triggered := !isBot && hasKeywordTrigger(fullText, c.triggers.Keywords)

	// Personality feedback: thread replies to Toad messages (non-mention, non-bot).
	if ev.ThreadTimeStamp != "" && !isBot && c.personalityTextHandler != nil {
		if c.IsToadReply(ev.Channel, ev.ThreadTimeStamp) {
			c.personalityTextHandler(ctx, fullText, ev.Channel, ev.ThreadTimeStamp)
			// Don't return — the message should still be dispatched to the regular handler
		}
	}

	msg := &IncomingMessage{
		Text:            fullText,
		Channel:         ev.Channel,
		User:            ev.User,
		Timestamp:       ev.TimeStamp,
		ThreadTimestamp: ev.ThreadTimeStamp,
		IsMention:       false,
		IsTriggered:     triggered,
		IsBot:           isBot,
		BotID:           ev.BotID,
	}

	slog.Debug("dispatching message", "channel", ev.Channel, "triggered", triggered, "bot", isBot)
	if c.handler != nil {
		c.handler(ctx, msg)
	}
}

func handleReaction(ctx context.Context, c *Client, ev *slackevents.ReactionAddedEvent) {
	slog.Debug("reaction event", "emoji", ev.Reaction, "user", ev.User, "channel", ev.Item.Channel)

	// Personality feedback: non-trigger reactions on Toad's own messages.
	if ev.Reaction != c.triggers.Emoji && c.personalityHandler != nil {
		if c.inChannel(ev.Item.Channel) && c.IsToadReply(ev.Item.Channel, ev.Item.Timestamp) {
			c.personalityHandler(ctx, ev.Reaction, ev.Item.Channel, ev.Item.Timestamp)
			return
		}
	}

	if ev.Reaction != c.triggers.Emoji {
		slog.Debug("skipping: non-trigger emoji", "emoji", ev.Reaction, "trigger", c.triggers.Emoji)
		return
	}
	if !c.inChannel(ev.Item.Channel) {
		slog.Debug("skipping: unmonitored channel", "channel", ev.Item.Channel)
		return
	}
	if c.markSeen(ev.Item.Channel, "react:"+ev.Item.Timestamp) {
		slog.Debug("skipping: duplicate reaction", "ts", ev.Item.Timestamp)
		return
	}

	// Check if reaction is on a toad reply (tadpole request) or on a user message (triage trigger)
	isTadpoleRequest := c.IsToadReply(ev.Item.Channel, ev.Item.Timestamp)

	slog.Info("trigger reaction received",
		"emoji", ev.Reaction, "channel", ev.Item.Channel, "tadpole_request", isTadpoleRequest)

	// Fetch the message that was reacted to
	msg, err := c.FetchMessage(ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		slog.Error("failed to fetch reacted message", "error", err)
		return
	}
	msg.IsTriggered = true
	msg.IsTadpoleRequest = isTadpoleRequest

	slog.Debug("dispatching message", "triggered", true, "tadpole_request", isTadpoleRequest, "bot", msg.IsBot)
	if c.handler != nil {
		c.handler(ctx, msg)
	}
}

func hasKeywordTrigger(text string, keywords []string) bool {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// ResolveUserName fetches the display name for a Slack user ID.
func (c *Client) ResolveUserName(userID string) string {
	user, err := c.api.GetUserInfo(userID)
	if err != nil {
		return userID
	}
	if user.Profile.DisplayName != "" {
		return user.Profile.DisplayName
	}
	return user.RealName
}

// ResolveChannelName returns the name for a Slack channel ID, using a cache
// to avoid redundant API calls on every incoming message.
func (c *Client) ResolveChannelName(channelID string) string {
	c.channelNamesMu.RLock()
	if name, ok := c.channelNames[channelID]; ok {
		c.channelNamesMu.RUnlock()
		return name
	}
	c.channelNamesMu.RUnlock()

	ch, err := c.api.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		return channelID
	}

	c.channelNamesMu.Lock()
	c.channelNames[channelID] = ch.Name
	c.channelNamesMu.Unlock()
	return ch.Name
}
