package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/slack-go/slack"
)

// FixThisBlocks builds Block Kit blocks for a passive ribbit with a "Fix this" button.
func FixThisBlocks(text, threadTS string) []slack.Block {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
		nil, nil,
	)
	btn := slack.NewButtonBlockElement("toad_fix", threadTS,
		slack.NewTextBlockObject(slack.PlainTextType, "Let Toad fix this", false, false),
	)
	btn.WithStyle(slack.StylePrimary)
	actions := slack.NewActionBlock("toad_fix_actions", btn)
	return []slack.Block{section, actions}
}

// SpawnedByBlocks builds Block Kit blocks that replace the button after a tadpole is spawned.
// origBlocks are the original message blocks (with the button); the section text is preserved
// and the action block is replaced with a context line showing who triggered the fix.
func SpawnedByBlocks(origBlocks slack.Blocks, userName string) []slack.Block {
	var result []slack.Block
	for _, b := range origBlocks.BlockSet {
		// Keep all blocks except the action block (the button)
		if _, isAction := b.(*slack.ActionBlock); !isAction {
			result = append(result, b)
		}
	}
	statusText := ":hatching_chick: Tadpole spawned by " + userName
	if userName == "" {
		statusText = ":hourglass_flowing_sand: Spawning tadpole..."
	}
	result = append(result, slack.NewContextBlock("toad_fix_status",
		slack.NewTextBlockObject(slack.MarkdownType, statusText, false, false),
	))
	return result
}

// ReplyInThreadWithBlocks posts a Block Kit message as a thread reply and tracks it.
func (c *Client) ReplyInThreadWithBlocks(channel, threadTS, fallbackText string, blocks []slack.Block) (string, error) {
	if c.pathScrubber != nil {
		fallbackText = c.pathScrubber(fallbackText)
	}
	_, ts, err := c.api.PostMessage(
		channel,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionTS(threadTS),
	)
	if err != nil {
		slog.Error("failed to reply in thread with blocks", "error", err, "channel", channel)
		return "", fmt.Errorf("posting thread reply with blocks: %w", err)
	}
	c.trackReply(channel, ts)
	return ts, nil
}

// UpdateMessageWithBlocks updates an existing message with new blocks.
func (c *Client) UpdateMessageWithBlocks(channel, timestamp, fallbackText string, blocks []slack.Block) error {
	if c.pathScrubber != nil {
		fallbackText = c.pathScrubber(fallbackText)
	}
	_, _, _, err := c.api.UpdateMessage(
		channel,
		timestamp,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		slog.Error("failed to update message with blocks", "error", err)
		return fmt.Errorf("updating message with blocks: %w", err)
	}
	return nil
}

// ReplyInThread posts a message as a thread reply and tracks it as a toad reply.
func (c *Client) ReplyInThread(channel, threadTS, text string) (string, error) {
	if c.pathScrubber != nil {
		text = c.pathScrubber(text)
	}
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

// SetStatus shows a native Slack thinking indicator on a thread.
// The status text appears in the typing bar; loadingMessages control the inline
// loading indicator. If no loadingMessages are provided, the status text is
// used as the loading message so both displays stay consistent.
// The status auto-clears when the bot posts a reply to the thread, or after 2 minutes.
// Best-effort: errors are logged, not returned (purely cosmetic).
func (c *Client) SetStatus(channel, threadTS, status string, loadingMessages ...string) {
	if c.api == nil {
		return
	}
	// Default the inline loading message to match the status text,
	// otherwise Slack shows its own "Generating response..." default.
	if len(loadingMessages) == 0 && status != "" {
		loadingMessages = []string{status}
	}
	err := c.api.SetAssistantThreadsStatusContext(context.Background(), slack.AssistantThreadsSetStatusParameters{
		ChannelID:       channel,
		ThreadTS:        threadTS,
		Status:          status,
		LoadingMessages: loadingMessages,
	})
	if err != nil {
		slog.Debug("failed to set thread status", "error", err, "status", status)
	}
}

// ClearStatus explicitly clears the thinking indicator on a thread.
// Use on error paths where no reply will be posted to auto-clear it.
func (c *Client) ClearStatus(channel, threadTS string) {
	c.SetStatus(channel, threadTS, "")
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
	if c.pathScrubber != nil {
		newText = c.pathScrubber(newText)
	}
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
