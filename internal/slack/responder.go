package slack

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/slack-go/slack"
)

// ReplyInThread posts a message as a thread reply and tracks it as a toad reply.
func (c *Client) ReplyInThread(channel, threadTS, text string) (string, error) {
	_, ts, err := c.api.PostMessage(
		channel,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	)
	if err != nil {
		slog.Error("failed to reply in thread", "error", err, "channel", channel)
		return "", fmt.Errorf("posting thread reply: %w", err)
	}
	c.trackReply(channel, ts)
	return ts, nil
}

// React adds an emoji reaction to a message.
func (c *Client) React(channel, timestamp, emoji string) error {
	err := c.api.AddReaction(emoji, slack.ItemRef{
		Channel:   channel,
		Timestamp: timestamp,
	})
	if err != nil {
		if strings.Contains(err.Error(), "already_reacted") {
			slog.Debug("reaction already exists", "emoji", emoji)
			return nil
		}
		slog.Error("failed to add reaction", "error", err, "emoji", emoji)
		return fmt.Errorf("adding reaction: %w", err)
	}
	return nil
}

// RemoveReaction removes an emoji reaction from a message. Best-effort.
func (c *Client) RemoveReaction(channel, timestamp, emoji string) {
	err := c.api.RemoveReaction(emoji, slack.ItemRef{
		Channel:   channel,
		Timestamp: timestamp,
	})
	if err != nil && !strings.Contains(err.Error(), "no_reaction") {
		slog.Debug("failed to remove reaction", "error", err, "emoji", emoji)
	}
}

// SwapReaction removes one emoji and adds another on the same message.
func (c *Client) SwapReaction(channel, timestamp, remove, add string) {
	c.RemoveReaction(channel, timestamp, remove)
	c.React(channel, timestamp, add)
}

// GetPermalink returns a permanent URL to a specific Slack message.
func (c *Client) GetPermalink(channel, timestamp string) (string, error) {
	params := &slack.PermalinkParameters{
		Channel: channel,
		Ts:      timestamp,
	}
	link, err := c.api.GetPermalink(params)
	if err != nil {
		return "", fmt.Errorf("getting permalink: %w", err)
	}
	return link, nil
}

// UpdateMessage edits an existing message (for status updates).
func (c *Client) UpdateMessage(channel, timestamp, newText string) error {
	_, _, _, err := c.api.UpdateMessage(
		channel,
		timestamp,
		slack.MsgOptionText(newText, false),
	)
	if err != nil {
		slog.Error("failed to update message", "error", err)
		return fmt.Errorf("updating message: %w", err)
	}
	return nil
}
