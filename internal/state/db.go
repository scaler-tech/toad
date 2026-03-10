package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scaler-tech/toad/internal/toadpath"
	_ "modernc.org/sqlite" // SQLite driver
)

// dbTimeout is the default timeout for hot-path DB operations.
const dbTimeout = 10 * time.Second

func dbCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), dbTimeout)
}

// DB wraps SQLite for persistent state.
type DB struct {
	db *sql.DB
}

// OpenDB opens or creates the SQLite database at ~/.toad/state.db.
func OpenDB() (*DB, error) {
	home, err := toadpath.Home()
	if err != nil {
		return nil, fmt.Errorf("getting toad home: %w", err)
	}

	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	dbPath := filepath.Join(home, "state.db")
	return OpenDBAt(dbPath)
}

// OpenDBAt opens or creates a SQLite database at the given path.
func OpenDBAt(dbPath string) (*DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening state db: %w", err)
	}

	// WAL mode for better concurrent read/write
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting WAL mode: %w", err)
	}

	// Wait up to 5s on write contention instead of failing immediately with SQLITE_BUSY
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting busy timeout: %w", err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating state db: %w", err)
	}

	return &DB{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			id            TEXT PRIMARY KEY,
			status        TEXT NOT NULL,
			slack_channel TEXT,
			slack_thread  TEXT,
			branch        TEXT,
			worktree_path TEXT,
			task          TEXT,
			repo_name     TEXT DEFAULT '',
			started_at    DATETIME NOT NULL,
			result_json   TEXT,
			updated_at    DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
		CREATE INDEX IF NOT EXISTS idx_runs_thread ON runs(slack_thread);

		CREATE TABLE IF NOT EXISTS thread_memory (
			thread_ts   TEXT PRIMARY KEY,
			channel     TEXT NOT NULL,
			triage_json TEXT,
			response    TEXT,
			created_at  DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS pr_watches (
			pr_number              INTEGER PRIMARY KEY,
			pr_url                 TEXT NOT NULL,
			branch                 TEXT NOT NULL,
			run_id                 TEXT NOT NULL,
			slack_channel          TEXT,
			slack_thread           TEXT,
			last_comment_id        INTEGER DEFAULT 0,
			fix_count              INTEGER DEFAULT 0,
			ci_fix_count           INTEGER DEFAULT 0,
			conflict_fix_count     INTEGER DEFAULT 0,
			repo_path              TEXT DEFAULT '',
			ci_exhausted_notified  BOOLEAN DEFAULT FALSE,
			created_at             DATETIME NOT NULL,
			closed                 BOOLEAN DEFAULT FALSE,
			final_state            TEXT DEFAULT '',
			original_summary       TEXT DEFAULT '',
			original_description   TEXT DEFAULT ''
		);

		CREATE TABLE IF NOT EXISTS daemon_stats (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			stats_json TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS digest_opportunities (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			summary       TEXT NOT NULL,
			category      TEXT NOT NULL,
			confidence    REAL NOT NULL,
			est_size      TEXT NOT NULL,
			channel       TEXT,
			message       TEXT,
			keywords      TEXT,
			dry_run       BOOLEAN NOT NULL DEFAULT TRUE,
			dismissed     BOOLEAN NOT NULL DEFAULT FALSE,
			reasoning     TEXT NOT NULL DEFAULT '',
			investigating BOOLEAN NOT NULL DEFAULT FALSE,
			created_at    DATETIME NOT NULL
		);
	`)
	if err != nil {
		return err
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS mcp_tokens (
		token TEXT PRIMARY KEY,
		slack_user_id TEXT NOT NULL,
		slack_user TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT 'user',
		created_at DATETIME NOT NULL,
		last_used_at DATETIME
	)`)
	if err != nil {
		return fmt.Errorf("creating mcp_tokens table: %w", err)
	}

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS github_slack_mappings (
		slack_user_id TEXT NOT NULL,
		github_login  TEXT NOT NULL COLLATE NOCASE,
		created_at    DATETIME NOT NULL,
		UNIQUE(github_login)
	)`)
	if err != nil {
		return fmt.Errorf("creating github_slack_mappings table: %w", err)
	}

	// Add columns for existing databases that predate the investigation gate.
	// SQLite has no IF NOT EXISTS for ALTER TABLE, so check first.
	var count int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('digest_opportunities') WHERE name = 'dismissed'`).Scan(&count)
	if count == 0 {
		if _, err := db.Exec(`ALTER TABLE digest_opportunities ADD COLUMN dismissed BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
			slog.Warn("migration: failed to add dismissed column", "error", err)
		}
		if _, err := db.Exec(`ALTER TABLE digest_opportunities ADD COLUMN reasoning TEXT NOT NULL DEFAULT ''`); err != nil {
			slog.Warn("migration: failed to add reasoning column", "error", err)
		}
	}

	// Add ci_fix_count column for existing databases that predate CI fix watching.
	var ciFixCountExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pr_watches') WHERE name = 'ci_fix_count'`).Scan(&ciFixCountExists)
	if ciFixCountExists == 0 {
		if _, err := db.Exec(`ALTER TABLE pr_watches ADD COLUMN ci_fix_count INTEGER DEFAULT 0`); err != nil {
			slog.Warn("migration: failed to add ci_fix_count column", "error", err)
		}
	}

	// Add ci_exhausted_notified column for existing databases that predate zombie watch fix.
	var ciExhaustedExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pr_watches') WHERE name = 'ci_exhausted_notified'`).Scan(&ciExhaustedExists)
	if ciExhaustedExists == 0 {
		if _, err := db.Exec(`ALTER TABLE pr_watches ADD COLUMN ci_exhausted_notified BOOLEAN DEFAULT FALSE`); err != nil {
			slog.Warn("migration: failed to add ci_exhausted_notified column", "error", err)
		}
	}

	// Add conflict_fix_count column for existing databases that predate merge conflict watching.
	var conflictFixCountExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pr_watches') WHERE name = 'conflict_fix_count'`).Scan(&conflictFixCountExists)
	if conflictFixCountExists == 0 {
		if _, err := db.Exec(`ALTER TABLE pr_watches ADD COLUMN conflict_fix_count INTEGER DEFAULT 0`); err != nil {
			slog.Warn("migration: failed to add conflict_fix_count column", "error", err)
		}
	}

	// Add investigating column for existing databases that predate investigation visibility.
	var investigatingExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('digest_opportunities') WHERE name = 'investigating'`).Scan(&investigatingExists)
	if investigatingExists == 0 {
		if _, err := db.Exec(`ALTER TABLE digest_opportunities ADD COLUMN investigating BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
			slog.Warn("migration: failed to add investigating column", "error", err)
		}
	}

	// Add channel_id and thread_ts columns for Slack deep links from the dashboard.
	var channelIDExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('digest_opportunities') WHERE name = 'channel_id'`).Scan(&channelIDExists)
	if channelIDExists == 0 {
		if _, err := db.Exec(`ALTER TABLE digest_opportunities ADD COLUMN channel_id TEXT DEFAULT ''`); err != nil {
			slog.Warn("migration: failed to add channel_id column", "error", err)
		}
		if _, err := db.Exec(`ALTER TABLE digest_opportunities ADD COLUMN thread_ts TEXT DEFAULT ''`); err != nil {
			slog.Warn("migration: failed to add thread_ts column", "error", err)
		}
	}

	// Add final_state column for existing databases that predate merge rate tracking.
	var finalStateExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pr_watches') WHERE name = 'final_state'`).Scan(&finalStateExists)
	if finalStateExists == 0 {
		if _, err := db.Exec(`ALTER TABLE pr_watches ADD COLUMN final_state TEXT DEFAULT ''`); err != nil {
			slog.Warn("migration: failed to add final_state column", "error", err)
		}
	}

	// Add original_summary/original_description columns so CI/review fix tadpoles
	// know the original PR intent and don't blindly revert changes.
	var origSummaryExists int
	_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pr_watches') WHERE name = 'original_summary'`).Scan(&origSummaryExists)
	if origSummaryExists == 0 {
		if _, err := db.Exec(`ALTER TABLE pr_watches ADD COLUMN original_summary TEXT DEFAULT ''`); err != nil {
			slog.Warn("migration: failed to add original_summary column", "error", err)
		}
		if _, err := db.Exec(`ALTER TABLE pr_watches ADD COLUMN original_description TEXT DEFAULT ''`); err != nil {
			slog.Warn("migration: failed to add original_description column", "error", err)
		}
	}

	return nil
}

