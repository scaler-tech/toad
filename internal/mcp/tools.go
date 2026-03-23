package mcp

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/scaler-tech/toad/internal/config"
	"github.com/scaler-tech/toad/internal/ribbit"
	islack "github.com/scaler-tech/toad/internal/slack"
	"github.com/scaler-tech/toad/internal/state"
	"github.com/scaler-tech/toad/internal/triage"
)

type logsArgs struct {
	Lines  int    `json:"lines" jsonschema:"Number of recent log lines to return (default 50)"`
	Level  string `json:"level,omitempty" jsonschema:"Filter by log level (DEBUG INFO WARN ERROR)"`
	Search string `json:"search,omitempty" jsonschema:"Substring or regex filter (regex if valid pattern, e.g. 'invalid.*<')"`
	Since  string `json:"since,omitempty" jsonschema:"Time filter: duration like 1h or absolute RFC3339"`
}

// RegisterLogsTool registers the logs tool on the given MCP server.
func RegisterLogsTool(srv *gomcp.Server, logFile string) {
	gomcp.AddTool(srv, &gomcp.Tool{
		Name:        "logs",
		Description: "Read and filter toad daemon log lines. Dev-only access.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, args logsArgs) (*gomcp.CallToolResult, any, error) {
		tok := tokenFromContext(ctx)
		if tok == nil || tok.Role != "dev" {
			return nil, nil, fmt.Errorf("access denied: dev role required")
		}

		maxLines := args.Lines
		if maxLines <= 0 {
			maxLines = 50
		}

		result, err := readLogs(logFile, maxLines, args.Level, args.Search, args.Since)
		if err != nil {
			return nil, nil, err
		}

		return &gomcp.CallToolResult{
			Content: []gomcp.Content{&gomcp.TextContent{Text: result}},
		}, nil, nil
	})
}

// maxScanLines is the maximum number of lines read from the log file.
// This caps memory usage for long-running daemons with large log files.
const maxScanLines = 10000

// readLogs reads the log file, keeps the last maxScanLines lines to bound
// memory, filters them, and returns up to maxLines matches in chronological order.
func readLogs(logFile string, maxLines int, level, search, since string) (string, error) {
	f, err := os.Open(logFile)
	if err != nil {
		return "", fmt.Errorf("reading log file: %w", err)
	}
	defer f.Close()

	// Read all lines, then keep only the tail to bound memory.
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) > maxScanLines {
		lines = lines[len(lines)-maxScanLines:]
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scanning log file: %w", err)
	}

	var sinceTime time.Time
	if since != "" {
		sinceTime, err = parseSince(since)
		if err != nil {
			return "", fmt.Errorf("invalid since value: %w", err)
		}
	}

	levelUpper := strings.ToUpper(level)

	// Try compiling search as regex; fall back to case-insensitive substring.
	var searchRe *regexp.Regexp
	var searchLower string
	if search != "" {
		if re, err := regexp.Compile("(?i)" + search); err == nil {
			searchRe = re
		} else {
			searchLower = strings.ToLower(search)
		}
	}

	// Filter from end for tail behavior, collect matches.
	var matched []string
	for i := len(lines) - 1; i >= 0 && len(matched) < maxLines; i-- {
		line := lines[i]
		if line == "" {
			continue
		}

		if levelUpper != "" && !strings.Contains(line, "level="+levelUpper) {
			continue
		}

		if searchRe != nil && !searchRe.MatchString(line) {
			continue
		} else if searchLower != "" && !strings.Contains(strings.ToLower(line), searchLower) {
			continue
		}

		if !sinceTime.IsZero() {
			if t, ok := parseLogTime(line); ok && t.Before(sinceTime) {
				continue
			}
		}

		matched = append(matched, line)
	}

	if len(matched) == 0 {
		return "No matching log lines found.", nil
	}

	// Reverse to chronological order.
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}

	return strings.Join(matched, "\n"), nil
}

