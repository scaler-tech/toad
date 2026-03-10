# Investigation Outreach Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make digest investigation findings visible to the right people — tag relevant devs in Slack for bot messages, crosspost to Linear tickets, nudge humans to share.

**Architecture:** Extend the digest notification callback with a richer `InvestigationNotice` struct so the outreach logic in `root.go` has access to bot ID, issue refs, and file hints. Add a `github_slack_mappings` DB table with `/toad github` slash commands for self-service mapping. Resolve GitHub logins to Slack users via DB lookup then fuzzy name match.

**Tech Stack:** Go, SQLite (state.db), Slack API (`GetUsers`, `<@U...>` mentions), Linear GraphQL (`PostComment`), existing `GetSuggestedReviewers` on VCS `Provider` interface.

**Spec:** `docs/plans/2026-03-10-investigation-outreach-design.md`

---

## Task 1: GitHub-Slack Mapping DB Table

**Files:**
- Modify: `internal/state/db.go` (add table + CRUD methods)
- Modify: `internal/state/db_test.go` (add tests)

- [ ] **Step 1: Write failing tests for CRUD operations**

In `internal/state/db_test.go`, add:

```go
func TestDB_GitHubSlackMapping_AddAndLookup(t *testing.T) {
	db := openTestDB(t)

	// Add a mapping
	if err := db.AddGitHubMapping("U123", "johndoe"); err != nil {
		t.Fatalf("AddGitHubMapping: %v", err)
	}

	// Lookup by GitHub login
	slackID, err := db.LookupSlackByGitHub("johndoe")
	if err != nil {
		t.Fatalf("LookupSlackByGitHub: %v", err)
	}
	if slackID != "U123" {
		t.Errorf("expected U123, got %q", slackID)
	}

	// Case-insensitive lookup
	slackID, _ = db.LookupSlackByGitHub("JohnDoe")
	if slackID != "U123" {
		t.Errorf("case-insensitive: expected U123, got %q", slackID)
	}

	// Unknown login returns empty
	slackID, _ = db.LookupSlackByGitHub("unknown")
	if slackID != "" {
		t.Errorf("expected empty, got %q", slackID)
	}
}

func TestDB_GitHubSlackMapping_MultiplePerUser(t *testing.T) {
	db := openTestDB(t)

	db.AddGitHubMapping("U123", "johndoe")
	db.AddGitHubMapping("U123", "john-work")

	logins, err := db.ListGitHubMappings("U123")
	if err != nil {
		t.Fatalf("ListGitHubMappings: %v", err)
	}
	if len(logins) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(logins))
	}
}

func TestDB_GitHubSlackMapping_UniqueGitHub(t *testing.T) {
	db := openTestDB(t)

	db.AddGitHubMapping("U123", "johndoe")
	err := db.AddGitHubMapping("U456", "johndoe")
	if err == nil {
		t.Fatal("expected error for duplicate github login")
	}
}

func TestDB_GitHubSlackMapping_Remove(t *testing.T) {
	db := openTestDB(t)

	db.AddGitHubMapping("U123", "johndoe")
	if err := db.RemoveGitHubMapping("U123", "johndoe"); err != nil {
		t.Fatalf("RemoveGitHubMapping: %v", err)
	}

	slackID, _ := db.LookupSlackByGitHub("johndoe")
	if slackID != "" {
		t.Errorf("expected empty after remove, got %q", slackID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/state/ -run TestDB_GitHubSlackMapping -v`
Expected: FAIL — methods don't exist yet.

- [ ] **Step 3: Add table creation and CRUD methods**

In `internal/state/db.go`, add table creation in `OpenDB()` alongside the other `CREATE TABLE` statements:

```go
_, err = db.Exec(`CREATE TABLE IF NOT EXISTS github_slack_mappings (
	slack_user_id TEXT NOT NULL,
	github_login  TEXT NOT NULL COLLATE NOCASE,
	created_at    DATETIME NOT NULL,
	UNIQUE(github_login)
)`)
```

Then add the CRUD methods after the existing digest opportunity methods:

```go
// AddGitHubMapping links a Slack user to a GitHub login.
func (d *DB) AddGitHubMapping(slackUserID, githubLogin string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO github_slack_mappings (slack_user_id, github_login, created_at)
		VALUES (?, ?, ?)`,
		slackUserID, strings.ToLower(githubLogin), time.Now(),
	)
	return err
}

