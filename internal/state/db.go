package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// dbRetry retries fn up to 3 times on SQLITE_BUSY errors with exponential backoff.
func dbRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		err = fn()
		if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "SQLITE_BUSY") {
			return err
		}
		time.Sleep(time.Duration(100<<attempt) * time.Millisecond) // 100ms, 200ms, 400ms
	}
	return err
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

	// Wait up to 10s on write contention instead of failing immediately with SQLITE_BUSY
	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
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
			claim_scope   TEXT DEFAULT '',
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
			channel_id    TEXT DEFAULT '',
			thread_ts     TEXT DEFAULT '',
			message       TEXT,
			keywords      TEXT,
			dry_run       BOOLEAN NOT NULL DEFAULT TRUE,
			dismissed     BOOLEAN NOT NULL DEFAULT FALSE,
			reasoning     TEXT NOT NULL DEFAULT '',
			investigating BOOLEAN NOT NULL DEFAULT FALSE,
			created_at    DATETIME NOT NULL
		);

		CREATE TABLE IF NOT EXISTS ticket_index (
			external_key      TEXT PRIMARY KEY,
			issue_id          TEXT NOT NULL,
			issue_url         TEXT,
			source            TEXT DEFAULT '',
			investigation_id  TEXT DEFAULT '',
			created_at        DATETIME NOT NULL,
			last_seen_at      DATETIME NOT NULL,
			last_status       TEXT DEFAULT '',
			last_state_type   TEXT DEFAULT '',
			status_checked_at DATETIME
		);

		CREATE TABLE IF NOT EXISTS investigations (
			id            TEXT PRIMARY KEY,
			thread_ts     TEXT,
			channel       TEXT,
			repo          TEXT,
			findings_json TEXT NOT NULL,
			duration_ms   INTEGER DEFAULT 0,
			created_at    DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_invest_thread ON investigations(thread_ts);

		CREATE TABLE IF NOT EXISTS metrics_hourly (
			bucket TEXT NOT NULL,
			name   TEXT NOT NULL,
			count  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (bucket, name)
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
		last_used_at DATETIME,
		expires_at DATETIME
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

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS personality_adjustments (
	id INTEGER PRIMARY KEY,
	trait TEXT NOT NULL,
	delta REAL NOT NULL,
	source TEXT NOT NULL,
	trigger_detail TEXT,
	reasoning TEXT,
	before_value REAL NOT NULL,
	after_value REAL NOT NULL,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)`)
	if err != nil {
		return fmt.Errorf("creating personality_adjustments table: %w", err)
	}

	// Numbered schema migrations. Each step runs only if the current
	// schema_version is less than the migration's version number.
	type migration struct {
		version int
		sql     string
	}
	migrations := []migration{
		{1, `ALTER TABLE digest_opportunities ADD COLUMN dismissed BOOLEAN NOT NULL DEFAULT FALSE;
		     ALTER TABLE digest_opportunities ADD COLUMN reasoning TEXT NOT NULL DEFAULT ''`},
		{2, `ALTER TABLE pr_watches ADD COLUMN ci_fix_count INTEGER DEFAULT 0`},
		{3, `ALTER TABLE pr_watches ADD COLUMN ci_exhausted_notified BOOLEAN DEFAULT FALSE`},
		{4, `ALTER TABLE pr_watches ADD COLUMN conflict_fix_count INTEGER DEFAULT 0`},
		{5, `ALTER TABLE digest_opportunities ADD COLUMN investigating BOOLEAN NOT NULL DEFAULT FALSE`},
		{6, `ALTER TABLE digest_opportunities ADD COLUMN channel_id TEXT DEFAULT '';
		     ALTER TABLE digest_opportunities ADD COLUMN thread_ts TEXT DEFAULT ''`},
		{7, `ALTER TABLE pr_watches ADD COLUMN final_state TEXT DEFAULT ''`},
		{8, `ALTER TABLE pr_watches ADD COLUMN original_summary TEXT DEFAULT '';
		     ALTER TABLE pr_watches ADD COLUMN original_description TEXT DEFAULT ''`},
		{9, `ALTER TABLE runs ADD COLUMN claim_scope TEXT DEFAULT ''`},
		{10, `CREATE TABLE IF NOT EXISTS ticket_index (
			  external_key TEXT PRIMARY KEY, issue_id TEXT NOT NULL, issue_url TEXT,
			  source TEXT DEFAULT '', investigation_id TEXT DEFAULT '', created_at DATETIME NOT NULL, last_seen_at DATETIME NOT NULL,
			  last_status TEXT DEFAULT '', status_checked_at DATETIME);
			  CREATE TABLE IF NOT EXISTS investigations (
			  id TEXT PRIMARY KEY, thread_ts TEXT, channel TEXT, repo TEXT,
			  findings_json TEXT NOT NULL, created_at DATETIME NOT NULL);
			  CREATE INDEX IF NOT EXISTS idx_invest_thread ON investigations(thread_ts)`},
		// v11: MCP token hardening + Linear state-type outcome classification.
		//
		// mcp_tokens previously stored bearer tokens in plaintext (token TEXT
		// PRIMARY KEY held the raw "toad_<hex>" value). Existing rows can't be
		// hashed retroactively in a way that stays compatible with the plaintext
		// bearer a client already holds, and rotating is cheap (`/toad-mcp
		// connect` reissues a fresh token in one command), so this migration
		// forces rotation by wiping the table outright rather than attempting an
		// in-place rehash. From this point forward SaveMCPToken/ValidateMCPToken
		// store and look up the SHA-256 hex digest of the token, never the
		// plaintext. expires_at supports the new token_ttl_days config knob;
		// NULL means "no expiry" for tokens issued before this existed.
		//
		// ticket_index.last_state_type stores the Linear workflow state TYPE
		// (triage/backlog/unstarted/started/completed/canceled) alongside the
		// existing last_status name, so outcome classification can key off the
		// stable type instead of guessing from custom workflow state names.
		{11, `DELETE FROM mcp_tokens;
			  ALTER TABLE mcp_tokens ADD COLUMN expires_at DATETIME;
			  ALTER TABLE ticket_index ADD COLUMN last_state_type TEXT DEFAULT ''`},
		// v12: hourly metrics counters (for dashboard trend sparklines) plus
		// investigation duration tracking. metrics_hourly is a brand-new
		// table so CREATE TABLE IF NOT EXISTS is safe to replay here even
		// though the base schema block above also creates it for fresh DBs
		// (same dual-path convention as migration 10's ticket_index/
		// investigations tables). investigations.duration_ms is likewise
		// mirrored into the base CREATE TABLE above for fresh installs.
		{12, `CREATE TABLE IF NOT EXISTS metrics_hourly (
				  bucket TEXT NOT NULL, name TEXT NOT NULL, count INTEGER NOT NULL DEFAULT 0,
				  PRIMARY KEY (bucket, name));
			  ALTER TABLE investigations ADD COLUMN duration_ms INTEGER DEFAULT 0`},
	}

	// Read current schema version. If no version is stored, detect whether
	// this is a pre-versioned database (columns already added by the old
	// ad-hoc ALTER TABLE code) or a truly fresh database.
	var currentVersion int
	var versionStr string
	err = db.QueryRow(`SELECT value FROM settings WHERE key = 'schema_version'`).Scan(&versionStr)
	if err == nil {
		_, _ = fmt.Sscanf(versionStr, "%d", &currentVersion)
	} else {
		// No schema_version row. Check if a column from the last migration
		// exists — if so, the old ad-hoc migrations already ran everything.
		var colExists int
		_ = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pr_watches') WHERE name = 'original_summary'`).Scan(&colExists)
		if colExists > 0 {
			currentVersion = len(migrations)
		}
	}

	for _, m := range migrations {
		if currentVersion >= m.version {
			continue
		}
		for _, stmt := range strings.Split(m.sql, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				slog.Warn("migration: failed to run statement", "version", m.version, "error", err)
			}
		}
		currentVersion = m.version
	}

	// Persist the schema version so future runs skip completed migrations.
	if _, err := db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES ('schema_version', ?)`,
		fmt.Sprintf("%d", currentVersion)); err != nil {
		return fmt.Errorf("updating schema_version: %w", err)
	}

	return nil
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
	cutoff := time.Now().Add(-olderThan)
	var n int64
	err := dbRetry(func() error {
		ctx, cancel := dbCtx()
		defer cancel()
		result, err := d.db.ExecContext(ctx, "DELETE FROM thread_memory WHERE created_at < ?", cutoff)
		if err != nil {
			return err
		}
		n, _ = result.RowsAffected()
		return nil
	})
	return int(n), err
}

// ThreadMemoryCount returns the number of thread memory rows currently
// stored — a genuinely-live count (unlike the old runs-derived Stats),
// surfaced on the dashboard as a lightweight "active context" signal.
func (d *DB) ThreadMemoryCount() (int, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	var count int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM thread_memory").Scan(&count); err != nil {
		return 0, fmt.Errorf("counting threads: %w", err)
	}
	return count, nil
}

// metricBucketFormat is the UTC-hour bucket key layout used by
// metrics_hourly, e.g. "2026-08-02T14".
const metricBucketFormat = "2006-01-02T15"

// IncrementMetric bumps the counter for name in the UTC-hour bucket
// containing t (upsert +1). Used for lightweight event counters (e.g.
// intake events, questions answered) that back the dashboard's trend
// sparklines; an empty/missing table degrades gracefully since
// MetricSeries and MetricSeriesDaily just zero-fill on query errors.
func (d *DB) IncrementMetric(name string, t time.Time) error {
	bucket := t.UTC().Format(metricBucketFormat)
	return dbRetry(func() error {
		ctx, cancel := dbCtx()
		defer cancel()
		_, err := d.db.ExecContext(ctx, `
			INSERT INTO metrics_hourly (bucket, name, count) VALUES (?, ?, 1)
			ON CONFLICT(bucket, name) DO UPDATE SET count = count + 1`,
			bucket, name,
		)
		return err
	})
}

// MetricSeries returns the last `buckets` hourly counts for name, oldest
// first, ending in the UTC hour containing now. Hours with no recorded
// events are zero-filled, so the result is always exactly `buckets` long.
// A query error (e.g. table not yet migrated) degrades to an all-zero
// series rather than failing the caller.
func (d *DB) MetricSeries(name string, buckets int, now time.Time) []int {
	series := make([]int, buckets)
	if buckets <= 0 {
		return series
	}

	counts := make(map[string]int, buckets)
	ctx, cancel := dbCtx()
	defer cancel()

	oldest := now.UTC().Add(-time.Duration(buckets-1) * time.Hour).Format(metricBucketFormat)
	rows, err := d.db.QueryContext(ctx,
		"SELECT bucket, count FROM metrics_hourly WHERE name = ? AND bucket >= ?",
		name, oldest,
	)
	if err != nil {
		return series
	}
	defer rows.Close()
	for rows.Next() {
		var bucket string
		var count int
		if err := rows.Scan(&bucket, &count); err != nil {
			continue
		}
		counts[bucket] = count
	}

	for i := 0; i < buckets; i++ {
		hour := now.UTC().Add(-time.Duration(buckets-1-i) * time.Hour)
		series[i] = counts[hour.Format(metricBucketFormat)]
	}
	return series
}

// MetricSeriesDaily returns the last `days` daily counts for name, oldest
// first, summing each day's hourly buckets. Zero-filled and error-tolerant
// the same way as MetricSeries.
func (d *DB) MetricSeriesDaily(name string, days int, now time.Time) []int {
	series := make([]int, days)
	if days <= 0 {
		return series
	}

	counts := make(map[string]int, days)
	ctx, cancel := dbCtx()
	defer cancel()

	oldestDay := now.UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := d.db.QueryContext(ctx,
		"SELECT substr(bucket, 1, 10) AS day, SUM(count) FROM metrics_hourly WHERE name = ? AND bucket >= ? GROUP BY day",
		name, oldestDay,
	)
	if err != nil {
		return series
	}
	defer rows.Close()
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			continue
		}
		counts[day] = count
	}

	for i := 0; i < days; i++ {
		day := now.UTC().AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		series[i] = counts[day]
	}
	return series
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
	Heartbeat         time.Time        `json:"heartbeat"`
	StartedAt         time.Time        `json:"started_at"`
	PID               int              `json:"pid"`
	Version           string           `json:"version,omitempty"`
	Draining          bool             `json:"draining,omitempty"`
	Ribbits           int64            `json:"ribbits"`
	Triages           int64            `json:"triages"`
	TriageByCategory  map[string]int64 `json:"triage_by_category"`
	DigestEnabled     bool             `json:"digest_enabled"`
	DigestDryRun      bool             `json:"digest_dry_run"`
	DigestCommentMode bool             `json:"digest_comment_mode,omitempty"`
	DigestBuffer      int              `json:"digest_buffer"`
	DigestNextFlush   time.Time        `json:"digest_next_flush"`
	DigestProcessed   int64            `json:"digest_processed"`
	DigestOpps        int64            `json:"digest_opportunities"`
	DigestSpawns      int64            `json:"digest_spawns"`
	IssueTracker      bool             `json:"issue_tracker,omitempty"`
	IssueProvider     string           `json:"issue_provider,omitempty"`
	MCPEnabled        bool             `json:"mcp_enabled,omitempty"`
	MCPHost           string           `json:"mcp_host,omitempty"`
	MCPPort           int              `json:"mcp_port,omitempty"`

	// Concurrency gauges for the dashboard's "Live now" card. Populated every
	// 10s (root.go's stats ticker) from the live occupancy/capacity of the
	// ribbit/investigate semaphore channels.
	InvestigateSlots    int `json:"investigate_slots,omitempty"`
	InvestigateInFlight int `json:"investigate_in_flight,omitempty"`
	RibbitSlots         int `json:"ribbit_slots,omitempty"`
	RibbitInFlight      int `json:"ribbit_in_flight,omitempty"`

	// RepoSync holds the last known sync outcome per configured repo name,
	// backing the dashboard's per-repo sync freshness display and its
	// sync-warning attention-strip alert. Updated by every repo sync attempt
	// (the periodic background sync and the on-demand pre-investigation sync
	// alike — see cmd's repoSyncTracker) and snapshotted here alongside the
	// other gauges. Absent until a sync has been attempted at least once.
	RepoSync map[string]RepoSyncStatus `json:"repo_sync,omitempty"`
}

// RepoSyncStatus is the last known outcome of syncing one configured repo's
// local checkout. LastSyncAt is the most recent *successful* sync; LastError
// is the most recent failure's message and is cleared on the next success.
// Both may be zero/empty if no sync has been attempted yet.
type RepoSyncStatus struct {
	LastSyncAt time.Time `json:"last_sync_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
}