// SaveRun inserts or replaces a run in the database.
func (d *DB) SaveRun(run *Run) error {
	var resultJSON []byte
	if run.Result != nil {
		var err error
		resultJSON, err = json.Marshal(run.Result)
		if err != nil {
			return fmt.Errorf("marshaling result: %w", err)
		}
	}

	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO runs (id, status, slack_channel, slack_thread, branch, worktree_path, task, repo_name, started_at, result_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Status, run.SlackChannel, run.SlackThreadTS,
		run.Branch, run.WorktreePath, run.Task, run.RepoName, run.StartedAt,
		string(resultJSON), time.Now(),
	)
	return err
}

// UpdateStatus updates the status of a run.
func (d *DB) UpdateStatus(runID, status string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE runs SET status = ?, updated_at = ? WHERE id = ?",
		status, time.Now(), runID,
	)
	return err
}

// CompleteRun marks a run as done or failed with a result.
func (d *DB) CompleteRun(runID string, result *RunResult) error {
	status := "done"
	if !result.Success {
		status = "failed"
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshaling result: %w", err)
	}

	ctx, cancel := dbCtx()
	defer cancel()
	_, err = d.db.ExecContext(ctx,
		"UPDATE runs SET status = ?, result_json = ?, updated_at = ? WHERE id = ?",
		status, string(resultJSON), time.Now(), runID,
	)
	return err
}

