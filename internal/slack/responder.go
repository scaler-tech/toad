package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/slack-go/slack"
)

// TicketBlocks builds Block Kit blocks for a passive ribbit with a
// "Create Linear ticket" button.
func TicketBlocks(text, threadTS string) []slack.Block {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
		nil, nil,
	)
	btn := slack.NewButtonBlockElement(actionIDTicket, threadTS,
		slack.NewTextBlockObject(slack.PlainTextType, "Create Linear ticket", false, false),
	)
	btn.WithStyle(slack.StylePrimary)
	actions := slack.NewActionBlock("toad_ticket_actions", btn)
	return []slack.Block{section, actions}
}

// TicketedByBlocks builds Block Kit blocks that replace the button after a
// ticket is requested. origBlocks are the original message blocks (with the
// button); the section text is preserved and the action block is replaced
// with a context line showing who triggered the ticket request.
func TicketedByBlocks(origBlocks slack.Blocks, userName string) []slack.Block {
	var result []slack.Block
	for _, b := range origBlocks.BlockSet {
		// Keep all blocks except the action block (the button)
		if _, isAction := b.(*slack.ActionBlock); !isAction {
			result = append(result, b)
		}
	}
	statusText := ":ticket: Ticket requested by " + userName
	if userName == "" {
		statusText = ":ticket: Creating ticket..."
	}
	result = append(result, slack.NewContextBlock("toad_ticket_status",
		slack.NewTextBlockObject(slack.MarkdownType, statusText, false, false),
	))
	return result
}

// TicketInProgressBlocks builds Block Kit blocks showing the ticket flow is
// still in progress for a resolved requester — used between resolving the
// clicking user's name and confirming the fetch/dispatch that actually
// starts the flow has succeeded. Distinct from TicketedByBlocks' final
// ":ticket: Ticket requested by <user>" wording, which implies the flow has
// already started; this is truthful-in-progress instead (Critical fix: the
// prior code flipped straight to the final wording before FetchMessage had
// even run, so a fetch failure left the button falsely claiming success).
func TicketInProgressBlocks(origBlocks slack.Blocks, userName string) []slack.Block {
	var result []slack.Block
	for _, b := range origBlocks.BlockSet {
		// Keep all blocks except the action block (the button)
		if _, isAction := b.(*slack.ActionBlock); !isAction {
			result = append(result, b)
		}
	}
	result = append(result, slack.NewContextBlock("toad_ticket_status",
		slack.NewTextBlockObject(slack.MarkdownType, ":hourglass: Ticket requested by "+userName+" — investigating…", false, false),
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

// ReplyWithOptionalCTA posts text as a thread reply, attaching a
// "Create Linear ticket" button (via TicketBlocks) when showCTA is true, or
// posting plain text otherwise. This is the single choke point for the
// "CTA-or-plain reply" pattern repeated across cmd/'s emission sites —
// callers pass showCTA as their existing condition (e.g. an outcome kind or
// category check) ANDed with ticketEngine.ShouldCreateIssues(), since the
// button must never appear when the tracker can't actually create issues
// (it would always error when clicked).
func (c *Client) ReplyWithOptionalCTA(channel, threadTS, text string, showCTA bool) (string, error) {
	if showCTA {
		blocks := TicketBlocks(text, threadTS)
		return c.ReplyInThreadWithBlocks(channel, threadTS, text, blocks)
	}
	return c.ReplyInThread(channel, threadTS, text)
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