// WriteDaemonStats upserts the daemon's live stats (single row).
func (d *DB) WriteDaemonStats(stats *DaemonStats) error {
	data, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshaling daemon stats: %w", err)
	}
	return dbRetry(func() error {
		ctx, cancel := dbCtx()
		defer cancel()
		_, err := d.db.ExecContext(ctx, `
			INSERT OR REPLACE INTO daemon_stats (id, stats_json, updated_at)
			VALUES (1, ?, ?)`,
			string(data), time.Now(),
		)
		return err
	})
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

// hashMCPToken returns the SHA-256 hex digest of a bearer token. Tokens are
// stored and looked up by this digest, never in plaintext — see the v11
// migration comment for why existing plaintext rows can't be rehashed
// in place.
func hashMCPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SaveMCPToken inserts or replaces an MCP token. tok.Token must be the
// plaintext bearer token; it is hashed before storage.
func (d *DB) SaveMCPToken(tok *MCPToken) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO mcp_tokens (token, slack_user_id, slack_user, role, created_at, last_used_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hashMCPToken(tok.Token), tok.SlackUserID, tok.SlackUser, tok.Role, tok.CreatedAt, tok.LastUsedAt, nullableTime(tok.ExpiresAt),
	)
	return err
}

// ValidateMCPToken looks up a token (by its hash) and updates its
// last_used_at timestamp. Returns nil, nil if the token is not found or has
// expired. A NULL expires_at (tokens issued before expiry existed, or issued
// with token_ttl_days=0) is treated as "never expires".
func (d *DB) ValidateMCPToken(token string) (*MCPToken, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	hash := hashMCPToken(token)

	var tok MCPToken
	var lastUsed sql.NullTime
	var expiresAt sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT token, slack_user_id, slack_user, role, created_at, last_used_at, expires_at FROM mcp_tokens WHERE token = ?",
		hash,
	).Scan(&tok.Token, &tok.SlackUserID, &tok.SlackUser, &tok.Role, &tok.CreatedAt, &lastUsed, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		tok.LastUsedAt = lastUsed.Time
	}
	if expiresAt.Valid {
		tok.ExpiresAt = expiresAt.Time
		if time.Now().After(tok.ExpiresAt) {
			return nil, nil
		}
	}

	// Update last_used_at
	now := time.Now()
	_, _ = d.db.ExecContext(ctx, "UPDATE mcp_tokens SET last_used_at = ? WHERE token = ?", now, hash)
	tok.LastUsedAt = now

	return &tok, nil
}