// parseSince parses a duration string (e.g. "1h", "30m") relative to now,
// or an absolute RFC3339 timestamp.
func parseSince(s string) (time.Time, error) {
	// Try as duration first.
	d, err := time.ParseDuration(s)
	if err == nil {
		return time.Now().Add(-d), nil
	}

	// Try common time formats.
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("expected duration (e.g. 1h) or timestamp (RFC3339, 2006-01-02), got %q", s)
}

// parseLogTime extracts the time from a slog TextHandler log line.
// Expected format: time=2026-03-09T10:00:00Z ...
func parseLogTime(line string) (time.Time, bool) {
	if !strings.HasPrefix(line, "time=") {
		return time.Time{}, false
	}
	end := strings.IndexByte(line[5:], ' ')
	if end < 0 {
		end = len(line) - 5
	}
	t, err := time.Parse(time.RFC3339, line[5:5+end])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// RibbitResponder abstracts the ribbit engine for testability.
type RibbitResponder interface {
	Respond(ctx context.Context, messageText string, tr *triage.Result, prior *ribbit.PriorContext, repoPath string, defaultBranch string, repoPaths map[string]string) (*ribbit.Response, error)
}

// TriageClassifier abstracts the triage engine for testability.
type TriageClassifier interface {
	Classify(ctx context.Context, msg *islack.IncomingMessage, channelName string) (*triage.Result, error)
}

// RepoResolver abstracts the config resolver for testability.
type RepoResolver interface {
	Resolve(triageRepo string, fileHints []string) *config.RepoConfig
}

// AskDeps holds dependencies for the ask tool.
type AskDeps struct {
	Ribbit   RibbitResponder
	Triage   TriageClassifier
	Resolver RepoResolver
	Repos    []config.RepoConfig
	Sessions *SessionStore
	Sem      chan struct{} // ribbit semaphore
}

type askArgs struct {
	Question     string `json:"question" jsonschema:"The question to ask about the codebase"`
	Repo         string `json:"repo,omitempty" jsonschema:"Optional repo name (auto-detected if omitted)"`
	ClearContext bool   `json:"clear_context,omitempty" jsonschema:"Reset conversation context"`
}

// RegisterAskTool registers the ask tool on the given MCP server.
func RegisterAskTool(srv *gomcp.Server, deps *AskDeps) {
	gomcp.AddTool(srv, &gomcp.Tool{
		Name:        "ask",
		Description: "Ask toad a question about the codebase. Toad searches the code and answers using its project knowledge.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, args askArgs) (*gomcp.CallToolResult, any, error) {
		tok := tokenFromContext(ctx)
		if tok == nil {
			return nil, nil, fmt.Errorf("authentication required")
		}

		if strings.TrimSpace(args.Question) == "" {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Please provide a question."}},
				IsError: true,
			}, nil, nil
		}

		sessionID := tok.SlackUserID
		if args.ClearContext {
			deps.Sessions.Clear(sessionID)
		}

		slog.Info("MCP ask", "user", tok.SlackUser, "question", args.Question)

		// Acquire ribbit semaphore.
		select {
		case deps.Sem <- struct{}{}:
			defer func() { <-deps.Sem }()
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}

		// Build synthetic IncomingMessage for triage.
		msg := &islack.IncomingMessage{
			Text:      args.Question,
			IsMention: true,
		}

		// Triage the question.
		tr, err := deps.Triage.Classify(ctx, msg, "mcp")
		if err != nil {
			slog.Error("MCP triage failed", "error", err)
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Sorry, I couldn't process that question."}},
				IsError: true,
			}, nil, nil
		}

		// Only allow questions through MCP.
		if tr.Category != "question" && tr.Category != "" {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{
					Text: "I can only answer questions via MCP. For bugs and feature requests, please use Slack!",
				}},
			}, nil, nil
		}

		// Resolve repo.
		repoHint := args.Repo
		if repoHint == "" {
			repoHint = tr.Repo
		}
		repo := deps.Resolver.Resolve(repoHint, tr.FilesHint)
		if repo == nil {
			repo = config.PrimaryRepo(deps.Repos)
		}
		if repo == nil {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "No repo configured. Please specify a repo or set a primary repo in config."}},
				IsError: true,
			}, nil, nil
		}

		repoPath := repo.Path
		repoPaths := make(map[string]string)
		for _, r := range deps.Repos {
			repoPaths[r.Path] = r.Name
		}

		// Build prior context from session.
		var prior *ribbit.PriorContext
		if sc := deps.Sessions.GetContext(sessionID); sc != nil {
			var summary []string
			for _, ex := range sc.Exchanges {
				summary = append(summary, "Q: "+ex.Question, "A: "+ex.Answer)
			}
			last := sc.Exchanges[len(sc.Exchanges)-1]
			prior = &ribbit.PriorContext{
				Summary:  strings.Join(summary, "\n"),
				Response: last.Answer,
			}
		}

		// Run ribbit.
		resp, err := deps.Ribbit.Respond(ctx, args.Question, tr, prior, repoPath, repo.DefaultBranch, repoPaths)
		if err != nil {
			slog.Error("MCP ribbit failed", "error", err)
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Sorry, I encountered an error answering that."}},
				IsError: true,
			}, nil, nil
		}

		// Store exchange in session.
		deps.Sessions.AddExchange(sessionID, args.Question, resp.Text)

		return &gomcp.CallToolResult{
			Content: []gomcp.Content{&gomcp.TextContent{Text: resp.Text}},
		}, nil, nil
	})
}