// RemoveGitHubMapping unlinks a GitHub login from a Slack user.
func (d *DB) RemoveGitHubMapping(slackUserID, githubLogin string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		DELETE FROM github_slack_mappings
		WHERE slack_user_id = ? AND github_login = ?`,
		slackUserID, strings.ToLower(githubLogin),
	)
	return err
}

// LookupSlackByGitHub returns the Slack user ID for a GitHub login, or empty if not found.
func (d *DB) LookupSlackByGitHub(githubLogin string) (string, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	var slackID string
	err := d.db.QueryRowContext(ctx, `
		SELECT slack_user_id FROM github_slack_mappings
		WHERE github_login = ?`,
		strings.ToLower(githubLogin),
	).Scan(&slackID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return slackID, err
}

// ListGitHubMappings returns all GitHub logins linked to a Slack user.
func (d *DB) ListGitHubMappings(slackUserID string) ([]string, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx, `
		SELECT github_login FROM github_slack_mappings
		WHERE slack_user_id = ? ORDER BY created_at`,
		slackUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logins []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, err
		}
		logins = append(logins, login)
	}
	return logins, nil
}
```

Note: `db.go` already imports `database/sql`, `strings`, and `time`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/state/ -run TestDB_GitHubSlackMapping -v`
Expected: All PASS.

- [ ] **Step 5: Run full build and vet**

Run: `go build ./... && go vet ./...`

---

## Task 2: /toad github Slash Commands

**Files:**
- Modify: `internal/slack/mcp_commands.go` (add github command handlers)

- [ ] **Step 1: Add command routing in `handleSlashCommand`**

In `internal/slack/mcp_commands.go`, the `handleSlashCommand` function at line 35 has a `switch args[0]` block. Add a `case "github"` before the `default`:

```go
	case "github":
		if len(args) < 2 {
			c.mcpHandler.handleGitHubHelp(cmd)
			return
		}
		switch args[1] {
		case "add":
			if len(args) < 3 {
				c.mcpHandler.ephemeral(cmd, "Usage: `/toad github add <github-username>`")
				return
			}
			c.mcpHandler.handleGitHubAdd(cmd, args[2])
		case "list":
			c.mcpHandler.handleGitHubList(cmd)
		case "remove":
			if len(args) < 3 {
				c.mcpHandler.ephemeral(cmd, "Usage: `/toad github remove <github-username>`")
				return
			}
			c.mcpHandler.handleGitHubRemove(cmd, args[2])
		default:
			c.mcpHandler.handleGitHubHelp(cmd)
		}
```

- [ ] **Step 2: Add handler methods**

Add at the end of `internal/slack/mcp_commands.go`:

```go
// --- /toad github ---

func (h *SlashCommandHandler) handleGitHubAdd(cmd slack.SlashCommand, login string) {
	if err := h.db.AddGitHubMapping(cmd.UserID, login); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			h.ephemeral(cmd, fmt.Sprintf("GitHub account `%s` is already linked to another Slack user.", login))
			return
		}
		h.ephemeral(cmd, "Failed to add mapping: "+err.Error())
		return
	}
	h.ephemeral(cmd, fmt.Sprintf(":white_check_mark: Linked GitHub account `%s` to your Slack profile.", login))
}

func (h *SlashCommandHandler) handleGitHubList(cmd slack.SlashCommand) {
	logins, err := h.db.ListGitHubMappings(cmd.UserID)
	if err != nil {
		h.ephemeral(cmd, "Failed to list mappings: "+err.Error())
		return
	}
	if len(logins) == 0 {
		h.ephemeral(cmd, "No GitHub accounts linked. Use `/toad github add <username>` to link one.")
		return
	}
	lines := make([]string, len(logins))
	for i, l := range logins {
		lines[i] = fmt.Sprintf("• `%s`", l)
	}
	h.ephemeral(cmd, ":link: Your linked GitHub accounts:\n"+strings.Join(lines, "\n"))
}

func (h *SlashCommandHandler) handleGitHubRemove(cmd slack.SlashCommand, login string) {
	if err := h.db.RemoveGitHubMapping(cmd.UserID, login); err != nil {
		h.ephemeral(cmd, "Failed to remove mapping: "+err.Error())
		return
	}
	h.ephemeral(cmd, fmt.Sprintf(":white_check_mark: Unlinked GitHub account `%s`.", login))
}