// GetMCPTokenByUser looks up a token by Slack user ID (without updating last_used_at).
func (d *DB) GetMCPTokenByUser(slackUserID string) (*MCPToken, error) {
	ctx, cancel := dbCtx()
	defer cancel()

	var tok MCPToken
	var lastUsed sql.NullTime
	var expiresAt sql.NullTime
	err := d.db.QueryRowContext(ctx,
		"SELECT token, slack_user_id, slack_user, role, created_at, last_used_at, expires_at FROM mcp_tokens WHERE slack_user_id = ?",
		slackUserID,
	).Scan(&tok.Token, &tok.SlackUserID, &tok.SlackUser, &tok.Role, &tok.CreatedAt, &lastUsed, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastUsed.Valid {
		tok.LastUsedAt = lastUsed.Time
	}
	if expiresAt.Valid {
		tok.ExpiresAt = expiresAt.Time
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

// TicketIndexEntry maps an external event key (e.g. a Sentry issue or a Slack
// thread) to the tracking issue filed for it, so toad doesn't re-file
// duplicate tickets for the same underlying problem.
type TicketIndexEntry struct {
	ExternalKey     string // "sentry:BILLING-2291" | "thread:C123:1722500000.000100"
	IssueID         string // "SCL-1482"
	IssueURL        string
	Source          string // "auto" | "cta" | "digest" | "escalation"
	InvestigationID string
	CreatedAt       time.Time
	LastSeenAt      time.Time
	LastStatus      string // raw status name, e.g. "In Progress", "Done"
	LastStateType   string // Linear workflow state type: triage/backlog/unstarted/started/completed/canceled
	StatusCheckedAt time.Time
}

// nullableTime converts a zero time.Time into a nil driver value so it is
// stored as SQL NULL rather than the zero-value timestamp.
func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}

// UpsertTicketIndex inserts a ticket index entry, or on conflict (same
// external_key) refreshes the identity fields (issue_id, issue_url, source)
// and bumps last_seen_at. last_status, status_checked_at, and
// investigation_id are preserved across re-observation: the natural calling
// pattern is to build a fresh TicketIndexEntry per incoming event and call
// Upsert just to record that the ticket was seen again, so those fields
// will typically be the zero value on the entry passed in. Status is meant
// to be managed independently via UpdateTicketStatus, and the investigation
// link (once set) must not be silently severed by a later, unrelated
// re-observation. Passing a non-zero value for either field on a
// subsequent call still updates it — only zero values are treated as
// "unspecified, leave alone".
func (d *DB) UpsertTicketIndex(e *TicketIndexEntry) error {
	return dbRetry(func() error {
		ctx, cancel := dbCtx()
		defer cancel()
		_, err := d.db.ExecContext(ctx, `
			INSERT INTO ticket_index (external_key, issue_id, issue_url, source, investigation_id, created_at, last_seen_at, last_status, last_state_type, status_checked_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(external_key) DO UPDATE SET
				issue_id = excluded.issue_id,
				issue_url = excluded.issue_url,
				source = excluded.source,
				last_seen_at = excluded.last_seen_at,
				investigation_id = COALESCE(NULLIF(excluded.investigation_id, ''), ticket_index.investigation_id),
				last_status = COALESCE(NULLIF(excluded.last_status, ''), ticket_index.last_status),
				last_state_type = COALESCE(NULLIF(excluded.last_state_type, ''), ticket_index.last_state_type),
				status_checked_at = COALESCE(excluded.status_checked_at, ticket_index.status_checked_at)`,
			e.ExternalKey, e.IssueID, e.IssueURL, e.Source, e.InvestigationID,
			e.CreatedAt, e.LastSeenAt, e.LastStatus, e.LastStateType, nullableTime(e.StatusCheckedAt),
		)
		return err
	})
}

// GetTicketIndex looks up a ticket index entry by its external key.
// Returns nil, nil if no entry exists.
func (d *DB) GetTicketIndex(externalKey string) (*TicketIndexEntry, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	row := d.db.QueryRowContext(ctx,
		`SELECT external_key, issue_id, COALESCE(issue_url,''), COALESCE(source,''), COALESCE(investigation_id,''),
		        created_at, last_seen_at, COALESCE(last_status,''), COALESCE(last_state_type,''), status_checked_at
		 FROM ticket_index WHERE external_key = ?`,
		externalKey,
	)
	return scanTicketIndex(row)
}

// RecentTicketIndex returns the most recently seen ticket index entries, newest first.
func (d *DB) RecentTicketIndex(limit int) ([]*TicketIndexEntry, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		`SELECT external_key, issue_id, COALESCE(issue_url,''), COALESCE(source,''), COALESCE(investigation_id,''),
		        created_at, last_seen_at, COALESCE(last_status,''), COALESCE(last_state_type,''), status_checked_at
		 FROM ticket_index ORDER BY last_seen_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*TicketIndexEntry
	for rows.Next() {
		e, err := scanTicketIndexRow(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// UpdateTicketStatus updates the last known status (and Linear state type,
// when known) of a tracked ticket.
func (d *DB) UpdateTicketStatus(externalKey, status, stateType string) error {
	ctx, cancel := dbCtx()
	defer cancel()
	_, err := d.db.ExecContext(ctx,
		"UPDATE ticket_index SET last_status = ?, last_state_type = ?, status_checked_at = ? WHERE external_key = ?",
		status, stateType, time.Now(), externalKey,
	)
	return err
}

// scanner is the subset of *sql.Row / *sql.Rows needed for scanning.
type scanner interface {
	Scan(dest ...interface{}) error
}

func scanTicketIndex(row *sql.Row) (*TicketIndexEntry, error) {
	e, err := scanTicketIndexRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func scanTicketIndexRow(row scanner) (*TicketIndexEntry, error) {
	var e TicketIndexEntry
	var statusCheckedAt sql.NullTime
	err := row.Scan(
		&e.ExternalKey, &e.IssueID, &e.IssueURL, &e.Source, &e.InvestigationID,
		&e.CreatedAt, &e.LastSeenAt, &e.LastStatus, &e.LastStateType, &statusCheckedAt,
	)
	if err != nil {
		return nil, err
	}
	if statusCheckedAt.Valid {
		e.StatusCheckedAt = statusCheckedAt.Time
	}
	return &e, nil
}

// InvestigationRecord holds the findings from an investigation into a bug
// report, keyed by ID and linkable to the Slack thread and tracking ticket
// it originated from.
type InvestigationRecord struct {
	ID           string
	ThreadTS     string
	Channel      string
	Repo         string
	FindingsJSON string
	DurationMs   int64
	CreatedAt    time.Time
}

// SaveInvestigation inserts or replaces an investigation record.
func (d *DB) SaveInvestigation(rec *InvestigationRecord) error {
	return dbRetry(func() error {
		ctx, cancel := dbCtx()
		defer cancel()
		_, err := d.db.ExecContext(ctx, `
			INSERT OR REPLACE INTO investigations (id, thread_ts, channel, repo, findings_json, duration_ms, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.ThreadTS, rec.Channel, rec.Repo, rec.FindingsJSON, rec.DurationMs, rec.CreatedAt,
		)
		return err
	})
}