// PRWatchReader abstracts PR watch queries for testability.
type PRWatchReader interface {
	OpenPRWatches(maxReviewRounds, maxCIFixRounds int) ([]*state.PRWatch, error)
}

type watchesArgs struct {
	// no args needed — returns all open watches
}

// RegisterWatchesTool registers the watches tool on the given MCP server.
func RegisterWatchesTool(srv *gomcp.Server, db PRWatchReader) {
	gomcp.AddTool(srv, &gomcp.Tool{
		Name:        "watches",
		Description: "List open PR watches being monitored by the review watcher. Dev-only access.",
	}, func(ctx context.Context, req *gomcp.CallToolRequest, args watchesArgs) (*gomcp.CallToolResult, any, error) {
		tok := tokenFromContext(ctx)
		if tok == nil || tok.Role != "dev" {
			return nil, nil, fmt.Errorf("access denied: dev role required")
		}

		// Use high limits to return all open watches.
		watches, err := db.OpenPRWatches(100, 100)
		if err != nil {
			return nil, nil, fmt.Errorf("reading PR watches: %w", err)
		}

		if len(watches) == 0 {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "No open PR watches."}},
			}, nil, nil
		}

		result := formatWatches(watches)
		return &gomcp.CallToolResult{
			Content: []gomcp.Content{&gomcp.TextContent{Text: result}},
		}, nil, nil
	})
}

func formatWatches(watches []*state.PRWatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d open PR watches:\n\n", len(watches))
	for _, w := range watches {
		age := time.Since(w.CreatedAt).Truncate(time.Minute)
		fmt.Fprintf(&b, "PR #%d  %s\n", w.PRNumber, w.PRURL)
		fmt.Fprintf(&b, "  Branch: %s\n", w.Branch)
		fmt.Fprintf(&b, "  Age: %s\n", age)
		fmt.Fprintf(&b, "  Review fixes: %d  CI fixes: %d  Conflict fixes: %d\n",
			w.FixCount, w.CIFixCount, w.ConflictFixCount)
		if w.OriginalSummary != "" {
			fmt.Fprintf(&b, "  Summary: %s\n", w.OriginalSummary)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// DBQuerier abstracts read-only database queries for testability.
type DBQuerier interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (RowScanner, error)
}

// RowScanner abstracts sql.Rows for testability.
type RowScanner interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...interface{}) error
	Close() error
	Err() error
}

