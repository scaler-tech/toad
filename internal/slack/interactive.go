package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

const actionIDTicket = "toad_ticket"

// parseTicketAction extracts the "toad_ticket" button click from an InteractionCallback.
// Returns (found, threadTS, channelID, userID).
func parseTicketAction(cb *slack.InteractionCallback) (bool, string, string, string) {
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID == actionIDTicket {
			return true, a.Value, cb.Channel.ID, cb.User.ID
		}
	}
	return false, "", "", ""
}

func handleInteractive(ctx context.Context, c *Client, evt socketmode.Event) {
	cb, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		return
	}

	if cb.Type != slack.InteractionTypeBlockActions {
		return
	}

	found, threadTS, channel, userID := parseTicketAction(&cb)
	if !found {
		return
	}

	slog.Info("ticket button clicked", "channel", channel, "user", userID, "thread", threadTS)

	c.SetStatus(channel, threadTS, ":ticket: Creating ticket...")

	// Instant feedback: replace button with processing indicator before any API calls.
	processingBlocks := TicketedByBlocks(cb.Message.Blocks, "")
	if err := respondToInteraction(cb.ResponseURL, cb.Message.Text, processingBlocks); err != nil {
		slog.Warn("failed to update button message", "error", err)
	}

	// The button's message text is the investigation finding — use it as the
	// primary text so the ticket captures exactly what to fix, rather than
	// re-triaging from the thread root (which may be a planning doc or unrelated).
	// However, if the button is on a toad system message (e.g. failure notification),
	// fall back to the thread root so toad retries the original request.
	buttonMessageText := cb.Message.Text
	isToadSystemMessage := strings.HasPrefix(buttonMessageText, ":x: ") ||
		strings.HasPrefix(buttonMessageText, ":warning: ") ||
		strings.HasPrefix(buttonMessageText, ":white_check_mark: ")

	go func() {
		userName := c.ResolveUserName(userID)

		// Truthful-in-progress flip: name the requester, but don't claim
		// success yet — the flow hasn't actually started until FetchMessage
		// below succeeds. The prior code jumped straight to the final
		// ":ticket: Ticket requested by <user>" wording here, before any
		// fetch had even been attempted, so a fetch failure below (which
		// used to just log and return) left the button falsely reporting
		// success with nothing actually happening (Critical fix).
		inProgressBlocks := TicketInProgressBlocks(cb.Message.Blocks, userName)
		if err := respondToInteraction(cb.ResponseURL, cb.Message.Text, inProgressBlocks); err != nil {
			slog.Warn("failed to update button message", "error", err)
		}

		msg, err := c.FetchMessage(channel, threadTS)
		if err != nil {
			slog.Warn("failed to fetch thread message for ticket button, retrying once", "error", err, "channel", channel, "thread", threadTS)
			time.Sleep(1 * time.Second)
			msg, err = c.FetchMessage(channel, threadTS)
		}
		if err != nil {
			slog.Error("failed to fetch thread message for ticket button after retry — ticket flow was not started",
				"error", err, "channel", channel, "thread", threadTS, "user", userID)

			if _, replyErr := c.ReplyInThread(channel, threadTS,
				":x: couldn't start the ticket flow (Slack fetch failed) — click again to retry"); replyErr != nil {
				slog.Warn("failed to post ticket-flow-failed reply", "error", replyErr)
			}
			// Restore the button to its original clickable state (rather
			// than leaving the in-progress/final wording stuck on the
			// message) so the user can simply click again to retry.
			if respErr := respondToInteraction(cb.ResponseURL, cb.Message.Text, cb.Message.Blocks.BlockSet); respErr != nil {
				slog.Warn("failed to restore button to retry state", "error", respErr)
			}
			return
		}

		// The fetch succeeded and the flow is about to actually start —
		// the final wording can now safely stick.
		finalBlocks := TicketedByBlocks(cb.Message.Blocks, userName)
		if err := respondToInteraction(cb.ResponseURL, cb.Message.Text, finalBlocks); err != nil {
			slog.Warn("failed to update button message", "error", err)
		}

		if !isToadSystemMessage {
			msg.Text = buttonMessageText
		}
		msg.IsTriggered = true
		msg.IsTicketRequest = true

		if c.handler != nil {
			c.handler(ctx, msg)
		}
	}()
}

// respondToInteraction POSTs a response payload to a Slack ResponseURL,
// replacing the original message with updated blocks.
func respondToInteraction(responseURL, fallbackText string, blocks []slack.Block) error {
	payload := struct {
		ReplaceOriginal bool          `json:"replace_original"`
		Text            string        `json:"text"`
		Blocks          []slack.Block `json:"blocks"`
	}{
		ReplaceOriginal: true,
		Text:            fallbackText,
		Blocks:          blocks,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal response payload: %w", err)
	}

	resp, err := http.Post(responseURL, "application/json", bytes.NewReader(body)) //nolint:gosec // URL is a trusted Slack ResponseURL from InteractionCallback
	if err != nil {
		return fmt.Errorf("post to response_url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("response_url returned status %d", resp.StatusCode)
	}
	return nil
}
