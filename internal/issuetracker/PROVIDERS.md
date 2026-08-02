# Issue Tracker Providers

Toad uses a `Tracker` interface to link opportunities to external issue trackers. In toad v2 this is the primary output of the investigation flow — investigations file Linear tickets rather than coding a fix — but the interface itself stays lightweight: extract references from messages, optionally create new issues.

## Current Providers

| Provider | Config key | Status |
|----------|-----------|--------|
| Linear | `linear` | Implemented |
| Noop | (disabled) | Built-in fallback |

## Possible Additions

| Provider | Config key | Notes |
|----------|-----------|-------|
| Jira | `jira` | REST API, issue key format `PROJ-123` |
| GitHub Issues | `github` | Already have `gh` CLI from VCS provider |
| GitLab Issues | `gitlab` | Already have `glab` CLI from VCS provider |
| Shortcut | `shortcut` | REST API, `sc-12345` references |

## The Tracker Interface

```go
type Tracker interface {
    ExtractIssueRef(text string) *IssueRef
    CreateIssue(ctx context.Context, opts CreateIssueOpts) (*IssueRef, error)
    ShouldCreateIssues() bool
}
```

**`ExtractIssueRef`** scans message text for issue references (URLs or bare IDs like `PLF-3125`). Returns the first match or nil. Used by the digest engine to link opportunities to existing issues.

**`CreateIssue`** creates a new issue with title, description, and category (`"bug"` or `"feature"`). Called when `create_issues: true` is configured and no existing reference was found.

**`ShouldCreateIssues`** returns whether the tracker is configured for auto-creation (checks `create_issues` config + required fields like API token and team ID).

The `NoopTracker` returns nil/false for everything — used when issue tracking is disabled.

## Adding a New Provider

### 1. Create the provider file

Create `internal/issuetracker/<name>.go`:

```go
type JiraTracker struct {
    host     string
    token    string
    project  string
    createOk bool
}

func (j *JiraTracker) ExtractIssueRef(text string) *IssueRef {
    // Match Jira URLs: https://company.atlassian.net/browse/PROJ-123
    // Match bare keys: PROJ-123
    // Return &IssueRef{Provider: "jira", ID: "PROJ-123", URL: "..."}
}

func (j *JiraTracker) CreateIssue(ctx context.Context, opts CreateIssueOpts) (*IssueRef, error) {
    // POST to Jira REST API
}

func (j *JiraTracker) ShouldCreateIssues() bool {
    return j.createOk
}
```

### 2. Register in the factory

In `tracker.go`, add a case to `NewTracker`:

```go
case "jira":
    return NewJiraTracker(cfg)
```

### 3. Add config fields

In `internal/config/config.go`, the existing `IssueTrackerConfig` struct is provider-agnostic:

```go
type IssueTrackerConfig struct {
    Enabled        bool   `yaml:"enabled"`
    Provider       string `yaml:"provider"`
    APIToken       string `yaml:"api_token"`
    TeamID         string `yaml:"team_id"`
    CreateIssues   bool   `yaml:"create_issues"`
    BugLabelID     string `yaml:"bug_label_id"`
    FeatureLabelID string `yaml:"feature_label_id"`
}
```

If new providers need additional fields (e.g., Jira host, project key), add them here. Fields unused by a given provider are simply ignored.

### 4. Write tests

See `linear_test.go` for the pattern. Key test areas:
- Issue reference extraction (URLs, bare IDs, edge cases, false positives)
- Issue creation (API mocking)
- `ShouldCreateIssues` logic

## Implementation Notes

- The `IssueRef.BranchPrefix()` method lowercases the issue ID for branch naming (e.g., `PLF-3125` → `plf-3125`). This is a holdover from the v1 tadpole workflow (branches were named after the linked issue); v2's investigation flow doesn't create branches, but the method is cheap to keep for any future coding-agent workflow that wants issue-derived branch names.
- Linear's `ExtractIssueRef` filters common false positives (HTTP, JSON, UTF, etc.) to avoid matching uppercase acronyms as issue IDs. New providers with similar ID formats should do the same.
- The tracker is optional — Toad works fine without it. When disabled, `NewTracker` returns `NoopTracker`.