// GetInvestigationByThread returns the newest investigation for a Slack
// thread. Returns nil, nil if none exists.
func (d *DB) GetInvestigationByThread(threadTS string) (*InvestigationRecord, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	row := d.db.QueryRowContext(ctx,
		"SELECT id, thread_ts, channel, repo, findings_json, COALESCE(duration_ms,0), created_at FROM investigations WHERE thread_ts = ? ORDER BY created_at DESC LIMIT 1",
		threadTS,
	)
	return scanInvestigation(row)
}

// FindInvestigationByTicket resolves the investigation behind a tracking
// ticket by joining through ticket_index.investigation_id. Returns nil, nil
// if no ticket_index row references an investigation for this issue.
func (d *DB) FindInvestigationByTicket(issueID string) (*InvestigationRecord, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	row := d.db.QueryRowContext(ctx, `
		SELECT i.id, i.thread_ts, i.channel, i.repo, i.findings_json, COALESCE(i.duration_ms,0), i.created_at
		FROM investigations i
		JOIN ticket_index t ON t.investigation_id = i.id
		WHERE t.issue_id = ?
		ORDER BY t.last_seen_at DESC LIMIT 1`,
		issueID,
	)
	return scanInvestigation(row)
}

// RecentInvestigations returns the most recently created investigation
// records, newest first, for the dashboard's Investigations tab.
func (d *DB) RecentInvestigations(limit int) ([]*InvestigationRecord, error) {
	ctx, cancel := dbCtx()
	defer cancel()
	rows, err := d.db.QueryContext(ctx,
		"SELECT id, thread_ts, channel, repo, findings_json, COALESCE(duration_ms,0), created_at FROM investigations ORDER BY created_at DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recs []*InvestigationRecord
	for rows.Next() {
		var rec InvestigationRecord
		if err := rows.Scan(&rec.ID, &rec.ThreadTS, &rec.Channel, &rec.Repo, &rec.FindingsJSON, &rec.DurationMs, &rec.CreatedAt); err != nil {
			return nil, err
		}
		recs = append(recs, &rec)
	}
	return recs, rows.Err()
}

// TicketForInvestigation resolves the tracking ticket filed for an
// investigation — the reverse join of FindInvestigationByTicket. Returns
// nil, nil if no ticket_index row references this investigation.
func (d *DB) TicketForInvestigation(investigationID string) (*TicketIndexEntry, error) {
	if investigationID == "" {
		return nil, nil
	}
	ctx, cancel := dbCtx()
	defer cancel()
	row := d.db.QueryRowContext(ctx, `
		SELECT external_key, issue_id, COALESCE(issue_url,''), COALESCE(source,''), COALESCE(investigation_id,''),
		       created_at, last_seen_at, COALESCE(last_status,''), COALESCE(last_state_type,''), status_checked_at
		FROM ticket_index WHERE investigation_id = ? ORDER BY last_seen_at DESC LIMIT 1`,
		investigationID,
	)
	return scanTicketIndex(row)
}

func scanInvestigation(row *sql.Row) (*InvestigationRecord, error) {
	var rec InvestigationRecord
	err := row.Scan(&rec.ID, &rec.ThreadTS, &rec.Channel, &rec.Repo, &rec.FindingsJSON, &rec.DurationMs, &rec.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// ExecContext exposes the underlying DB ExecContext for other packages.
func (d *DB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return d.db.ExecContext(ctx, query, args...)
}

// QueryContext exposes the underlying DB QueryContext for other packages.
func (d *DB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, query, args...)
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// MCPToken represents an MCP authentication token linked to a Slack user.
// Token holds the plaintext bearer value when constructed for issuance
// (SaveMCPToken hashes it before storage); when returned from a lookup it
// holds the stored hash, not the original plaintext.
type MCPToken struct {
	Token       string
	SlackUserID string
	SlackUser   string
	Role        string // "dev" or "user"
	CreatedAt   time.Time
	LastUsedAt  time.Time
	ExpiresAt   time.Time // zero = no expiry
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
