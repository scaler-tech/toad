# Button CTA for Passive Ribbits — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the text CTA on passive ribbit replies with a Slack Block Kit "Fix this" button that spawns a tadpole on click.

**Architecture:** Add two new methods to the Slack client (`ReplyInThreadWithBlocks`, `UpdateMessageWithBlocks`), handle `socketmode.EventTypeInteractive` in event routing, and update `handlePassive` in `cmd/root.go` to use blocks instead of text CTA.

**Tech Stack:** `github.com/slack-go/slack` v0.18.0 Block Kit API, Socket Mode interactive events.

---

### Task 1: Add `ReplyInThreadWithBlocks` to Slack client

**Files:**
- Modify: `internal/slack/responder.go`
- Test: `internal/slack/client_test.go`

**Step 1: Write the test**

Add to `internal/slack/client_test.go`:

```go
func TestFixThisBlocks(t *testing.T) {
	text := "Found a bug in utils/time.go"
	threadTS := "1234567890.123456"
	blocks := FixThisBlocks(text, threadTS)

	// Should have 2 blocks: section (text) + actions (button)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	// First block is section with the investigation text
	section, ok := blocks[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("expected SectionBlock, got %T", blocks[0])
	}
	if section.Text == nil || section.Text.Text != text {
		t.Errorf("expected text %q, got %q", text, section.Text.Text)
	}

	// Second block is actions with a button
	actions, ok := blocks[1].(*slack.ActionBlock)
	if !ok {
		t.Fatalf("expected ActionBlock, got %T", blocks[1])
	}
	if len(actions.Elements.ElementSet) != 1 {
		t.Fatalf("expected 1 element, got %d", len(actions.Elements.ElementSet))
	}
	btn, ok := actions.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if !ok {
		t.Fatalf("expected ButtonBlockElement, got %T", actions.Elements.ElementSet[0])
	}
	if btn.ActionID != "toad_fix" {
		t.Errorf("expected action_id 'toad_fix', got %q", btn.ActionID)
	}
	if btn.Value != threadTS {
		t.Errorf("expected value %q, got %q", threadTS, btn.Value)
	}
	if btn.Style != slack.StylePrimary {
		t.Errorf("expected primary style, got %q", btn.Style)
	}
}

func TestSpawnedByBlocks(t *testing.T) {
	text := "Found a bug in utils/time.go"
	userName := "jamie"
	blocks := SpawnedByBlocks(text, userName)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}

	// First block is the investigation text
	section, ok := blocks[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("expected SectionBlock, got %T", blocks[0])
	}
	if section.Text.Text != text {
		t.Errorf("expected text %q, got %q", text, section.Text.Text)
	}

	// Second block is context with "spawned by" text
	ctx, ok := blocks[1].(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected ContextBlock, got %T", blocks[1])
	}
	if len(ctx.ContextElements.Elements) != 1 {
		t.Fatalf("expected 1 context element, got %d", len(ctx.ContextElements.Elements))
	}
	ctxText, ok := ctx.ContextElements.Elements[0].(*slack.TextBlockObject)
	if !ok {
		t.Fatalf("expected TextBlockObject, got %T", ctx.ContextElements.Elements[0])
	}
	if ctxText.Text != ":hatching_chick: Tadpole spawned by jamie" {
		t.Errorf("unexpected context text: %q", ctxText.Text)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/slack/ -run "TestFixThisBlocks|TestSpawnedByBlocks" -v`
Expected: FAIL — `FixThisBlocks` and `SpawnedByBlocks` undefined.

**Step 3: Implement block builders and new methods**

Add to `internal/slack/responder.go`:

```go
// FixThisBlocks builds Block Kit blocks for a passive ribbit with a "Fix this" button.
// The button's value carries the thread timestamp so the handler knows which thread to act on.
func FixThisBlocks(text, threadTS string) []slack.Block {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
		nil, nil,
	)
	btn := slack.NewButtonBlockElement("toad_fix", threadTS,
		slack.NewTextBlockObject(slack.PlainTextType, "Fix this", false, false),
	)
	btn.WithStyle(slack.StylePrimary)
	actions := slack.NewActionBlock("toad_fix_actions", btn)
	return []slack.Block{section, actions}
}

// SpawnedByBlocks builds Block Kit blocks that replace the button after a tadpole is spawned.
func SpawnedByBlocks(text, userName string) []slack.Block {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
		nil, nil,
	)
	ctx := slack.NewContextBlock("toad_fix_status",
		slack.NewTextBlockObject(slack.MarkdownType,
			":hatching_chick: Tadpole spawned by "+userName, false, false),
	)
	return []slack.Block{section, ctx}
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
```

**Step 4: Run tests to verify they pass**

Run: `go test ./internal/slack/ -run "TestFixThisBlocks|TestSpawnedByBlocks" -v`
Expected: PASS

**Step 5: Run full test suite**

Run: `go test ./internal/slack/ -v`
Expected: All tests PASS.

**Step 6: Commit**

```
git add internal/slack/responder.go internal/slack/client_test.go
git commit -m "Add Block Kit builders and methods for button CTA"
```

---

### Task 2: Handle interactive events (button clicks)

**Files:**
- Modify: `internal/slack/client.go:491-510` (routeEvent)
- Create: `internal/slack/interactive.go`
- Test: `internal/slack/client_test.go`

**Step 1: Write the test**

Add to `internal/slack/client_test.go`:

```go
func TestParseInteraction_FixButton(t *testing.T) {
	cb := &slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		Channel: slack.Channel{
			GroupConversation: slack.GroupConversation{
				Conversation: slack.Conversation{ID: "C123"},
			},
		},
		User:      slack.User{ID: "U456", Name: "jamie"},
		MessageTs: "111.222",
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{
					ActionID: "toad_fix",
					Value:    "999.888",
					BlockID:  "toad_fix_actions",
				},
			},
		},
	}

	action, threadTS, channel, userID := parseFixAction(cb)
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

func TestParseInteraction_WrongAction(t *testing.T) {
	cb := &slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: "something_else", Value: "999.888"},
			},
		},
	}

	action, _, _, _ := parseFixAction(cb)
	if action {
		t.Fatal("expected action=false for non-toad action")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/slack/ -run "TestParseInteraction" -v`
Expected: FAIL — `parseFixAction` undefined.

**Step 3: Create `internal/slack/interactive.go`**

```go
package slack

import (
	"context"
	"log/slog"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

const actionIDFix = "toad_fix"

// parseFixAction extracts the "toad_fix" button click from an InteractionCallback.
// Returns (found, threadTS, channelID, userID).
func parseFixAction(cb *slack.InteractionCallback) (bool, string, string, string) {
	for _, a := range cb.ActionCallback.BlockActions {
		if a.ActionID == actionIDFix {
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

	found, threadTS, channel, userID := parseFixAction(&cb)
	if !found {
		return
	}

	slog.Info("fix button clicked", "channel", channel, "user", userID, "thread", threadTS)

	// Update the message to replace the button with "Tadpole spawned by @user"
	userName := c.ResolveUserName(userID)
	// Fetch the original message to get the investigation text from the section block
	origText := ""
	if cb.Message.Text != "" {
		origText = cb.Message.Text
	}
	blocks := SpawnedByBlocks(origText, userName)
	if err := c.UpdateMessageWithBlocks(channel, cb.MessageTs, origText, blocks); err != nil {
		slog.Warn("failed to update button message", "error", err)
	}

	// Build an IncomingMessage for the original thread and dispatch as tadpole request
	msg, err := c.FetchMessage(channel, threadTS)
	if err != nil {
		slog.Error("failed to fetch thread message for fix button", "error", err)
		return
	}
	msg.IsTriggered = true
	msg.IsTadpoleRequest = true

	if c.handler != nil {
		c.handler(ctx, msg)
	}
}
```

**Step 4: Wire into `routeEvent`**

In `internal/slack/client.go`, add the interactive case to `routeEvent` (after the `EventTypeSlashCommand` case, around line 502):

```go
	case socketmode.EventTypeInteractive:
		c.socket.Ack(*evt.Request)
		handleInteractive(ctx, c, evt)
```

**Step 5: Run tests to verify they pass**

Run: `go test ./internal/slack/ -run "TestParseInteraction" -v`
Expected: PASS

**Step 6: Build check**

Run: `go build ./...`
Expected: Clean build.

**Step 7: Commit**

```
git add internal/slack/interactive.go internal/slack/client.go internal/slack/client_test.go
git commit -m "Handle interactive events for fix button clicks"
```

---

### Task 3: Update `handlePassive` to use Block Kit

**Files:**
- Modify: `cmd/root.go:839-841`

**Step 1: Update `handlePassive`**

In `cmd/root.go`, replace lines 839-841:

```go
	daemonCounters.ribbits.Add(1)
	slackClient.ReplyInThread(msg.Channel, msg.Timestamp,
		resp.Text+"\n\n_React :frog: if you'd like me to fix this._")
```

With:

```go
	daemonCounters.ribbits.Add(1)
	blocks := islack.FixThisBlocks(resp.Text, msg.ThreadTS())
	slackClient.ReplyInThreadWithBlocks(msg.Channel, msg.Timestamp,
		resp.Text, blocks)
```

Note: The fallback text (second arg) is the plain investigation text — it's what shows in notifications and non-Block Kit clients.

**Step 2: Verify the import**

`cmd/root.go` already imports `islack "github.com/scaler-tech/toad/internal/slack"`, so `islack.FixThisBlocks` is available.

**Step 3: Build check**

Run: `go build ./...`
Expected: Clean build.

**Step 4: Run all tests**

Run: `go test ./...`
Expected: All PASS.

**Step 5: Commit**

```
git add cmd/root.go
git commit -m "Use Block Kit button CTA for passive ribbit replies"
```

---

### Task 4: Formatting check and final verification

**Step 1: Format check**

Run: `gofmt -l .`
Expected: No output (all files formatted).

If any files listed, run: `gofmt -w <file>`

**Step 2: Vet**

Run: `go vet ./...`
Expected: Clean.

**Step 3: Full test suite**

Run: `go test ./...`
Expected: All PASS.

**Step 4: Lint**

Run: `golangci-lint run ./...`
Expected: No issues.

---

### Task 5: Update SETUP.md

**Files:**
- Modify: `SETUP.md`

**Step 1: Add interactivity note to Slack App Setup**

In `SETUP.md`, after Step 4b (slash command), add a new step:

```markdown
### Step 4c: Enable Interactivity (optional, for button CTAs)

If you want toad's passive investigation messages to include a clickable "Fix this" button:

1. Go to **Interactivity & Shortcuts** in the left sidebar
2. Toggle **Interactivity** on
3. For the **Request URL**, leave it empty — Socket Mode handles routing automatically

No additional scopes are needed. When interactivity is enabled, passive ribbit replies show a green "Fix this" button instead of a text prompt. Clicking the button spawns a tadpole immediately.
```

**Step 2: Commit**

```
git add SETUP.md
git commit -m "Document interactivity setup for button CTA"
```
