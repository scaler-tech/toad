package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // SQLite driver
)

// DB wraps SQLite for persistent state.
type DB struct {
	db *sql.DB
}

// OpenDB opens or creates the SQLite database at ~/.toad/state.db.
func OpenDB() (*DB, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home dir: %w", err)
	}

	dbDir := filepath.Join(homeDir, ".toad")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating db directory: %w", err)
	}

	dbPath := filepath.Join(dbDir, "state.db")
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
			pr_number       INTEGER PRIMARY KEY,
			pr_url          TEXT NOT NULL,
			branch          TEXT NOT NULL,
			run_id          TEXT NOT NULL,
			slack_channel   TEXT,
			slack_thread    TEXT,
			last_comment_id INTEGER DEFAULT 0,
			fix_count       INTEGER DEFAULT 0,
			created_at      DATETIME NOT NULL,
			closed          BOOLEAN DEFAULT FALSE
		);

		CREATE TABLE IF NOT EXISTS daemon_stats (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			stats_json TEXT NOT NULL,
			updated_at DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS digest_opportunities (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			summary    TEXT NOT NULL,
			category   TEXT NOT NULL,
			confidence REAL NOT NULL,
			est_size   TEXT NOT NULL,
			channel    TEXT,
			message    TEXT,
			keywords   TEXT,
			dry_run    BOOLEAN NOT NULL DEFAULT TRUE,
			dismissed  BOOLEAN NOT NULL DEFAULT FALSE,
			reasoning  TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		);
	`)
	if err != nil {
		return err
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

	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO runs (id, status, slack_channel, slack_thread, branch, worktree_path, task, started_at, result_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Status, run.SlackChannel, run.SlackThreadTS,
		run.Branch, run.WorktreePath, run.Task, run.StartedAt,
		string(resultJSON), time.Now(),
	)
	return err
}

// UpdateStatus updates the status of a run.
func (d *DB) UpdateStatus(runID, status string) error {
	_, err := d.db.Exec(
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

	_, err = d.db.Exec(
		"UPDATE runs SET status = ?, result_json = ?, updated_at = ? WHERE id = ?",
		status, string(resultJSON), time.Now(), runID,
	)
	return err
}

// GetByThread looks up a run by its Slack thread timestamp.
// Returns nil if not found.
func (d *DB) GetByThread(threadTS string) (*Run, error) {
	row := d.db.QueryRow(
		"SELECT id, status, slack_channel, slack_thread, branch, worktree_path, task, started_at, result_json FROM runs WHERE slack_thread = ? AND status NOT IN ('done', 'failed') LIMIT 1",
		threadTS,
	)
	return scanRun(row)
}

// ActiveRuns returns all runs in active states.
func (d *DB) ActiveRuns() ([]*Run, error) {
	rows, err := d.db.Query(
		"SELECT id, status, slack_channel, slack_thread, branch, worktree_path, task, started_at, result_json FROM runs WHERE status NOT IN ('done', 'failed') ORDER BY started_at",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// History returns completed runs, most recent first.
func (d *DB) History(limit int) ([]*Run, error) {
	rows, err := d.db.Query(
		"SELECT id, status, slack_channel, slack_thread, branch, worktree_path, task, started_at, result_json FROM runs WHERE status IN ('done', 'failed') ORDER BY started_at DESC LIMIT ?",
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
	var count int
	err := d.db.QueryRow(
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
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO thread_memory (thread_ts, channel, triage_json, response, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		threadTS, channel, triageJSON, response, time.Now(),
	)
	return err
}

// GetThreadMemory retrieves cached context for a thread.
func (d *DB) GetThreadMemory(threadTS string) (*ThreadMemory, error) {
	row := d.db.QueryRow(
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
	cutoff := time.Now().Add(-olderThan)
	result, err := d.db.Exec("DELETE FROM thread_memory WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// SavePRWatch registers a PR for review comment monitoring.
func (d *DB) SavePRWatch(prNumber int, prURL, branch, runID, slackChannel, slackThread string) error {
	_, err := d.db.Exec(`
		INSERT OR REPLACE INTO pr_watches (pr_number, pr_url, branch, run_id, slack_channel, slack_thread, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		prNumber, prURL, branch, runID, slackChannel, slackThread, time.Now(),
	)
	return err
}

