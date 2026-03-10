# Design: Slack Button CTA for Passive Ribbits

## Problem

Passive ribbit replies currently append a text CTA: `_React :frog: if you'd like me to fix this._` This is easy to miss and requires users to know the emoji reaction workflow. A Slack Block Kit button is more discoverable and standard.

## Changes

### 1. Block Kit message for passive ribbits

Replace the text CTA with a Block Kit message containing:
- A `section` block with the investigation text (mrkdwn)
- An `actions` block with a single green "Fix this" button

Button metadata:
- `action_id`: `"toad_fix"`
- `value`: thread timestamp (to identify which message to spawn a tadpole for)
- `style`: `"primary"` (green)

### 2. New Slack client methods

**`ReplyInThreadWithBlocks(channel, threadTS, text string, blocks ...slack.Block) (string, error)`**
- Posts a message with both `MsgOptionText` (fallback) and `MsgOptionBlocks`
- Tracks the reply like `ReplyInThread`

**`UpdateMessageWithBlocks(channel, timestamp, text string, blocks ...slack.Block) error`**
- Updates a message to replace blocks (used after button click)

### 3. Interactive event handler

Add `socketmode.EventTypeInteractive` case to `routeEvent()` in `client.go`:
- Acknowledge the event
- Handle `InteractionTypeBlockActions`
- Match `action_id: "toad_fix"`
- Extract thread TS from button value, channel from callback
- Build an `IncomingMessage` and call the message handler with `IsTadpoleRequest = true`
- Update the original message: replace button with "_Tadpole spawned by @user_"

### 4. `handlePassive` change

In `cmd/root.go`, switch from:
```go
slackClient.ReplyInThread(msg.Channel, msg.Timestamp,
    resp.Text+"\n\n_React :frog: if you'd like me to fix this._")
```

To building Block Kit blocks with the investigation text and a "Fix this" button, then calling `ReplyInThreadWithBlocks`.

### 5. Slack app requirement

The Slack app must have **Interactivity** enabled. Socket Mode handles the routing — no public URL needed. No new OAuth scopes required.

## What stays the same

- Emoji :frog: reaction works on any toad reply (unchanged)
- Triggered messages (explicit @toad mentions) unchanged
- Toad King auto-spawn flow unchanged
- Non-passive ribbits unchanged