type queryArgs struct {
	SQL   string `json:"sql" jsonschema:"Read-only SQL query to execute against the toad state database"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum rows to return (default 100, max 1000)"`
}

// RegisterQueryTool registers a read-only SQL query tool on the given MCP server.
func RegisterQueryTool(srv *gomcp.Server, db *state.DB) {
	gomcp.AddTool(srv, &gomcp.Tool{
		Name: "query",
		Description: `Execute a read-only SQL query against the toad state database. Dev-only access.

Tables: runs, thread_memory, pr_watches, daemon_stats, settings, personality_adjustments

personality_adjustments columns: id, trait, delta, source, trigger_detail, reasoning, before_value, after_value, created_at
runs columns: id, status, slack_channel, slack_thread, branch, worktree_path, task, repo_name, claim_scope, started_at, result_json, updated_at
pr_watches columns: pr_number, pr_url, branch, run_id, slack_channel, slack_thread, last_comment_id, fix_count, ci_fix_count, conflict_fix_count, repo_path, ci_exhausted_notified, created_at, closed, final_state, original_summary, original_description`,
	}, func(ctx context.Context, req *gomcp.CallToolRequest, args queryArgs) (*gomcp.CallToolResult, any, error) {
		tok := tokenFromContext(ctx)
		if tok == nil || tok.Role != "dev" {
			return nil, nil, fmt.Errorf("access denied: dev role required")
		}

		sql := strings.TrimSpace(args.SQL)
		if sql == "" {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Please provide a SQL query."}},
				IsError: true,
			}, nil, nil
		}

		// Block write operations by checking for keywords as whole words
		// (not substrings of identifiers like "personality_adjustments").
		upper := strings.ToUpper(sql)
		writeRe := regexp.MustCompile(`\b(INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|REPLACE|ATTACH|DETACH|VACUUM|REINDEX)\b`)
		if writeRe.MatchString(upper) {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Only read-only (SELECT) queries are allowed."}},
				IsError: true,
			}, nil, nil
		}

		limit := args.Limit
		if limit <= 0 {
			limit = 100
		}
		if limit > 1000 {
			limit = 1000
		}

		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		rows, err := db.QueryContext(queryCtx, sql)
		if err != nil {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Query error: " + err.Error()}},
				IsError: true,
			}, nil, nil
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "Column error: " + err.Error()}},
				IsError: true,
			}, nil, nil
		}

		var b strings.Builder
		b.WriteString(strings.Join(cols, " | "))
		b.WriteString("\n")
		b.WriteString(strings.Repeat("-", len(b.String())-1))
		b.WriteString("\n")

		count := 0
		for rows.Next() && count < limit {
			values := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return &gomcp.CallToolResult{
					Content: []gomcp.Content{&gomcp.TextContent{Text: "Scan error: " + err.Error()}},
					IsError: true,
				}, nil, nil
			}
			for i, v := range values {
				if i > 0 {
					b.WriteString(" | ")
				}
				switch val := v.(type) {
				case nil:
					b.WriteString("NULL")
				case []byte:
					s := string(val)
					if len(s) > 200 {
						s = s[:200] + "..."
					}
					b.WriteString(s)
				default:
					s := fmt.Sprintf("%v", val)
					if len(s) > 200 {
						s = s[:200] + "..."
					}
					b.WriteString(s)
				}
			}
			b.WriteString("\n")
			count++
		}

		if err := rows.Err(); err != nil {
			b.WriteString("\nRow iteration error: " + err.Error())
		}

		if count == 0 {
			return &gomcp.CallToolResult{
				Content: []gomcp.Content{&gomcp.TextContent{Text: "No rows returned."}},
			}, nil, nil
		}

		if count >= limit {
			fmt.Fprintf(&b, "\n(limited to %d rows)", limit)
		}

		return &gomcp.CallToolResult{
			Content: []gomcp.Content{&gomcp.TextContent{Text: b.String()}},
		}, nil, nil
	})
}