// OpenPRWatches returns all PRs being monitored (not closed, under fix limit).
func (d *DB) OpenPRWatches(maxReviewRounds int) ([]*PRWatch, error) {
	rows, err := d.db.Query(
		"SELECT pr_number, pr_url, branch, run_id, slack_channel, slack_thread, last_comment_id, fix_count FROM pr_watches WHERE closed = FALSE AND fix_count < ?",
		maxReviewRounds,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var watches []*PRWatch
	for rows.Next() {
		var w PRWatch
		if err := rows.Scan(&w.PRNumber, &w.PRURL, &w.Branch, &w.RunID, &w.SlackChannel, &w.SlackThread, &w.LastCommentID, &w.FixCount); err != nil {
			return nil, err
		}
		watches = append(watches, &w)
	}
	return watches, rows.Err()
}

// UpdatePRWatchLastComment updates the last seen comment ID and increments fix count.
func (d *DB) UpdatePRWatchLastComment(prNumber, lastCommentID int) error {
	_, err := d.db.Exec(
		"UPDATE pr_watches SET last_comment_id = ?, fix_count = fix_count + 1 WHERE pr_number = ?",
		lastCommentID, prNumber,
	)
	return err
}

// ClosePRWatch marks a PR watch as closed (merged/closed).
func (d *DB) ClosePRWatch(prNumber int) error {
	_, err := d.db.Exec("UPDATE pr_watches SET closed = TRUE WHERE pr_number = ?", prNumber)
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
	rows, err := d.db.Query(
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

	if err := d.db.QueryRow("SELECT COUNT(*) FROM thread_memory").Scan(&s.ThreadCount); err != nil {
		return nil, fmt.Errorf("counting threads: %w", err)
	}

	return &s, nil
}

// DigestOpportunity represents a potential one-shot fix found by the digest engine.
type DigestOpportunity struct {
	ID         int
	Summary    string
	Category   string
	Confidence float64
	EstSize    string
	Channel    string
	Message    string
	Keywords   string
	DryRun     bool
	Dismissed  bool
	Reasoning  string
	CreatedAt  time.Time
}

// SaveDigestOpportunity persists a digest opportunity to the database.
func (d *DB) SaveDigestOpportunity(opp *DigestOpportunity) error {
	_, err := d.db.Exec(`
		INSERT INTO digest_opportunities (summary, category, confidence, est_size, channel, message, keywords, dry_run, dismissed, reasoning, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		opp.Summary, opp.Category, opp.Confidence, opp.EstSize,
		opp.Channel, opp.Message, opp.Keywords, opp.DryRun,
		opp.Dismissed, opp.Reasoning, opp.CreatedAt,
	)
	return err
}

// HasRecentOpportunity checks if a similar opportunity was already processed
// within the given duration. Uses keyword overlap to catch semantically
// equivalent issues that Haiku summarized with slightly different wording.
// Falls back to exact summary match when keywords are unavailable.
func (d *DB) HasRecentOpportunity(summary string, keywords string, within time.Duration) (bool, error) {
	cutoff := time.Now().Add(-within)

	// Fast path: exact summary match
	var count int
	err := d.db.QueryRow(
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

	rows, err := d.db.Query(
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
	rows, err := d.db.Query(
		"SELECT id, summary, category, confidence, est_size, channel, message, keywords, dry_run, dismissed, reasoning, created_at FROM digest_opportunities ORDER BY created_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var opps []*DigestOpportunity
	for rows.Next() {
		var opp DigestOpportunity
		if err := rows.Scan(&opp.ID, &opp.Summary, &opp.Category, &opp.Confidence, &opp.EstSize, &opp.Channel, &opp.Message, &opp.Keywords, &opp.DryRun, &opp.Dismissed, &opp.Reasoning, &opp.CreatedAt); err != nil {
			return nil, err
		}
		opps = append(opps, &opp)
	}
	return opps, rows.Err()
}

// DaemonStats holds live daemon metrics written periodically while running.
type DaemonStats struct {
	Heartbeat        time.Time        `json:"heartbeat"`
	StartedAt        time.Time        `json:"started_at"`
	PID              int              `json:"pid"`
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
}

// WriteDaemonStats upserts the daemon's live stats (single row).
func (d *DB) WriteDaemonStats(stats *DaemonStats) error {
	data, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshaling daemon stats: %w", err)
	}
	_, err = d.db.Exec(`
		INSERT OR REPLACE INTO daemon_stats (id, stats_json, updated_at)
		VALUES (1, ?, ?)`,
		string(data), time.Now(),
	)
	return err
}

// ReadDaemonStats reads the daemon's live stats. Returns nil if never written.
func (d *DB) ReadDaemonStats() (*DaemonStats, error) {
	var data string
	err := d.db.QueryRow("SELECT stats_json FROM daemon_stats WHERE id = 1").Scan(&data)
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

// ClearDaemonStats removes daemon stats (called on clean shutdown).
func (d *DB) ClearDaemonStats() error {
	_, err := d.db.Exec("DELETE FROM daemon_stats")
	return err
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// PRWatch represents a monitored toad PR.
type PRWatch struct {
	PRNumber      int
	PRURL         string
	Branch        string
	RunID         string
	SlackChannel  string
	SlackThread   string
	LastCommentID int
	FixCount      int
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
		&run.Branch, &run.WorktreePath, &run.Task, &run.StartedAt, &resultJSON,
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
			&run.Branch, &run.WorktreePath, &run.Task, &run.StartedAt, &resultJSON,
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