// GetByThread looks up a run by its Slack thread timestamp.
// Returns nil if not found.
func (d *DB) GetByThread(threadTS string) (*Run, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	row := d.db.QueryRowContext(ctx,
		"SELECT id, status, slack_channel, slack_thread, branch, worktree_path, task, repo_name, started_at, result_json FROM runs WHERE slack_thread = ? AND status NOT IN ('done', 'failed') LIMIT 1",
		threadTS,
	)
	return scanRun(row)
}

// ActiveRuns returns all runs in active states.
func (d *DB) ActiveRuns() ([]*Run, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, status, slack_channel, slack_thread, branch, worktree_path, task, repo_name, started_at, result_json FROM runs WHERE status NOT IN ('done', 'failed') ORDER BY started_at",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// History returns completed runs, most recent first.
func (d *DB) History(limit int) ([]*Run, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, status, slack_channel, slack_thread, branch, worktree_path, task, repo_name, started_at, result_json FROM runs WHERE status IN ('done', 'failed') ORDER BY started_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// HasWorktree checks if any active run references the given worktree path.
func (d *DB) HasWorktree(path string) bool {
	ctx, cancel := dbCtx()
	defer cancel()
	var count int
	err := d.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM runs WHERE worktree_path = ? AND status NOT IN ('done', 'failed')",
		path,
	).Scan(&count)
	if err != nil {
		slog.Warn("HasWorktree query failed, assuming not tracked", "path", path, "error", err)
		return false
	}
	return count > 0
}

// SaveThreadMemory stores triage + response context for a thread.
func (d *DB) SaveThreadMemory(threadTS, channel, triageJSON, response string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO thread_memory (thread_ts, channel, triage_json, response, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		threadTS, channel, triageJSON, response, time.Now(),
	)
	return err
}

// GetThreadMemory retrieves cached context for a thread.
func (d *DB) GetThreadMemory(threadTS string) (*ThreadMemory, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	row := d.db.QueryRowContext(ctx,
		"SELECT thread_ts, channel, triage_json, response, created_at FROM thread_memory WHERE thread_ts = ?",
		threadTS,
	)
	var mem ThreadMemory
	err := row.Scan(&mem.ThreadTS, &mem.Channel, &mem.TriageJSON, &mem.Response, &mem.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &mem, nil
}

// PruneThreadMemory removes thread memories older than the given duration.
func (d *DB) PruneThreadMemory(olderThan time.Duration) (int, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	cutoff := time.Now().Add(-olderThan)
	result, err := d.db.ExecContext(ctx, "DELETE FROM thread_memory WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// SavePRWatch registers a PR for review comment monitoring.
func (d *DB) SavePRWatch(prNumber int, prURL, branch, runID, slackChannel, slackThread, repoPath, originalSummary, originalDescription string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		INSERT INTO pr_watches (pr_number, pr_url, branch, run_id, slack_channel, slack_thread, repo_path, original_summary, original_description, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pr_number) DO UPDATE SET
			pr_url = excluded.pr_url,
			branch = excluded.branch,
			run_id = excluded.run_id,
			slack_channel = excluded.slack_channel,
			slack_thread = excluded.slack_thread,
			repo_path = excluded.repo_path,
			original_summary = excluded.original_summary,
			original_description = excluded.original_description`,
		prNumber, prURL, branch, runID, slackChannel, slackThread, repoPath, originalSummary, originalDescription, time.Now(),
	)
	return err
}

// OpenPRWatches returns all PRs being monitored (not closed, under any fix limit).
func (d *DB) OpenPRWatches(maxReviewRounds, maxCIFixRounds int) ([]*PRWatch, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		"SELECT pr_number, pr_url, branch, run_id, slack_channel, slack_thread, last_comment_id, fix_count, ci_fix_count, conflict_fix_count, repo_path, ci_exhausted_notified, COALESCE(original_summary, ''), COALESCE(original_description, ''), created_at FROM pr_watches WHERE closed = FALSE AND (fix_count < ? OR ci_fix_count < ? OR conflict_fix_count < ?)",
		maxReviewRounds, maxCIFixRounds, maxReviewRounds,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var watches []*PRWatch
	for rows.Next() {
		var w PRWatch
		if err := rows.Scan(&w.PRNumber, &w.PRURL, &w.Branch, &w.RunID, &w.SlackChannel, &w.SlackThread, &w.LastCommentID, &w.FixCount, &w.CIFixCount, &w.ConflictFixCount, &w.RepoPath, &w.CIExhaustedNotified, &w.OriginalSummary, &w.OriginalDescription, &w.CreatedAt); err != nil {
			return nil, err
		}
		watches = append(watches, &w)
	}
	return watches, rows.Err()
}

// UpdatePRWatchLastComment updates the last seen comment ID.
func (d *DB) UpdatePRWatchLastComment(prNumber, lastCommentID int) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE pr_watches SET last_comment_id = ? WHERE pr_number = ?",
		lastCommentID, prNumber,
	)
	return err
}

// IncrementReviewFixCount bumps the review fix attempt counter for a PR watch.
func (d *DB) IncrementReviewFixCount(prNumber int) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE pr_watches SET fix_count = fix_count + 1 WHERE pr_number = ?",
		prNumber,
	)
	return err
}

// IncrementCIFixCount bumps the CI fix attempt counter for a PR watch.
func (d *DB) IncrementCIFixCount(prNumber int) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE pr_watches SET ci_fix_count = ci_fix_count + 1 WHERE pr_number = ?",
		prNumber,
	)
	return err
}

// IncrementConflictFixCount bumps the merge conflict fix attempt counter for a PR watch.
func (d *DB) IncrementConflictFixCount(prNumber int) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE pr_watches SET conflict_fix_count = conflict_fix_count + 1 WHERE pr_number = ?",
		prNumber,
	)
	return err
}

// ResetCIFixCount resets the CI fix counter and exhaustion flag for a PR watch.
// Called when a new push is made (e.g. after a review fix), so CI gets a fresh budget.
func (d *DB) ResetCIFixCount(prNumber int) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE pr_watches SET ci_fix_count = 0, ci_exhausted_notified = FALSE WHERE pr_number = ?",
		prNumber,
	)
	return err
}

// MarkCIExhaustedNotified marks that the CI exhaustion notification has been sent for a PR.
func (d *DB) MarkCIExhaustedNotified(prNumber int) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE pr_watches SET ci_exhausted_notified = TRUE WHERE pr_number = ?",
		prNumber,
	)
	return err
}

// ClosePRWatch marks a PR watch as closed with its final state (e.g. "MERGED", "CLOSED").
func (d *DB) ClosePRWatch(prNumber int, finalState string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, "UPDATE pr_watches SET closed = TRUE, final_state = ? WHERE pr_number = ?", finalState, prNumber)
	return err
}

// Stats holds aggregate metrics across all completed runs.
type Stats struct {
	TotalRuns   int
	Succeeded   int
	Failed      int
	TotalCost   float64
	AvgDuration time.Duration
	ThreadCount int
}

// Stats returns aggregate metrics for completed runs and thread memory count.
func (d *DB) Stats() (*Stats, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		"SELECT status, result_json FROM runs WHERE status IN ('done', 'failed')",
	)
	if err != nil {
		return nil, fmt.Errorf("querying runs: %w", err)
	}
	defer rows.Close()

	var s Stats
	var totalDuration time.Duration
	for rows.Next() {
		var status string
		var resultJSON sql.NullString
		if err := rows.Scan(&status, &resultJSON); err != nil {
			return nil, fmt.Errorf("scanning run: %w", err)
		}
		s.TotalRuns++
		if status == "done" {
			s.Succeeded++
		} else {
			s.Failed++
		}
		if resultJSON.Valid && resultJSON.String != "" {
			var result RunResult
			if err := json.Unmarshal([]byte(resultJSON.String), &result); err == nil {
				s.TotalCost += result.Cost
				totalDuration += result.Duration
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating runs: %w", err)
	}

	if s.TotalRuns > 0 {
		s.AvgDuration = totalDuration / time.Duration(s.TotalRuns)
	}

	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM thread_memory").Scan(&s.ThreadCount); err != nil {
		return nil, fmt.Errorf("counting threads: %w", err)
	}

	return &s, nil
}

// MergeStats holds aggregate PR outcome metrics.
type MergeStats struct {
	PRsCreated int
	PRsMerged  int
	PRsClosed  int
	PRsOpen    int
}

// MergeRate returns the merge rate as a percentage (0-100), or -1 if no PRs exist.
func (s *MergeStats) MergeRate() float64 {
	total := s.PRsMerged + s.PRsClosed
	if total == 0 {
		return -1
	}
	return float64(s.PRsMerged) / float64(total) * 100
}

// MergeStats returns aggregate PR outcome metrics from pr_watches.
func (d *DB) MergeStats() (*MergeStats, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	var s MergeStats
	err := d.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN UPPER(final_state) = 'MERGED' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN closed = TRUE AND UPPER(final_state) != 'MERGED' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN closed = FALSE THEN 1 ELSE 0 END), 0)
		FROM pr_watches`,
	).Scan(&s.PRsCreated, &s.PRsMerged, &s.PRsClosed, &s.PRsOpen)
	if err != nil {
		return nil, fmt.Errorf("querying merge stats: %w", err)
	}
	return &s, nil
}

// DigestOpportunity represents a potential one-shot fix found by the digest engine.
type DigestOpportunity struct {
	ID            int
	Summary       string
	Category      string
	Confidence    float64
	EstSize       string
	Channel       string
	ChannelID     string
	ThreadTS      string
	Message       string
	Keywords      string
	DryRun        bool
	Dismissed     bool
	Reasoning     string
	Investigating bool
	CreatedAt     time.Time
}

// SaveDigestOpportunity persists a digest opportunity to the database.
// Returns the auto-generated ID in opp.ID.
func (d *DB) SaveDigestOpportunity(opp *DigestOpportunity) error {
	ctx, cancel := dbCtx()
	defer cancel()
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO digest_opportunities (summary, category, confidence, est_size, channel, channel_id, thread_ts, message, keywords, dry_run, dismissed, reasoning, investigating, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		opp.Summary, opp.Category, opp.Confidence, opp.EstSize,
		opp.Channel, opp.ChannelID, opp.ThreadTS, opp.Message, opp.Keywords, opp.DryRun,
		opp.Dismissed, opp.Reasoning, opp.Investigating, opp.CreatedAt,
	)
	if err != nil {
		return err
	}
	id, _ := result.LastInsertId()
	opp.ID = int(id)
	return nil
}

// UpdateDigestOpportunity updates an existing opportunity after investigation completes.
func (d *DB) UpdateDigestOpportunity(opp *DigestOpportunity) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		UPDATE digest_opportunities SET dry_run = ?, dismissed = ?, reasoning = ?, investigating = ?
		WHERE id = ?`,
		opp.DryRun, opp.Dismissed, opp.Reasoning, opp.Investigating, opp.ID,
	)
	return err
}

// StaleInvestigations returns opportunities stuck in investigating state.
// The rows are left in the DB so they survive another crash during resume.
func (d *DB) StaleInvestigations() ([]*DigestOpportunity, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, summary, category, confidence, est_size, channel, COALESCE(channel_id,''), COALESCE(thread_ts,''), message, keywords, dry_run
		FROM digest_opportunities WHERE investigating = TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var opps []*DigestOpportunity
	for rows.Next() {
		o := &DigestOpportunity{}
		if err := rows.Scan(&o.ID, &o.Summary, &o.Category, &o.Confidence, &o.EstSize,
			&o.Channel, &o.ChannelID, &o.ThreadTS, &o.Message, &o.Keywords, &o.DryRun); err != nil {
			return nil, err
		}
		opps = append(opps, o)
	}
	return opps, nil
}

// HasRecentOpportunity checks if a similar opportunity was already processed
// within the given duration. Uses keyword overlap to catch semantically
// equivalent issues that Haiku summarized with slightly different wording.
// Falls back to exact summary match when keywords are unavailable.
func (d *DB) HasRecentOpportunity(summary string, keywords string, within time.Duration) (bool, error) {
	cutoff := time.Now().Add(-within)

	ctx, cancel := dbCtx()
	defer cancel()

	// Fast path: exact summary match
	var count int
	err := d.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM digest_opportunities WHERE summary = ? AND created_at > ?",
		summary, cutoff,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}

	// Fuzzy path: keyword overlap with recent opportunities
	if keywords == "" {
		return false, nil
	}

	rows, err := d.db.QueryContext(ctx,
		"SELECT keywords FROM digest_opportunities WHERE created_at > ? AND keywords != ''",
		cutoff,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	newKW := normalizeKeywords(keywords)
	for rows.Next() {
		var existingKW string
		if err := rows.Scan(&existingKW); err != nil {
			continue
		}
		if keywordOverlap(newKW, normalizeKeywords(existingKW)) >= 0.5 {
			return true, nil
		}
	}
	return false, rows.Err()
}

// normalizeKeywords splits a comma-separated keyword string into a set of
// lowercased terms. Multi-word keywords are also split into individual words
// so that "red dot indicator" matches "red dot" and "indicator".
func normalizeKeywords(kw string) map[string]bool {
	set := make(map[string]bool)
	for _, part := range strings.Split(kw, ",") {
		for _, word := range strings.Fields(strings.ToLower(strings.TrimSpace(part))) {
			if len(word) > 1 { // skip single-char noise
				set[word] = true
			}
		}
	}
	return set
}

// keywordOverlap returns the Jaccard similarity between two keyword sets.
func keywordOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for k := range a {
		if b[k] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// RecentDigestOpportunities returns the most recent digest opportunities, newest first.
func (d *DB) RecentDigestOpportunities(limit int) ([]*DigestOpportunity, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, summary, category, confidence, est_size, channel, COALESCE(channel_id,''), COALESCE(thread_ts,''), message, keywords, dry_run, dismissed, reasoning, investigating, created_at FROM digest_opportunities ORDER BY created_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var opps []*DigestOpportunity
	for rows.Next() {
		var opp DigestOpportunity
		if err := rows.Scan(&opp.ID, &opp.Summary, &opp.Category, &opp.Confidence, &opp.EstSize, &opp.Channel, &opp.ChannelID, &opp.ThreadTS, &opp.Message, &opp.Keywords, &opp.DryRun, &opp.Dismissed, &opp.Reasoning, &opp.Investigating, &opp.CreatedAt); err != nil {
			return nil, err
		}
		opps = append(opps, &opp)
	}
	return opps, rows.Err()
}

// DigestCounts holds aggregate counts across all digest opportunities.
type DigestCounts struct {
	Approved      int
	Dismissed     int
	DryRun        int
	Investigating int
}

// DigestOpportunityCounts returns aggregate counts across all opportunities in the DB.
func (d *DB) DigestOpportunityCounts() (*DigestCounts, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	var c DigestCounts
	err := d.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN investigating = TRUE THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN dismissed = TRUE AND investigating = FALSE THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN dry_run = TRUE AND dismissed = FALSE AND investigating = FALSE THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN dry_run = FALSE AND dismissed = FALSE AND investigating = FALSE THEN 1 ELSE 0 END), 0)
		FROM digest_opportunities`,
	).Scan(&c.Investigating, &c.Dismissed, &c.DryRun, &c.Approved)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// DaemonStats holds live daemon metrics written periodically while running.
type DaemonStats struct {
	Heartbeat        time.Time        `json:"heartbeat"`
	StartedAt        time.Time        `json:"started_at"`
	PID              int              `json:"pid"`
	Version          string           `json:"version,omitempty"`
	Draining         bool             `json:"draining,omitempty"`
	Ribbits          int64            `json:"ribbits"`
	Triages          int64            `json:"triages"`
	TriageByCategory map[string]int64 `json:"triage_by_category"`
	DigestEnabled    bool             `json:"digest_enabled"`
	DigestDryRun     bool             `json:"digest_dry_run"`
	DigestBuffer     int              `json:"digest_buffer"`
	DigestNextFlush  time.Time        `json:"digest_next_flush"`
	DigestProcessed  int64            `json:"digest_processed"`
	DigestOpps       int64            `json:"digest_opportunities"`
	DigestSpawns     int64            `json:"digest_spawns"`
	IssueTracker     bool             `json:"issue_tracker,omitempty"`
	IssueProvider    string           `json:"issue_provider,omitempty"`
	MCPEnabled       bool             `json:"mcp_enabled,omitempty"`
	MCPHost          string           `json:"mcp_host,omitempty"`
	MCPPort          int              `json:"mcp_port,omitempty"`
}

// WriteDaemonStats upserts the daemon's live stats (single row).
func (d *DB) WriteDaemonStats(stats *DaemonStats) error {
	data, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshaling daemon stats: %w", err)
	}
	ctx, cancel := dbCtx()
	defer cancel()
	_, err = d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO daemon_stats (id, stats_json, updated_at)
		VALUES (1, ?, ?)`,
		string(data), time.Now(),
	)
	return err
}

// ReadDaemonStats reads the daemon's live stats. Returns nil if never written.
func (d *DB) ReadDaemonStats() (*DaemonStats, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	var data string
	err := d.db.QueryRowContext(ctx, "SELECT stats_json FROM daemon_stats WHERE id = 1").Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stats DaemonStats
	if err := json.Unmarshal([]byte(data), &stats); err != nil {
		return nil, fmt.Errorf("parsing daemon stats: %w", err)
	}
	return &stats, nil
}

// GetSetting reads a setting value by key. Returns "" if not found.
func (d *DB) GetSetting(key string) (string, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	var value string
	err := d.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// SetSetting upserts a setting value.
func (d *DB) SetSetting(key, value string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, "INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)", key, value)
	return err
}

// ClearDaemonStats removes daemon stats (called on clean shutdown).
func (d *DB) ClearDaemonStats() error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, "DELETE FROM daemon_stats")
	return err
}

// SaveMCPToken inserts or replaces an MCP token.
func (d *DB) SaveMCPToken(tok *MCPToken) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO mcp_tokens (token, slack_user_id, slack_user, role, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		tok.Token, tok.SlackUserID, tok.SlackUser, tok.Role, tok.CreatedAt, tok.LastUsedAt,
	)
	return err
}

// ValidateMCPToken looks up a token and updates its last_used_at timestamp.
// Returns nil, nil if the token is not found.
func (d *DB) ValidateMCPToken(token string) (*MCPToken, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	var tok MCPToken
	var lastUsed sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT token, slack_user_id, slack_user, role, created_at, last_used_at FROM mcp_tokens WHERE token = ?",
		token,
	).Scan(&tok.Token, &tok.SlackUserID, &tok.SlackUser, &tok.Role, &tok.CreatedAt, &lastUsed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		tok.LastUsedAt = lastUsed.Time
	}

	// Update last_used_at
	now := time.Now()
	_, _ = d.db.ExecContext(ctx, "UPDATE mcp_tokens SET last_used_at = ? WHERE token = ?", now, token)
	tok.LastUsedAt = now

	return &tok, nil
}

// GetMCPTokenByUser looks up a token by Slack user ID (without updating last_used_at).
func (d *DB) GetMCPTokenByUser(slackUserID string) (*MCPToken, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	var tok MCPToken
	var lastUsed sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT token, slack_user_id, slack_user, role, created_at, last_used_at FROM mcp_tokens WHERE slack_user_id = ?",
		slackUserID,
	).Scan(&tok.Token, &tok.SlackUserID, &tok.SlackUser, &tok.Role, &tok.CreatedAt, &lastUsed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		tok.LastUsedAt = lastUsed.Time
	}
	return &tok, nil
}

// RevokeMCPToken deletes all tokens for the given Slack user ID.
func (d *DB) RevokeMCPToken(slackUserID string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, "DELETE FROM mcp_tokens WHERE slack_user_id = ?", slackUserID)
	return err
}

// AddGitHubMapping links a GitHub login to a Slack user ID.
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

// RemoveGitHubMapping removes a GitHub login mapping for a Slack user.
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

// LookupSlackByGitHub returns the Slack user ID for a GitHub login.
// Returns "", nil if not found.
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

// ListGitHubMappings returns all GitHub logins mapped to a Slack user ID.
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

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// PRWatch represents a monitored toad PR.
type PRWatch struct {
	PRNumber            int
	PRURL               string
	Branch              string
	RunID               string
	SlackChannel        string
	SlackThread         string
	LastCommentID       int
	FixCount            int
	CIFixCount          int
	ConflictFixCount    int
	RepoPath            string
	CIExhaustedNotified bool
	OriginalSummary     string
	OriginalDescription string
	CreatedAt           time.Time
}

// MCPToken represents an MCP authentication token linked to a Slack user.
type MCPToken struct {
	Token       string
	SlackUserID string
	SlackUser   string
	Role        string // "dev" or "user"
	CreatedAt   time.Time
	LastUsedAt  time.Time
}

// ThreadMemory holds cached context for a Slack thread.
type ThreadMemory struct {
	ThreadTS   string
	Channel    string
	TriageJSON string
	Response   string
	CreatedAt  time.Time
}

// ThreadMemoryTTL is how long thread memories are kept.
const ThreadMemoryTTL = 24 * time.Hour

func scanRun(row *sql.Row) (*Run, error) {
	var run Run
	var resultJSON sql.NullString
	err := row.Scan(
		&run.ID, &run.Status, &run.SlackChannel, &run.SlackThreadTS,
		&run.Branch, &run.WorktreePath, &run.Task, &run.RepoName, &run.StartedAt, &resultJSON,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if resultJSON.Valid && resultJSON.String != "" {
		var result RunResult
		if err := json.Unmarshal([]byte(resultJSON.String), &result); err == nil {
			run.Result = &result
		}
	}
	return &run, nil
}

func scanRuns(rows *sql.Rows) ([]*Run, error) {
	var runs []*Run
	for rows.Next() {
		var run Run
		var resultJSON sql.NullString
		if err := rows.Scan(
			&run.ID, &run.Status, &run.SlackChannel, &run.SlackThreadTS,
			&run.Branch, &run.WorktreePath, &run.Task, &run.RepoName, &run.StartedAt, &resultJSON,
		); err != nil {
			return nil, err
		}
		if resultJSON.Valid && resultJSON.String != "" {
			var result RunResult
			if err := json.Unmarshal([]byte(resultJSON.String), &result); err == nil {
				run.Result = &result
			}
		}
		runs = append(runs, &run)
	}
	return runs, rows.Err()
}
