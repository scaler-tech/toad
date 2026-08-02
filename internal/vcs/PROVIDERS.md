# VCS Providers

Toad v2 dropped tadpole coding/shipping in favor of an investigation agent that
files Linear tickets. VCS involvement shrank accordingly: the `Provider`
interface is now a thin, read-only abstraction used to sanity-check that the
platform CLI is available and to suggest reviewers for investigation output.
It no longer manages PR/MR lifecycle (creation, merging, CI status, review
comments, bot-PR cleanup) — that logic belonged to the tadpole/reviewer
subsystems, which no longer exist.

## Current Providers

| Provider | Platform key | CLI tool | Status |
|----------|-------------|----------|--------|
| GitHub | `github` | `gh` | Implemented |
| GitLab | `gitlab` | `glab` | Implemented (including self-hosted); `GetSuggestedReviewers` is a stub returning `nil` |

## Possible Additions

| Provider | Platform key | CLI tool | Notes |
|----------|-------------|----------|-------|
| Bitbucket | `bitbucket` | `bb` or REST API | Atlassian's platform, common in enterprise |
| Gitea / Forgejo | `gitea` | `tea` | Self-hosted Git, GitHub-compatible API subset |
| Azure DevOps | `azuredevops` | `az repos` | Microsoft's platform |

## The Provider Interface

```go
type Provider interface {
    Check() error
    GetSuggestedReviewers(ctx context.Context, repoPath string, files []string, botNames map[string]bool, max int) []string
}
```

**`Check`** verifies the platform CLI tool is installed and in `PATH`. Called once per unique provider config at `Resolver` construction time (preflight-style fail-fast).

**`GetSuggestedReviewers`** looks at recent git history for a set of files and returns up to `max` login handles of likely owners, excluding bots. It's used to enrich investigation output/Linear tickets with "who should look at this" signal — no PR is created or required.

## Architecture: Resolver Pattern

VCS uses a `Resolver` because each repo can have a different provider:

```go
type Resolver func(repoPath string) Provider
```

`NewResolver` pre-builds providers from per-repo configs, deduplicates identical configs to share instances, and `Check()`-s each unique provider once at startup.

Config example with per-repo overrides:
```yaml
vcs:
  platform: "github"          # global default

repos:
  - name: "main-app"
    path: "/code/main"        # uses global github

  - name: "internal-tool"
    path: "/code/internal"
    vcs:
      platform: "gitlab"      # override for this repo
      host: "gitlab.company.com"
```

## Adding a New Provider

### 1. Create the provider file

Create `internal/vcs/<name>.go` implementing both `Provider` methods. Use `github.go` or `gitlab.go` as reference.

```go
type BitbucketProvider struct {
    Host string
}

func (b *BitbucketProvider) Check() error {
    // Verify CLI tool is installed
}

func (b *BitbucketProvider) GetSuggestedReviewers(ctx context.Context, repoPath string, files []string, botNames map[string]bool, max int) []string {
    // Walk recent commit history per file and rank contributors, or
    // return nil if not implemented yet (see GitLabProvider's stub).
}
```

### 2. Register in the factory

In `provider.go`, add a case to `NewProvider`:

```go
case "bitbucket":
    return &BitbucketProvider{Host: cfg.Host}, nil
```

### 3. Add to config validation

In `internal/config/config.go`, add to both `validPlatforms` maps in `Validate()` and `ValidateRepos()`:

```go
validPlatforms := map[string]bool{"github": true, "gitlab": true, "bitbucket": true}
```

### 4. Write tests

`resolver_test.go` covers `Resolver`/`NewResolver`/`configKey` and is provider-agnostic (skips if the relevant CLI isn't installed). Add provider-specific tests alongside your new file if `GetSuggestedReviewers` has real logic worth covering (see `github.go`'s scoring-by-file-specificity approach).

## Implementation Notes

- Both existing providers shell out to their respective CLI tools (`gh`, `glab`) rather than using REST APIs directly. This keeps auth simple — users configure the CLI tool once, and Toad inherits the credentials.
- `repoPath` is passed to `GetSuggestedReviewers` so the CLI/git commands run in the correct repo context (`cmd.Dir`).
- `botNames` lets callers exclude known bot accounts (e.g. Renovate, Dependabot) from suggested-reviewer results.