func (h *SlashCommandHandler) handleGitHubHelp(cmd slack.SlashCommand) {
	h.ephemeral(cmd, "*GitHub account linking*\n\n"+
		"Link your GitHub account so toad can @mention you in investigation findings.\n\n"+
		"• `/toad github add <username>` — link a GitHub account\n"+
		"• `/toad github list` — show your linked accounts\n"+
		"• `/toad github remove <username>` — unlink an account")
}
```

- [ ] **Step 3: Verify build**

Run: `go build ./... && go vet ./...`

- [ ] **Step 4: Commit**

Stage `internal/state/db.go`, `internal/state/db_test.go`, `internal/slack/mcp_commands.go`.

---

## Task 3: GitHub-to-Slack User Resolver

**Files:**
- Create: `internal/slack/user_resolve.go` (resolution logic)
- Create: `internal/slack/user_resolve_test.go` (tests)

- [ ] **Step 1: Write failing tests**

Create `internal/slack/user_resolve_test.go`:

```go
package slack

import (
	"testing"
)

func TestFuzzyMatchSlackUser_ExactDisplayName(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "johndoe", RealName: "John Doe"},
		{ID: "U2", DisplayName: "janedoe", RealName: "Jane Doe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_CaseInsensitive(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "JohnDoe", RealName: "John Doe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_RealNameMatch(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "jd", RealName: "johndoe"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_NameComponentMatch(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "jd", RealName: "John Doe"},
	}
	// GitHub login "johndoe" matches "john"+"doe" from "John Doe"
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_NoMatch(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "alice", RealName: "Alice Smith"},
	}
	id := fuzzyMatchSlackUser("johndoe", users)
	if id != "" {
		t.Errorf("expected empty, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_MultipleMatches_ReturnsEmpty(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "john", RealName: "John Smith"},
		{ID: "U2", DisplayName: "john", RealName: "John Brown"},
	}
	id := fuzzyMatchSlackUser("john", users)
	if id != "" {
		t.Errorf("expected empty for ambiguous match, got %q", id)
	}
}

func TestFuzzyMatchSlackUser_FirstLastConcat(t *testing.T) {
	users := []slackUser{
		{ID: "U1", DisplayName: "hergen", RealName: "Hergen Dillema"},
	}
	// GitHub login "hergendillema" matches "hergen"+"dillema" concatenated
	id := fuzzyMatchSlackUser("hergendillema", users)
	if id != "U1" {
		t.Errorf("expected U1, got %q", id)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/slack/ -run TestFuzzyMatch -v`
Expected: FAIL — function doesn't exist.

- [ ] **Step 3: Implement resolver**

Create `internal/slack/user_resolve.go`:

```go
package slack

import (
	"log/slog"
	"strings"

	goslack "github.com/slack-go/slack"

	"github.com/scaler-tech/toad/internal/state"
)

// slackUser is a minimal representation for fuzzy matching.
type slackUser struct {
	ID          string
	DisplayName string
	RealName    string
}

// fuzzyMatchSlackUser tries to match a GitHub login to a Slack user.
// Returns the Slack user ID if exactly one match is found, empty string otherwise.
func fuzzyMatchSlackUser(githubLogin string, users []slackUser) string {
	login := strings.ToLower(githubLogin)
	var matches []string

	for _, u := range users {
		display := strings.ToLower(u.DisplayName)
		real := strings.ToLower(u.RealName)

		// Exact match on display name or real name
		if display == login || real == login {
			matches = append(matches, u.ID)
			continue
		}

		// Name component match: concatenate first+last parts and compare
		parts := strings.Fields(real)
		if len(parts) >= 2 {
			concat := strings.Join(parts, "")
			if concat == login {
				matches = append(matches, u.ID)
				continue
			}
			// Also check if login matches first or last name exactly
			for _, part := range parts {
				if part == login {
					matches = append(matches, u.ID)
					break
				}
			}
		}
	}

	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// ResolveGitHubToSlack resolves a list of GitHub logins to Slack user IDs.
// Uses DB mappings first, then fuzzy matching against workspace users.
// Returns a map of github_login -> slack_user_id for resolved users only.
func ResolveGitHubToSlack(db *state.DB, api *goslack.Client, logins []string) map[string]string {
	result := make(map[string]string)
	var unresolved []string

	// Phase 1: DB lookup
	for _, login := range logins {
		slackID, err := db.LookupSlackByGitHub(login)
		if err != nil {
			slog.Debug("github-slack lookup failed", "login", login, "error", err)
			continue
		}
		if slackID != "" {
			result[login] = slackID
		} else {
			unresolved = append(unresolved, login)
		}
	}

	if len(unresolved) == 0 {
		return result
	}

	// Phase 2: Fuzzy match against workspace users
	slackUsers, err := api.GetUsers()
	if err != nil {
		slog.Warn("failed to fetch Slack users for fuzzy match", "error", err)
		return result
	}

	users := make([]slackUser, 0, len(slackUsers))
	for _, u := range slackUsers {
		if u.Deleted || u.IsBot {
			continue
		}
		users = append(users, slackUser{
			ID:          u.ID,
			DisplayName: u.Profile.DisplayName,
			RealName:    u.RealName,
		})
	}

	for _, login := range unresolved {
		if slackID := fuzzyMatchSlackUser(login, users); slackID != "" {
			result[login] = slackID
		}
	}

	return result
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/slack/ -run TestFuzzyMatch -v`
Expected: All PASS.

- [ ] **Step 5: Verify build**

Run: `go build ./... && go vet ./...`

- [ ] **Step 6: Commit**

Stage `internal/slack/user_resolve.go`, `internal/slack/user_resolve_test.go`.

---

## Task 4: BotID in Digest Message + Config

**Files:**
- Modify: `internal/digest/digest.go` (add BotID to Message)
- Modify: `internal/config/config.go` (add BotList to DigestConfig)
- Modify: `cmd/root.go` (pass BotID in Collect, pass IsBot in IncomingMessage)

- [ ] **Step 1: Add BotID to digest Message struct**

In `internal/digest/digest.go` at line 23, add `BotID` field:

```go
type Message struct {
	Channel     string
	ChannelName string
	User        string
	Text        string
	ThreadTS    string
	Timestamp   string
	BotID       string
}
```

- [ ] **Step 2: Add BotList to DigestConfig**

In `internal/config/config.go` at line 103 (after `CommentInvestigation`), add:

```go
	BotList              []string `yaml:"bot_list"`                 // only these bot IDs trigger outreach (empty = all bots)
```

- [ ] **Step 3: Pass BotID through in root.go Collect call**

In `cmd/root.go`, the `IncomingMessage` struct doesn't have a BotID field, but it has `IsBot`. We need the actual bot_id. First check events.go — the `BotID` is available on the Slack event but not passed through to `IncomingMessage`. Add a `BotID` field to `IncomingMessage` in `internal/slack/client.go`:

```go
type IncomingMessage struct {
	Text             string
	Channel          string
	ChannelName      string
	User             string
	UserName         string
	Timestamp        string
	ThreadTimestamp  string
	IsMention        bool
	IsTriggered      bool
	IsBot            bool
	BotID            string  // add this field
	IsTadpoleRequest bool
	ThreadContext    []string
}
```

Then in `internal/slack/events.go`, populate it in `handleMessage` (around line 126 where the `IncomingMessage` is built) — add `BotID: ev.BotID`.

And in `handleAppMention` (around line 65) — add `BotID: ev.BotID`.

Then in `cmd/root.go` at the `Collect` call (line 487), add `BotID`:

```go
digestEngine.Collect(digest.Message{
	Channel:     msg.Channel,
	ChannelName: channelName,
	User:        msg.User,
	Text:        msg.Text,
	ThreadTS:    msg.ThreadTimestamp,
	Timestamp:   msg.Timestamp,
	BotID:       msg.BotID,
})
```

- [ ] **Step 4: Verify build and all tests pass**

Run: `go build ./... && go test ./... && go vet ./...`

- [ ] **Step 5: Commit**

Stage `internal/digest/digest.go`, `internal/config/config.go`, `internal/slack/client.go`, `internal/slack/events.go`, `cmd/root.go`.

---

## Task 5: InvestigationNotice and Callback Refactor

**Files:**
- Modify: `internal/digest/digest.go` (new type, replace callback signature, build notice)
- Modify: `cmd/root.go` (update callback wiring)

- [ ] **Step 1: Define InvestigationNotice and new callback type**

In `internal/digest/digest.go`, replace the `notifyInvestigation` type. After the existing `NotifyFunc` type (line 68):

```go
// InvestigationNotice holds all data needed for outreach after an investigation.
type InvestigationNotice struct {
	Channel   string
	ThreadTS  string
	Text      string                       // formatted findings
	BotID     string                       // original message's bot ID (empty for human)
	IssueRefs []*issuetracker.IssueRef
	FilesHint []string
	Repo      string
}

// NotifyInvestigationFunc handles posting investigation findings with outreach.
type NotifyInvestigationFunc func(notice InvestigationNotice)
```

This requires adding `issuetracker` to the imports of `digest.go`.

- [ ] **Step 2: Update Engine struct and constructor**

Change the `notifyInvestigation` field type from `NotifyFunc` to `NotifyInvestigationFunc`:

```go
	notifyInvestigation NotifyInvestigationFunc
```

Update the `New` constructor signature to match:

```go
func New(cfg *config.DigestConfig, agentProvider agent.Provider, triageModel string, spawn SpawnFunc, notify NotifyFunc, notifyInvestigation NotifyInvestigationFunc, investigate InvestigateFunc, ...
```

- [ ] **Step 3: Update processOpportunities to build InvestigationNotice**

In `processOpportunities`, the dry-run notification block (around line 534) currently does:

```go
if e.cfg.CommentInvestigation && e.notifyInvestigation != nil && reasoning != "" {
	comment := fmt.Sprintf(":mag: *Investigation findings:*\n\n%s", reasoning)
	e.notifyInvestigation(msg.Channel, threadTS, comment)
}
```

Replace with:

```go
if e.cfg.CommentInvestigation && e.notifyInvestigation != nil && reasoning != "" {
	e.notifyInvestigation(InvestigationNotice{
		Channel:   msg.Channel,
		ThreadTS:  threadTS,
		Text:      fmt.Sprintf(":mag: *Investigation findings:*\n\n%s", reasoning),
		BotID:     msg.BotID,
		IssueRefs: allRefs,
		FilesHint: opp.FilesHint,
		Repo:      opp.Repo,
	})
}
```

Note: `allRefs` is already available in scope (line 457 area, populated from `e.tracker.ExtractAllIssueRefs`).

- [ ] **Step 4: Update ResumeInvestigations similarly**

In `ResumeInvestigations` (around line 241), replace:

```go
if e.cfg.CommentInvestigation && e.notifyInvestigation != nil && reasoning != "" {
	comment := fmt.Sprintf(":mag: *Investigation findings:*\n\n%s", reasoning)
	e.notifyInvestigation(msg.Channel, msg.ThreadTS, comment)
}
```

With:

```go
if e.cfg.CommentInvestigation && e.notifyInvestigation != nil && reasoning != "" {
	e.notifyInvestigation(InvestigationNotice{
		Channel:   msg.Channel,
		ThreadTS:  msg.ThreadTS,
		Text:      fmt.Sprintf(":mag: *Investigation findings:*\n\n%s", reasoning),
		BotID:     "",  // not available in resume path (DB doesn't store bot_id)
		FilesHint: opp.FilesHint,
		Repo:      opp.Repo,
	})
}
```

- [ ] **Step 5: Update root.go callback wiring**

In `cmd/root.go`, the `notifyInvestigation` callback (around line 219-224) currently is:

```go
func(channel, threadTS, text string) {
	blocks := islack.FixThisBlocks(text, threadTS)
	if _, err := slackClient.ReplyInThreadWithBlocks(channel, threadTS, text, blocks); err != nil {
		slog.Warn("digest investigation reply failed", "error", err)
	}
},
```

Replace with a temporary pass-through that preserves existing behavior (outreach logic comes in Task 6):

```go
func(notice digest.InvestigationNotice) {
	blocks := islack.FixThisBlocks(notice.Text, notice.ThreadTS)
	if _, err := slackClient.ReplyInThreadWithBlocks(notice.Channel, notice.ThreadTS, notice.Text, blocks); err != nil {
		slog.Warn("digest investigation reply failed", "error", err)
	}
},
```

- [ ] **Step 6: Verify build and all tests pass**

Run: `go build ./... && go test ./... && go vet ./...`

- [ ] **Step 7: Commit**

Stage `internal/digest/digest.go`, `cmd/root.go`.

---

## Task 6: Outreach Logic in Callback

**Files:**
- Modify: `cmd/root.go` (full outreach logic in the notifyInvestigation callback)

This is the main integration task. The callback now receives the full `InvestigationNotice` and does:

1. Determine if this is a bot message needing active outreach
2. If bot: resolve file contributors → GitHub logins → Slack user IDs → build cc line
3. If human: append nudge text
4. Post the investigation reply with blocks
5. If bot + issue refs: crosspost to Linear

- [ ] **Step 1: Implement the full outreach callback**

Replace the notifyInvestigation callback in `cmd/root.go` with:

```go
func(notice digest.InvestigationNotice) {
	text := notice.Text

	// Determine if this is a bot message needing active outreach
	isBot := notice.BotID != ""
	if isBot && len(cfg.Digest.BotList) > 0 {
		isBot = false
		for _, b := range cfg.Digest.BotList {
			if b == notice.BotID {
				isBot = true
				break
			}
		}
	}

	if isBot {
		// Resolve file contributors to Slack mentions
		if len(notice.FilesHint) > 0 {
			repo := resolver.Resolve(notice.Repo, notice.FilesHint)
			if repo != nil {
				vcsProvider := vcsResolver(repo.Path)
				botSet := make(map[string]bool)
				for _, u := range cfg.VCS.BotUsernames {
					botSet[strings.ToLower(u)] = true
				}
				if logins := vcsProvider.GetSuggestedReviewers(
					context.Background(), repo.Path, notice.FilesHint, botSet, 2,
				); len(logins) > 0 {
					resolved := islack.ResolveGitHubToSlack(stateDB, slackClient.API(), logins)
					var mentions []string
					for _, login := range logins {
						if slackID, ok := resolved[login]; ok {
							mentions = append(mentions, fmt.Sprintf("<@%s>", slackID))
						}
					}
					if len(mentions) > 0 {
						text += "\n\ncc " + strings.Join(mentions, " ")
					}
				}
			}
		}
	} else {
		text += "\n\n_Tag a relevant dev if you'd like someone to take a look._"
	}

	// Post investigation reply with CTA button
	blocks := islack.FixThisBlocks(text, notice.ThreadTS)
	replyTS := ""
	if ts, err := slackClient.ReplyInThreadWithBlocks(
		notice.Channel, notice.ThreadTS, text, blocks,
	); err != nil {
		slog.Warn("digest investigation reply failed", "error", err)
	} else {
		replyTS = ts
	}

	// Crosspost to Linear if bot message with issue refs
	if isBot && tracker != nil && len(notice.IssueRefs) > 0 && replyTS != "" {
		permalink, _ := slackClient.GetPermalink(notice.Channel, replyTS)
		for _, ref := range notice.IssueRefs {
			body := notice.Text + "\n\n"
			if permalink != "" {
				body += fmt.Sprintf("Toad can fix this automatically — [go to the Slack thread](%s) and click the button to start.", permalink)
			} else {
				body += "Toad can fix this automatically — go to the Slack thread and click the button to start."
			}
			if err := tracker.PostComment(context.Background(), ref, body); err != nil {
				slog.Warn("failed to crosspost investigation to issue tracker",
					"ref", ref.ID, "error", err)
			} else {
				slog.Info("crossposted investigation to issue tracker", "ref", ref.ID)
			}
		}
	}
},
```

Note: this callback closure captures `cfg`, `resolver`, `vcsResolver`, `stateDB`, `slackClient`, `tracker` — all of which are already in scope where the digest engine is created. Check that `vcsResolver` (the `vcs.Resolver` function) is accessible. It's built around line 155 in root.go. Also need `context` and `strings` imports (already present in root.go).

The `vcsResolver` variable name — check the actual name used in root.go. It's likely just the resolver returned from `vcs.NewResolver(...)`. Find the actual variable name and use it.

- [ ] **Step 2: Verify build and all tests pass**

Run: `go build ./... && go test ./... && go vet ./...`

- [ ] **Step 3: Test manually**

Restart toad. Verify:
- Bot messages in digest get investigation replies with @mentions
- Human messages get the nudge text
- Linear crossposting works when issue refs are present
- The CTA button still works after the text changes

- [ ] **Step 4: Commit**

Stage `cmd/root.go`.

---

## Task 7: Final Cleanup and Formatting

- [ ] **Step 1: Run gofmt**

Run: `gofmt -l .`
Fix any files: `gofmt -w <file>`

- [ ] **Step 2: Run golangci-lint**

Run: `golangci-lint run ./...`
Fix any issues.

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`

- [ ] **Step 4: Final commit if needed**

Stage any remaining fixes.
