package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SpendLimit defines a rolling time window with a USD cap.
type SpendLimit struct {
	Window   time.Duration `json:"window"`
	LimitUSD float64       `json:"limit_usd"`
	Label    string        `json:"label"` // human-readable, e.g. "5h", "7d"
}

// DefaultSpendLimits are enforced for all orgs unless overridden.
var DefaultSpendLimits = []SpendLimit{
	{Window: 5 * time.Hour, LimitUSD: 25, Label: "5h"},
	{Window: 7 * 24 * time.Hour, LimitUSD: 100, Label: "7d"},
}

// WindowSpend holds the spend for a single rolling window.
type WindowSpend struct {
	Label    string  `json:"label"`
	WindowMs int64   `json:"window_ms"`
	LimitUSD float64 `json:"limit_usd"`
	SpentUSD float64 `json:"spent_usd"`
	Exceeded bool    `json:"exceeded"`
}

// UsageStore manages per-org SQLite databases for usage tracking.
// Each org gets its own database file at {baseDir}/{orgId}/usage.db.
type UsageStore struct {
	baseDir string

	mu          sync.Mutex
	conns       map[string]*sql.DB // orgId -> *sql.DB
	analytics   *sql.DB
	analyticsMu sync.RWMutex
}

const analyticsDirName = "_analytics"
const analyticsSchemaVersion = "1"
const analyticsMetaSchemaVersionKey = "schema_version"

// NewUsageStore creates a new per-org usage store rooted at baseDir.
func NewUsageStore(baseDir string) (*UsageStore, error) {
	if baseDir == "" {
		return nil, errors.New("usage store base dir is required")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create usage store base dir: %w", err)
	}
	store := &UsageStore{
		baseDir: baseDir,
		conns:   make(map[string]*sql.DB),
	}
	if err := store.openAnalyticsDB(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.ensureAnalyticsIndexCurrent(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// Close closes all open per-org database connections.
func (u *UsageStore) Close() error {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	var firstErr error
	for orgID, db := range u.conns {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(u.conns, orgID)
	}
	if u.analytics != nil {
		if err := u.analytics.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		u.analytics = nil
	}
	return firstErr
}

func (u *UsageStore) openAnalyticsDB() error {
	analyticsDir := filepath.Join(u.baseDir, analyticsDirName)
	if err := os.MkdirAll(analyticsDir, 0o755); err != nil {
		return fmt.Errorf("create analytics usage dir: %w", err)
	}

	dbPath := filepath.Join(analyticsDir, "usage_analytics.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open usage analytics db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(2 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return fmt.Errorf("ping usage analytics db: %w", err)
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS org_usage_rollups (
			org_id TEXT PRIMARY KEY,
			total_cost_usd REAL NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0,
			updated_at_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS usage_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			org_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			thread_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_org_created_at ON usage_events(org_id, created_at_ms DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events(created_at_ms DESC)`,
		`CREATE TABLE IF NOT EXISTS org_effective_limits (
			org_id TEXT NOT NULL,
			window_ms INTEGER NOT NULL,
			limit_usd REAL NOT NULL,
			label TEXT NOT NULL,
			updated_at_ms INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (org_id, label)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_org_effective_limits_org_id ON org_effective_limits(org_id)`,
		`CREATE TABLE IF NOT EXISTS analytics_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}

	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return fmt.Errorf("init usage analytics schema: %w", err)
		}
	}

	u.analytics = db
	return nil
}

func (u *UsageStore) ensureAnalyticsIndexCurrent() error {
	if u == nil || u.analytics == nil {
		return nil
	}

	needsRebuild, err := u.analyticsNeedsRebuild()
	if err != nil {
		return err
	}
	if !needsRebuild {
		return nil
	}
	return u.rebuildAnalyticsIndex()
}

func (u *UsageStore) analyticsNeedsRebuild() (bool, error) {
	if u == nil || u.analytics == nil {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var schemaVersion string
	u.analyticsMu.RLock()
	err := u.analytics.QueryRowContext(
		ctx,
		`SELECT value FROM analytics_meta WHERE key = ?`,
		analyticsMetaSchemaVersionKey,
	).Scan(&schemaVersion)
	u.analyticsMu.RUnlock()
	if err == nil {
		return schemaVersion != analyticsSchemaVersion, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("read analytics schema version: %w", err)
	}

	orgIDs, err := u.listOrgIDsOnDisk()
	if err != nil {
		return false, err
	}
	if len(orgIDs) == 0 {
		if err := u.markAnalyticsSchemaCurrent(); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

func (u *UsageStore) markAnalyticsSchemaCurrent() error {
	if u == nil || u.analytics == nil {
		return nil
	}

	u.analyticsMu.Lock()
	defer u.analyticsMu.Unlock()

	return u.markAnalyticsSchemaCurrentLocked()
}

func (u *UsageStore) markAnalyticsSchemaCurrentLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	return writeAnalyticsMetaValueTx(ctx, u.analytics, analyticsMetaSchemaVersionKey, analyticsSchemaVersion)
}

func writeAnalyticsMetaValueTx(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, key string, value string) error {
	_, err := exec.ExecContext(
		ctx,
		`INSERT INTO analytics_meta (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key,
		value,
	)
	if err != nil {
		return fmt.Errorf("write analytics meta value for %s: %w", key, err)
	}
	return nil
}

func logAnalyticsWarning(operation string, err error) {
	if err == nil {
		return
	}
	log.Printf("[UsageStore] analytics sync warning during %s: %v", operation, err)
}

// ensureAnalyticsDefaultLimitsTx seeds default spend-limit rows for an org
// only when no rows exist yet. This avoids clobbering custom override labels
// (e.g. "1h"/"24h") with the default "5h"/"7d" rows.
func ensureAnalyticsDefaultLimitsTx(
	ctx context.Context,
	tx *sql.Tx,
	orgID string,
	updatedAtMs int64,
) error {
	var existingCount int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM org_effective_limits WHERE org_id = ?`,
		orgID,
	).Scan(&existingCount); err != nil {
		return fmt.Errorf("check existing analytics limits: %w", err)
	}
	if existingCount > 0 {
		return nil
	}

	for _, limit := range DefaultSpendLimits {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO org_effective_limits (org_id, window_ms, limit_usd, label, updated_at_ms)
			 VALUES (?, ?, ?, ?, ?)`,
			orgID,
			limit.Window.Milliseconds(),
			limit.LimitUSD,
			limit.Label,
			updatedAtMs,
		); err != nil {
			return fmt.Errorf("ensure default analytics limit: %w", err)
		}
	}
	return nil
}

func openSQLiteDB(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(2 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// getDB returns or creates the SQLite connection for an org.
func (u *UsageStore) getDB(orgID string) (*sql.DB, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if db, ok := u.conns[orgID]; ok {
		return db, nil
	}

	orgDir := filepath.Join(u.baseDir, orgID)
	if err := os.MkdirAll(orgDir, 0o755); err != nil {
		return nil, fmt.Errorf("create org usage dir: %w", err)
	}

	dbPath := filepath.Join(orgDir, "usage.db")
	db, err := openSQLiteDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open org usage db: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping org usage db: %w", err)
	}

	if err := u.initOrgSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	u.conns[orgID] = db
	return db, nil
}

func (u *UsageStore) initOrgSchema(ctx context.Context, db *sql.DB) error {
	statements := []string{
		// Per-request usage log.
		`CREATE TABLE IF NOT EXISTS usage_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			thread_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL,
			provider TEXT NOT NULL DEFAULT '',
			billing_source TEXT NOT NULL DEFAULT 'hosted',
			credit_chargeable INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
			cost_usd REAL NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_log_created_at ON usage_log(created_at_ms)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_log_workspace_id ON usage_log(workspace_id)`,

		// Org-level totals + optional per-org limit overrides.
		`CREATE TABLE IF NOT EXISTS spend (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			total_cost_usd REAL NOT NULL DEFAULT 0,
			total_input_tokens INTEGER NOT NULL DEFAULT 0,
			total_output_tokens INTEGER NOT NULL DEFAULT 0,
			total_cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			total_cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			total_requests INTEGER NOT NULL DEFAULT 0,
			limits_json TEXT NOT NULL DEFAULT '',
			updated_at_ms INTEGER NOT NULL DEFAULT 0
		)`,
	}

	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("init org usage schema: %w", err)
		}
	}

	if err := ensureUsageLogColumnExists(ctx, db, "billing_source", "TEXT NOT NULL DEFAULT 'hosted'"); err != nil {
		return err
	}
	if err := ensureUsageLogColumnExists(ctx, db, "credit_chargeable", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// Ensure the single spend row exists.
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO spend (id) VALUES (1)`)
	return err
}

func ensureUsageLogColumnExists(ctx context.Context, db *sql.DB, columnName string, columnDef string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(usage_log)`)
	if err != nil {
		return fmt.Errorf("inspect usage_log columns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			valueType string
			notNull   int
			defaultV  sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &valueType, &notNull, &defaultV, &pk); err != nil {
			return fmt.Errorf("scan usage_log column: %w", err)
		}
		if name == columnName {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage_log columns: %w", err)
	}

	if _, err := db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE usage_log ADD COLUMN %s %s`, columnName, columnDef)); err != nil {
		return fmt.Errorf("alter usage_log add %s: %w", columnName, err)
	}
	return nil
}

// UsageRecord represents a single AI Gateway request's token usage and cost.
type UsageRecord struct {
	OrgID                    string
	WorkspaceID              string
	UserID                   string
	ThreadID                 string
	Model                    string
	Provider                 string
	BillingSource            string
	CreditChargeable         bool
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	CostUSD                  float64
	DurationMs               int64
}

// RecordUsage inserts a usage log entry and atomically increments the spend totals.
func (u *UsageStore) RecordUsage(record UsageRecord) error {
	if u == nil {
		return nil
	}
	db, err := u.getDB(record.OrgID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	now := time.Now().UTC().UnixMilli()
	billingSource := strings.TrimSpace(record.BillingSource)
	if billingSource == "" {
		billingSource = "hosted"
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_log (
			workspace_id, user_id, thread_id, model, provider, billing_source, credit_chargeable,
			input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
			cost_usd, duration_ms, created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.WorkspaceID, record.UserID, record.ThreadID,
		record.Model, record.Provider, billingSource, boolToInt(record.CreditChargeable),
		record.InputTokens, record.OutputTokens,
		record.CacheCreationInputTokens, record.CacheReadInputTokens,
		record.CostUSD, record.DurationMs, now,
	); err != nil {
		return fmt.Errorf("insert usage_log: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE spend SET
			total_cost_usd = total_cost_usd + ?,
			total_input_tokens = total_input_tokens + ?,
			total_output_tokens = total_output_tokens + ?,
			total_cache_creation_tokens = total_cache_creation_tokens + ?,
			total_cache_read_tokens = total_cache_read_tokens + ?,
			total_requests = total_requests + 1,
			updated_at_ms = ?
		WHERE id = 1`,
		record.CostUSD,
		record.InputTokens, record.OutputTokens,
		record.CacheCreationInputTokens, record.CacheReadInputTokens,
		now,
	); err != nil {
		return fmt.Errorf("update spend: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	if err := u.recordAnalyticsUsage(record, now); err != nil {
		logAnalyticsWarning("record usage", err)
	}
	return nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// OrgSpend holds lifetime totals for an org.
type OrgSpend struct {
	TotalCostUSD  float64
	TotalRequests int64
}

// GetOrgSpend returns lifetime spend totals for an org.
func (u *UsageStore) GetOrgSpend(orgID string) (OrgSpend, error) {
	if u == nil {
		return OrgSpend{}, nil
	}
	db, err := u.getDB(orgID)
	if err != nil {
		return OrgSpend{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var spend OrgSpend
	err = db.QueryRowContext(ctx, `
		SELECT total_cost_usd, total_requests
		FROM spend WHERE id = 1`,
	).Scan(&spend.TotalCostUSD, &spend.TotalRequests)

	if errors.Is(err, sql.ErrNoRows) {
		return OrgSpend{}, nil
	}
	return spend, err
}

func loadSpendLimitsFromDB(ctx context.Context, db *sql.DB) ([]SpendLimit, error) {
	var limitsJSON string
	err := db.QueryRowContext(ctx, `SELECT limits_json FROM spend WHERE id = 1`).Scan(&limitsJSON)
	if err != nil || limitsJSON == "" {
		return DefaultSpendLimits, nil
	}

	var limits []SpendLimit
	if json.Unmarshal([]byte(limitsJSON), &limits) != nil || len(limits) == 0 {
		return DefaultSpendLimits, nil
	}
	return limits, nil
}

// GetSpendLimits returns the effective spend limits for an org.
// Returns per-org overrides if set, otherwise DefaultSpendLimits.
func (u *UsageStore) GetSpendLimits(orgID string) ([]SpendLimit, error) {
	if u == nil {
		return DefaultSpendLimits, nil
	}
	db, err := u.getDB(orgID)
	if err != nil {
		return DefaultSpendLimits, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	return loadSpendLimitsFromDB(ctx, db)
}

// SetSpendLimits sets per-org spend limit overrides. Pass nil to revert to defaults.
func (u *UsageStore) SetSpendLimits(orgID string, limits []SpendLimit) error {
	if u == nil {
		return nil
	}
	db, err := u.getDB(orgID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	now := time.Now().UTC().UnixMilli()

	limitsJSON := ""
	if len(limits) > 0 {
		data, err := json.Marshal(limits)
		if err != nil {
			return fmt.Errorf("marshal limits: %w", err)
		}
		limitsJSON = string(data)
	}

	_, err = db.ExecContext(ctx, `
		UPDATE spend SET limits_json = ?, updated_at_ms = ? WHERE id = 1`,
		limitsJSON, now,
	)
	if err != nil {
		return err
	}

	effective := DefaultSpendLimits
	if len(limits) > 0 {
		effective = limits
	}
	if err := u.replaceAnalyticsEffectiveLimits(orgID, effective, now); err != nil {
		logAnalyticsWarning("set spend limits", err)
	}
	return nil
}

// CheckSpendLimits checks all rolling time windows for an org.
// Returns the first exceeded window (if any) and the full window status.
func (u *UsageStore) CheckSpendLimits(orgID string) (exceeded *WindowSpend, windows []WindowSpend, err error) {
	if u == nil {
		return nil, nil, nil
	}

	limits, err := u.GetSpendLimits(orgID)
	if err != nil {
		return nil, nil, err
	}

	db, err := u.getDB(orgID)
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	now := time.Now().UTC().UnixMilli()
	windows = make([]WindowSpend, 0, len(limits))

	for _, limit := range limits {
		cutoff := now - limit.Window.Milliseconds()
		var spent float64
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(cost_usd), 0) FROM usage_log WHERE created_at_ms > ?`,
			cutoff,
		).Scan(&spent)
		if err != nil {
			return nil, nil, fmt.Errorf("query window spend (%s): %w", limit.Label, err)
		}

		ws := WindowSpend{
			Label:    limit.Label,
			WindowMs: limit.Window.Milliseconds(),
			LimitUSD: limit.LimitUSD,
			SpentUSD: spent,
			Exceeded: spent >= limit.LimitUSD,
		}
		windows = append(windows, ws)

		if ws.Exceeded && exceeded == nil {
			exceeded = &ws
		}
	}

	return exceeded, windows, nil
}

// UsageLogEntry represents a single row from the usage_log table.
type UsageLogEntry struct {
	ID                       int64   `json:"id"`
	WorkspaceID              string  `json:"workspace_id"`
	UserID                   string  `json:"user_id"`
	ThreadID                 string  `json:"thread_id"`
	Model                    string  `json:"model"`
	Provider                 string  `json:"provider"`
	BillingSource            string  `json:"billing_source"`
	CreditChargeable         int64   `json:"credit_chargeable"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
	CostUSD                  float64 `json:"cost_usd"`
	DurationMs               int64   `json:"duration_ms"`
	CreatedAtMs              int64   `json:"created_at_ms"`
}

// GetUsageLog returns the most recent usage log entries for an org.
func (u *UsageStore) GetUsageLog(orgID string, limit int) ([]UsageLogEntry, error) {
	if u == nil {
		return nil, nil
	}
	db, err := u.getDB(orgID)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT id, workspace_id, user_id, thread_id, model, provider,
		       billing_source, credit_chargeable,
		       input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		       cost_usd, duration_ms, created_at_ms
		FROM usage_log
		ORDER BY created_at_ms DESC
		LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query usage_log: %w", err)
	}
	defer rows.Close()

	var entries []UsageLogEntry
	for rows.Next() {
		var e UsageLogEntry
		if err := rows.Scan(
			&e.ID, &e.WorkspaceID, &e.UserID, &e.ThreadID,
			&e.Model, &e.Provider, &e.BillingSource, &e.CreditChargeable,
			&e.InputTokens, &e.OutputTokens,
			&e.CacheCreationInputTokens, &e.CacheReadInputTokens,
			&e.CostUSD, &e.DurationMs, &e.CreatedAtMs,
		); err != nil {
			return nil, fmt.Errorf("scan usage_log row: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// UsageLogQuery defines filters for paginated usage log queries.
type UsageLogQuery struct {
	Limit  int
	Cursor int64 // cursor is the id to paginate from (exclusive, descending)
	FromMs int64 // inclusive lower bound on created_at_ms (0 = no filter)
	ToMs   int64 // exclusive upper bound on created_at_ms (0 = no filter)
}

// UsageLogPage holds a page of usage log entries plus pagination metadata.
type UsageLogPage struct {
	Entries    []UsageLogEntry `json:"entries"`
	Count      int             `json:"count"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor"`
}

// GetUsageLogPaginated returns a paginated, optionally date-filtered page of usage log entries.
func (u *UsageStore) GetUsageLogPaginated(orgID string, q UsageLogQuery) (UsageLogPage, error) {
	if u == nil {
		return UsageLogPage{Entries: []UsageLogEntry{}}, nil
	}
	db, err := u.getDB(orgID)
	if err != nil {
		return UsageLogPage{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Build query dynamically based on filters.
	query := `SELECT id, workspace_id, user_id, thread_id, model, provider,
	       billing_source, credit_chargeable,
	       input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
	       cost_usd, duration_ms, created_at_ms
		FROM usage_log WHERE 1=1`
	var args []any

	if q.Cursor > 0 {
		query += ` AND id < ?`
		args = append(args, q.Cursor)
	}
	if q.FromMs > 0 {
		query += ` AND created_at_ms >= ?`
		args = append(args, q.FromMs)
	}
	if q.ToMs > 0 {
		query += ` AND created_at_ms < ?`
		args = append(args, q.ToMs)
	}

	// Fetch limit+1 to detect has_more.
	fetchLimit := q.Limit + 1
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, fetchLimit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return UsageLogPage{}, fmt.Errorf("query usage_log paginated: %w", err)
	}
	defer rows.Close()

	entries := make([]UsageLogEntry, 0, q.Limit)
	for rows.Next() {
		var e UsageLogEntry
		if err := rows.Scan(
			&e.ID, &e.WorkspaceID, &e.UserID, &e.ThreadID,
			&e.Model, &e.Provider, &e.BillingSource, &e.CreditChargeable,
			&e.InputTokens, &e.OutputTokens,
			&e.CacheCreationInputTokens, &e.CacheReadInputTokens,
			&e.CostUSD, &e.DurationMs, &e.CreatedAtMs,
		); err != nil {
			return UsageLogPage{}, fmt.Errorf("scan usage_log row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return UsageLogPage{}, err
	}

	hasMore := len(entries) > q.Limit
	if hasMore {
		entries = entries[:q.Limit]
	}

	var nextCursor string
	if hasMore && len(entries) > 0 {
		nextCursor = strconv.FormatInt(entries[len(entries)-1].ID, 10)
	}

	return UsageLogPage{
		Entries:    entries,
		Count:      len(entries),
		HasMore:    hasMore,
		NextCursor: nextCursor,
	}, nil
}

// UsageLogSum holds aggregated usage totals for a date range.
type UsageLogSum struct {
	TotalCostUSD                  float64 `json:"total_cost_usd"`
	TotalRequests                 int64   `json:"total_requests"`
	TotalInputTokens              int64   `json:"total_input_tokens"`
	TotalOutputTokens             int64   `json:"total_output_tokens"`
	TotalCacheCreationInputTokens int64   `json:"total_cache_creation_input_tokens"`
	TotalCacheReadInputTokens     int64   `json:"total_cache_read_input_tokens"`
}

// GetUsageLogSum returns aggregated totals for usage log entries in a date range.
func (u *UsageStore) GetUsageLogSum(orgID string, fromMs, toMs int64) (UsageLogSum, error) {
	return u.getUsageLogSum(orgID, fromMs, toMs, false)
}

func (u *UsageStore) GetCreditChargeableUsageLogSum(orgID string, fromMs, toMs int64) (UsageLogSum, error) {
	return u.getUsageLogSum(orgID, fromMs, toMs, true)
}

func (u *UsageStore) getUsageLogSum(orgID string, fromMs, toMs int64, creditChargeableOnly bool) (UsageLogSum, error) {
	if u == nil {
		return UsageLogSum{}, nil
	}
	db, err := u.getDB(orgID)
	if err != nil {
		return UsageLogSum{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var s UsageLogSum
	query := `
		SELECT COALESCE(SUM(cost_usd), 0),
		       COUNT(*),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation_input_tokens), 0),
		       COALESCE(SUM(cache_read_input_tokens), 0)
		FROM usage_log
		WHERE created_at_ms >= ? AND created_at_ms < ?`
	args := []any{fromMs, toMs}
	if creditChargeableOnly {
		query += ` AND credit_chargeable = 1`
	}
	err = db.QueryRowContext(ctx, query, args...).Scan(
		&s.TotalCostUSD, &s.TotalRequests,
		&s.TotalInputTokens, &s.TotalOutputTokens,
		&s.TotalCacheCreationInputTokens, &s.TotalCacheReadInputTokens,
	)
	if err != nil {
		return UsageLogSum{}, fmt.Errorf("query usage_log sum: %w", err)
	}
	return s, nil
}

type OrgUsageAnalyticsRow struct {
	OrgID         string        `json:"org_id"`
	TotalCostUSD  float64       `json:"total_cost_usd"`
	TotalRequests int64         `json:"total_requests"`
	Spend7d       float64       `json:"spend_7d"`
	Spend30d      float64       `json:"spend_30d"`
	Windows       []WindowSpend `json:"windows,omitempty"`
}

type DailySpendSummary struct {
	Date            string  `json:"date"`
	TotalSpendUSD   float64 `json:"total_spend_usd"`
	TotalRequests   int64   `json:"total_requests"`
	SpamSpendUSD    float64 `json:"spam_spend_usd"`
	NonSpamSpendUSD float64 `json:"non_spam_spend_usd"`
}

type DailySpendHourlyRow struct {
	Hour            int     `json:"hour"`
	SpendUSD        float64 `json:"spend_usd"`
	Requests        int64   `json:"requests"`
	SpamSpendUSD    float64 `json:"spam_spend_usd"`
	NonSpamSpendUSD float64 `json:"non_spam_spend_usd"`
}

type DailySpendModelRow struct {
	Model    string  `json:"model"`
	SpendUSD float64 `json:"spend_usd"`
	Requests int64   `json:"requests"`
}

type DailySpendOrgRow struct {
	OrgID    string  `json:"org_id"`
	SpendUSD float64 `json:"spend_usd"`
	Requests int64   `json:"requests"`
	IsSpam   bool    `json:"is_spam"`
}

type DailySpendAnalytics struct {
	Date              string                `json:"date"`
	IsPartial         bool                  `json:"is_partial"`
	TotalSpendUSD     float64               `json:"total_spend_usd"`
	TotalRequests     int64                 `json:"total_requests"`
	SpamSpendUSD      float64               `json:"spam_spend_usd"`
	NonSpamSpendUSD   float64               `json:"non_spam_spend_usd"`
	SpamOrgCount      int                   `json:"spam_org_count"`
	NonSpamOrgCount   int                   `json:"non_spam_org_count"`
	PreviousDay       DailySpendSummary     `json:"previous_day"`
	HourlySeries      []DailySpendHourlyRow `json:"hourly_series"`
	ModelBreakdown    []DailySpendModelRow  `json:"model_breakdown"`
	TopOrgs           []DailySpendOrgRow    `json:"top_orgs"`
	OtherOrgsSpendUSD float64               `json:"other_orgs_spend_usd"`
	OtherOrgsCount    int                   `json:"other_orgs_count"`
}

type DailySpendAnalyticsQuery struct {
	Date         string
	OrgIDs       []string
	TopOrgsLimit int
	Now          time.Time
}

func (u *UsageStore) recordAnalyticsUsage(record UsageRecord, createdAtMs int64) error {
	if u == nil || u.analytics == nil {
		return nil
	}

	u.analyticsMu.Lock()
	defer u.analyticsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tx, err := u.analytics.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin analytics tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events (
			org_id, workspace_id, user_id, thread_id, model, provider,
			input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
			cost_usd, duration_ms, created_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.OrgID, record.WorkspaceID, record.UserID, record.ThreadID,
		record.Model, record.Provider,
		record.InputTokens, record.OutputTokens,
		record.CacheCreationInputTokens, record.CacheReadInputTokens,
		record.CostUSD, record.DurationMs, createdAtMs,
	); err != nil {
		return fmt.Errorf("insert analytics usage event: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO org_usage_rollups (org_id, total_cost_usd, total_requests, updated_at_ms)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(org_id) DO UPDATE SET
			total_cost_usd = total_cost_usd + excluded.total_cost_usd,
			total_requests = total_requests + 1,
			updated_at_ms = excluded.updated_at_ms`,
		record.OrgID, record.CostUSD, createdAtMs,
	); err != nil {
		return fmt.Errorf("upsert analytics rollup: %w", err)
	}

	if err := ensureAnalyticsDefaultLimitsTx(ctx, tx, record.OrgID, createdAtMs); err != nil {
		return err
	}

	return tx.Commit()
}

func (u *UsageStore) replaceAnalyticsEffectiveLimits(orgID string, limits []SpendLimit, updatedAtMs int64) error {
	if u == nil || u.analytics == nil {
		return nil
	}

	u.analyticsMu.Lock()
	defer u.analyticsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tx, err := u.analytics.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin limits analytics tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM org_effective_limits WHERE org_id = ?`, orgID); err != nil {
		return fmt.Errorf("clear analytics limits: %w", err)
	}
	for _, limit := range limits {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_effective_limits (org_id, window_ms, limit_usd, label, updated_at_ms)
			VALUES (?, ?, ?, ?, ?)`,
			orgID, limit.Window.Milliseconds(), limit.LimitUSD, limit.Label, updatedAtMs,
		); err != nil {
			return fmt.Errorf("insert analytics limit: %w", err)
		}
	}
	return tx.Commit()
}

func (u *UsageStore) rebuildAnalyticsIndex() error {
	if u == nil || u.analytics == nil {
		return nil
	}

	u.analyticsMu.Lock()
	defer u.analyticsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := u.analytics.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rebuild analytics tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for _, stmt := range []string{
		`DELETE FROM org_usage_rollups`,
		`DELETE FROM usage_events`,
		`DELETE FROM org_effective_limits`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("clear analytics tables: %w", err)
		}
	}

	orgIDs, err := u.listOrgIDsOnDisk()
	if err != nil {
		return err
	}
	for _, orgID := range orgIDs {
		db, err := u.getDB(orgID)
		if err != nil {
			return fmt.Errorf("open org db during analytics rebuild: %w", err)
		}

		var totalCostUSD float64
		var totalRequests int64
		var updatedAtMs int64
		err = db.QueryRowContext(ctx, `
			SELECT total_cost_usd, total_requests, updated_at_ms
			FROM spend WHERE id = 1`,
		).Scan(&totalCostUSD, &totalRequests, &updatedAtMs)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read org spend rollup: %w", err)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_usage_rollups (org_id, total_cost_usd, total_requests, updated_at_ms)
				VALUES (?, ?, ?, ?)`,
				orgID, totalCostUSD, totalRequests, updatedAtMs,
			); err != nil {
				return fmt.Errorf("insert rebuilt org rollup: %w", err)
			}
		}

		rows, err := db.QueryContext(ctx, `
			SELECT workspace_id, user_id, thread_id, model, provider,
			       input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
			       cost_usd, duration_ms, created_at_ms
			FROM usage_log`)
		if err != nil {
			return fmt.Errorf("query org usage log during rebuild: %w", err)
		}
		for rows.Next() {
			var entry UsageLogEntry
			if err := rows.Scan(
				&entry.WorkspaceID, &entry.UserID, &entry.ThreadID,
				&entry.Model, &entry.Provider,
				&entry.InputTokens, &entry.OutputTokens,
				&entry.CacheCreationInputTokens, &entry.CacheReadInputTokens,
				&entry.CostUSD, &entry.DurationMs, &entry.CreatedAtMs,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scan usage log during rebuild: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO usage_events (
					org_id, workspace_id, user_id, thread_id, model, provider,
					input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
					cost_usd, duration_ms, created_at_ms
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				orgID, entry.WorkspaceID, entry.UserID, entry.ThreadID,
				entry.Model, entry.Provider,
				entry.InputTokens, entry.OutputTokens,
				entry.CacheCreationInputTokens, entry.CacheReadInputTokens,
				entry.CostUSD, entry.DurationMs, entry.CreatedAtMs,
			); err != nil {
				rows.Close()
				return fmt.Errorf("insert usage event during rebuild: %w", err)
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close usage log rows during rebuild: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate usage log during rebuild: %w", err)
		}

		limits, err := u.GetSpendLimits(orgID)
		if err != nil {
			return fmt.Errorf("load spend limits during rebuild: %w", err)
		}
		for _, limit := range limits {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_effective_limits (org_id, window_ms, limit_usd, label, updated_at_ms)
				VALUES (?, ?, ?, ?, ?)`,
				orgID, limit.Window.Milliseconds(), limit.LimitUSD, limit.Label, updatedAtMs,
			); err != nil {
				return fmt.Errorf("insert effective limit during rebuild: %w", err)
			}
		}
	}

	if err := writeAnalyticsMetaValueTx(ctx, tx, analyticsMetaSchemaVersionKey, analyticsSchemaVersion); err != nil {
		return err
	}

	return tx.Commit()
}

func (u *UsageStore) listOrgIDsOnDisk() ([]string, error) {
	entries, err := os.ReadDir(u.baseDir)
	if err != nil {
		return nil, fmt.Errorf("list usage dir: %w", err)
	}

	orgIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == analyticsDirName {
			continue
		}
		dbPath := filepath.Join(u.baseDir, entry.Name(), "usage.db")
		if _, err := os.Stat(dbPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat usage db %s: %w", dbPath, err)
		}
		orgIDs = append(orgIDs, entry.Name())
	}
	slices.Sort(orgIDs)
	return orgIDs, nil
}

func (u *UsageStore) ListSpamOrgIDs() ([]string, error) {
	if u == nil || u.analytics == nil {
		return nil, nil
	}

	u.analyticsMu.RLock()
	defer u.analyticsMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := u.analytics.QueryContext(ctx, `
		SELECT org_id
		FROM org_effective_limits
		GROUP BY org_id
		HAVING COUNT(*) > 0 AND MAX(limit_usd) <= 0.01
		ORDER BY org_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query spam org ids: %w", err)
	}
	defer rows.Close()

	var orgIDs []string
	for rows.Next() {
		var orgID string
		if err := rows.Scan(&orgID); err != nil {
			return nil, fmt.Errorf("scan spam org id: %w", err)
		}
		orgIDs = append(orgIDs, orgID)
	}
	return orgIDs, rows.Err()
}

func (u *UsageStore) GetDailySpendAnalytics(options DailySpendAnalyticsQuery) (DailySpendAnalytics, error) {
	selectedDayStart, err := parseDailySpendDate(options.Date)
	if err != nil {
		return DailySpendAnalytics{}, err
	}

	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	topOrgsLimit := options.TopOrgsLimit
	switch {
	case topOrgsLimit <= 0:
		topOrgsLimit = 20
	case topOrgsLimit > 50:
		topOrgsLimit = 50
	}

	selectedDate := selectedDayStart.Format("2006-01-02")
	previousDayStart := selectedDayStart.Add(-24 * time.Hour)
	nextDayStart := selectedDayStart.Add(24 * time.Hour)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	isPartial := selectedDayStart.Equal(todayStart)
	effectiveDayEnd := nextDayStart
	lastHour := 23
	if isPartial {
		effectiveDayEnd = now
		lastHour = now.Hour()
		if effectiveDayEnd.Before(selectedDayStart) {
			effectiveDayEnd = selectedDayStart
			lastHour = 0
		}
	}

	result := DailySpendAnalytics{
		Date:            selectedDate,
		IsPartial:       isPartial,
		PreviousDay:     DailySpendSummary{Date: previousDayStart.Format("2006-01-02")},
		HourlySeries:    make([]DailySpendHourlyRow, 0, lastHour+1),
		ModelBreakdown:  []DailySpendModelRow{},
		TopOrgs:         []DailySpendOrgRow{},
		SpamOrgCount:    0,
		NonSpamOrgCount: 0,
	}
	for hour := 0; hour <= lastHour; hour += 1 {
		result.HourlySeries = append(result.HourlySeries, DailySpendHourlyRow{Hour: hour})
	}

	if u == nil || u.analytics == nil {
		return result, nil
	}

	orgIDs := normalizeDailySpendOrgIDs(options.OrgIDs)
	if len(orgIDs) == 0 {
		return result, nil
	}

	spamOrgIDs, err := u.ListSpamOrgIDs()
	if err != nil {
		return DailySpendAnalytics{}, err
	}
	spamOrgIDSet := make(map[string]struct{}, len(spamOrgIDs))
	for _, orgID := range spamOrgIDs {
		spamOrgIDSet[orgID] = struct{}{}
	}

	u.analyticsMu.RLock()
	defer u.analyticsMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	selectedStartMs := selectedDayStart.UnixMilli()
	selectedEndMs := effectiveDayEnd.UnixMilli()
	previousStartMs := previousDayStart.UnixMilli()
	previousEndMs := selectedDayStart.UnixMilli()
	perOrg := make(map[string]*DailySpendOrgRow, len(orgIDs))
	modelBreakdownByName := make(map[string]*DailySpendModelRow)

	for _, chunk := range chunkDailySpendOrgIDs(orgIDs, 400) {
		if err := u.accumulateDailySpendOrgTotals(
			ctx,
			chunk,
			selectedStartMs,
			selectedEndMs,
			perOrg,
		); err != nil {
			return DailySpendAnalytics{}, err
		}

		previousRows, err := u.queryDailySpendGroupedByOrg(
			ctx,
			chunk,
			previousStartMs,
			previousEndMs,
		)
		if err != nil {
			return DailySpendAnalytics{}, err
		}
		for _, row := range previousRows {
			result.PreviousDay.TotalSpendUSD += row.SpendUSD
			result.PreviousDay.TotalRequests += row.Requests
			if _, isSpam := spamOrgIDSet[row.OrgID]; isSpam {
				result.PreviousDay.SpamSpendUSD += row.SpendUSD
				continue
			}
			result.PreviousDay.NonSpamSpendUSD += row.SpendUSD
		}

		if err := u.accumulateDailySpendHourlySeries(
			ctx,
			chunk,
			selectedStartMs,
			selectedEndMs,
			spamOrgIDSet,
			result.HourlySeries,
		); err != nil {
			return DailySpendAnalytics{}, err
		}

		if err := u.accumulateDailySpendModelBreakdown(
			ctx,
			chunk,
			selectedStartMs,
			selectedEndMs,
			modelBreakdownByName,
		); err != nil {
			return DailySpendAnalytics{}, err
		}
	}

	orgRows := make([]DailySpendOrgRow, 0, len(perOrg))
	for _, orgID := range orgIDs {
		row := perOrg[orgID]
		if row == nil || row.Requests <= 0 {
			continue
		}
		_, row.IsSpam = spamOrgIDSet[row.OrgID]
		result.TotalSpendUSD += row.SpendUSD
		result.TotalRequests += row.Requests
		if row.IsSpam {
			result.SpamSpendUSD += row.SpendUSD
			result.SpamOrgCount += 1
		} else {
			result.NonSpamSpendUSD += row.SpendUSD
			result.NonSpamOrgCount += 1
		}
		orgRows = append(orgRows, *row)
	}

	slices.SortFunc(orgRows, func(left, right DailySpendOrgRow) int {
		if left.SpendUSD != right.SpendUSD {
			if left.SpendUSD > right.SpendUSD {
				return -1
			}
			return 1
		}
		if left.Requests != right.Requests {
			if left.Requests > right.Requests {
				return -1
			}
			return 1
		}
		if left.OrgID < right.OrgID {
			return -1
		}
		if left.OrgID > right.OrgID {
			return 1
		}
		return 0
	})

	topCount := minDailySpendInt(len(orgRows), topOrgsLimit)
	if topCount > 0 {
		result.TopOrgs = append(result.TopOrgs, orgRows[:topCount]...)
	}
	for _, row := range orgRows[topCount:] {
		result.OtherOrgsSpendUSD += row.SpendUSD
		result.OtherOrgsCount += 1
	}

	modelRows := make([]DailySpendModelRow, 0, len(modelBreakdownByName))
	for _, row := range modelBreakdownByName {
		modelRows = append(modelRows, *row)
	}
	slices.SortFunc(modelRows, func(left, right DailySpendModelRow) int {
		if left.SpendUSD != right.SpendUSD {
			if left.SpendUSD > right.SpendUSD {
				return -1
			}
			return 1
		}
		if left.Requests != right.Requests {
			if left.Requests > right.Requests {
				return -1
			}
			return 1
		}
		if left.Model < right.Model {
			return -1
		}
		if left.Model > right.Model {
			return 1
		}
		return 0
	})
	result.ModelBreakdown = modelRows

	return result, nil
}

func (u *UsageStore) GetOrgUsageAnalytics(orgIDs []string, includeWindows bool) ([]OrgUsageAnalyticsRow, error) {
	if u == nil || u.analytics == nil || len(orgIDs) == 0 {
		return []OrgUsageAnalyticsRow{}, nil
	}

	orderedOrgIDs := append([]string(nil), orgIDs...)
	slices.Sort(orderedOrgIDs)

	u.analyticsMu.RLock()
	defer u.analyticsMu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultByOrg := make(map[string]*OrgUsageAnalyticsRow, len(orderedOrgIDs))
	for _, orgID := range orderedOrgIDs {
		resultByOrg[orgID] = &OrgUsageAnalyticsRow{OrgID: orgID}
	}

	placeholders := make([]string, len(orderedOrgIDs))
	args := make([]any, 0, len(orderedOrgIDs))
	for i, orgID := range orderedOrgIDs {
		placeholders[i] = "?"
		args = append(args, orgID)
	}

	rollupQuery := fmt.Sprintf(`
		SELECT org_id, total_cost_usd, total_requests
		FROM org_usage_rollups
		WHERE org_id IN (%s)`, strings.Join(placeholders, ","))
	rows, err := u.analytics.QueryContext(ctx, rollupQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage rollups: %w", err)
	}
	for rows.Next() {
		var orgID string
		var totalCostUSD float64
		var totalRequests int64
		if err := rows.Scan(&orgID, &totalCostUSD, &totalRequests); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan usage rollup: %w", err)
		}
		if row := resultByOrg[orgID]; row != nil {
			row.TotalCostUSD = totalCostUSD
			row.TotalRequests = totalRequests
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close usage rollups rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage rollups rows: %w", err)
	}

	now := time.Now().UTC().UnixMilli()
	windowAggregates := []struct {
		label  string
		cutoff int64
		setter func(*OrgUsageAnalyticsRow, float64)
	}{
		{
			label:  "spend_7d",
			cutoff: now - (7 * 24 * time.Hour).Milliseconds(),
			setter: func(row *OrgUsageAnalyticsRow, value float64) { row.Spend7d = value },
		},
		{
			label:  "spend_30d",
			cutoff: now - (30 * 24 * time.Hour).Milliseconds(),
			setter: func(row *OrgUsageAnalyticsRow, value float64) { row.Spend30d = value },
		},
	}
	for _, aggregate := range windowAggregates {
		windowArgs := append([]any{aggregate.cutoff}, args...)
		windowQuery := fmt.Sprintf(`
			SELECT org_id, COALESCE(SUM(cost_usd), 0)
			FROM usage_events
			WHERE created_at_ms >= ? AND org_id IN (%s)
			GROUP BY org_id`, strings.Join(placeholders, ","))
		rows, err := u.analytics.QueryContext(ctx, windowQuery, windowArgs...)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", aggregate.label, err)
		}
		for rows.Next() {
			var orgID string
			var spentUSD float64
			if err := rows.Scan(&orgID, &spentUSD); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s: %w", aggregate.label, err)
			}
			if row := resultByOrg[orgID]; row != nil {
				aggregate.setter(row, spentUSD)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close %s rows: %w", aggregate.label, err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate %s rows: %w", aggregate.label, err)
		}
	}

	if includeWindows {
		if err := u.loadAnalyticsWindows(ctx, resultByOrg, orderedOrgIDs, placeholders, args, now); err != nil {
			return nil, err
		}
	}

	items := make([]OrgUsageAnalyticsRow, 0, len(orderedOrgIDs))
	for _, orgID := range orgIDs {
		if row := resultByOrg[orgID]; row != nil {
			items = append(items, *row)
		}
	}
	return items, nil
}

func (u *UsageStore) accumulateDailySpendOrgTotals(
	ctx context.Context,
	orgIDs []string,
	fromMs int64,
	toMs int64,
	target map[string]*DailySpendOrgRow,
) error {
	rows, err := u.queryDailySpendGroupedByOrg(ctx, orgIDs, fromMs, toMs)
	if err != nil {
		return err
	}
	for _, row := range rows {
		current := target[row.OrgID]
		if current == nil {
			current = &DailySpendOrgRow{OrgID: row.OrgID}
			target[row.OrgID] = current
		}
		current.SpendUSD += row.SpendUSD
		current.Requests += row.Requests
	}
	return nil
}

func (u *UsageStore) queryDailySpendGroupedByOrg(
	ctx context.Context,
	orgIDs []string,
	fromMs int64,
	toMs int64,
) ([]DailySpendOrgRow, error) {
	if fromMs >= toMs || len(orgIDs) == 0 {
		return []DailySpendOrgRow{}, nil
	}

	query, args := buildDailySpendFilterQuery(
		`
			SELECT org_id, COALESCE(SUM(cost_usd), 0), COUNT(*)
			FROM usage_events
		`,
		orgIDs,
		fromMs,
		toMs,
		"GROUP BY org_id",
	)
	rows, err := u.analytics.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query daily spend org totals: %w", err)
	}
	defer rows.Close()

	result := make([]DailySpendOrgRow, 0, len(orgIDs))
	for rows.Next() {
		var row DailySpendOrgRow
		if err := rows.Scan(&row.OrgID, &row.SpendUSD, &row.Requests); err != nil {
			return nil, fmt.Errorf("scan daily spend org total: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily spend org totals: %w", err)
	}
	return result, nil
}

func (u *UsageStore) accumulateDailySpendHourlySeries(
	ctx context.Context,
	orgIDs []string,
	fromMs int64,
	toMs int64,
	spamOrgIDSet map[string]struct{},
	hourlySeries []DailySpendHourlyRow,
) error {
	if fromMs >= toMs || len(orgIDs) == 0 || len(hourlySeries) == 0 {
		return nil
	}

	query, args := buildDailySpendFilterQuery(
		`
			SELECT CAST(strftime('%H', created_at_ms / 1000, 'unixepoch') AS INTEGER), org_id, COALESCE(SUM(cost_usd), 0), COUNT(*)
			FROM usage_events
		`,
		orgIDs,
		fromMs,
		toMs,
		"GROUP BY 1, 2",
	)
	rows, err := u.analytics.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query daily spend hourly series: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var hour int
		var orgID string
		var spendUSD float64
		var requests int64
		if err := rows.Scan(&hour, &orgID, &spendUSD, &requests); err != nil {
			return fmt.Errorf("scan daily spend hourly row: %w", err)
		}
		if hour < 0 || hour >= len(hourlySeries) {
			continue
		}
		hourlySeries[hour].SpendUSD += spendUSD
		hourlySeries[hour].Requests += requests
		if _, isSpam := spamOrgIDSet[orgID]; isSpam {
			hourlySeries[hour].SpamSpendUSD += spendUSD
			continue
		}
		hourlySeries[hour].NonSpamSpendUSD += spendUSD
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate daily spend hourly series: %w", err)
	}
	return nil
}

func (u *UsageStore) accumulateDailySpendModelBreakdown(
	ctx context.Context,
	orgIDs []string,
	fromMs int64,
	toMs int64,
	target map[string]*DailySpendModelRow,
) error {
	if fromMs >= toMs || len(orgIDs) == 0 {
		return nil
	}

	query, args := buildDailySpendFilterQuery(
		`
			SELECT model, COALESCE(SUM(cost_usd), 0), COUNT(*)
			FROM usage_events
		`,
		orgIDs,
		fromMs,
		toMs,
		"GROUP BY model",
	)
	rows, err := u.analytics.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query daily spend model breakdown: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var model string
		var spendUSD float64
		var requests int64
		if err := rows.Scan(&model, &spendUSD, &requests); err != nil {
			return fmt.Errorf("scan daily spend model row: %w", err)
		}
		model = normalizeDailySpendModel(model)
		current := target[model]
		if current == nil {
			current = &DailySpendModelRow{Model: model}
			target[model] = current
		}
		current.SpendUSD += spendUSD
		current.Requests += requests
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate daily spend model breakdown: %w", err)
	}
	return nil
}

func buildDailySpendFilterQuery(
	baseQuery string,
	orgIDs []string,
	fromMs int64,
	toMs int64,
	suffix string,
) (string, []any) {
	placeholders := make([]string, len(orgIDs))
	args := make([]any, 0, len(orgIDs)+2)
	args = append(args, fromMs, toMs)
	for index, orgID := range orgIDs {
		placeholders[index] = "?"
		args = append(args, orgID)
	}
	query := fmt.Sprintf(
		`%s
			WHERE created_at_ms >= ? AND created_at_ms < ?
			  AND org_id IN (%s)
			%s`,
		baseQuery,
		strings.Join(placeholders, ","),
		suffix,
	)
	return query, args
}

func normalizeDailySpendOrgIDs(orgIDs []string) []string {
	seen := make(map[string]struct{}, len(orgIDs))
	result := make([]string, 0, len(orgIDs))
	for _, rawOrgID := range orgIDs {
		orgID := strings.TrimSpace(rawOrgID)
		if orgID == "" {
			continue
		}
		if _, ok := seen[orgID]; ok {
			continue
		}
		seen[orgID] = struct{}{}
		result = append(result, orgID)
	}
	return result
}

func normalizeDailySpendModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

func chunkDailySpendOrgIDs(orgIDs []string, chunkSize int) [][]string {
	if len(orgIDs) == 0 {
		return nil
	}
	if chunkSize <= 0 {
		chunkSize = len(orgIDs)
	}

	chunks := make([][]string, 0, (len(orgIDs)+chunkSize-1)/chunkSize)
	for start := 0; start < len(orgIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(orgIDs) {
			end = len(orgIDs)
		}
		chunks = append(chunks, orgIDs[start:end])
	}
	return chunks
}

func parseDailySpendDate(date string) (time.Time, error) {
	if strings.TrimSpace(date) == "" {
		return time.Time{}, errors.New("daily spend date is required")
	}
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid daily spend date: %w", err)
	}
	return parsed.UTC(), nil
}

func minDailySpendInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

func (u *UsageStore) loadAnalyticsWindows(
	ctx context.Context,
	resultByOrg map[string]*OrgUsageAnalyticsRow,
	orderedOrgIDs []string,
	placeholders []string,
	args []any,
	now int64,
) error {
	type storedLimit struct {
		label    string
		windowMs int64
		limitUSD float64
	}

	limitQuery := fmt.Sprintf(`
		SELECT org_id, label, window_ms, limit_usd
		FROM org_effective_limits
		WHERE org_id IN (%s)
		ORDER BY org_id ASC, window_ms ASC`, strings.Join(placeholders, ","))
	rows, err := u.analytics.QueryContext(ctx, limitQuery, args...)
	if err != nil {
		return fmt.Errorf("query effective limits: %w", err)
	}
	storedLimitsByOrg := make(map[string][]storedLimit, len(orderedOrgIDs))
	for rows.Next() {
		var orgID string
		var limit storedLimit
		if err := rows.Scan(&orgID, &limit.label, &limit.windowMs, &limit.limitUSD); err != nil {
			rows.Close()
			return fmt.Errorf("scan effective limit: %w", err)
		}
		storedLimitsByOrg[orgID] = append(storedLimitsByOrg[orgID], limit)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close effective limits rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate effective limits rows: %w", err)
	}

	defaultMissingOrgIDs := make([]string, 0)
	for _, orgID := range orderedOrgIDs {
		if len(storedLimitsByOrg[orgID]) == 0 {
			defaultMissingOrgIDs = append(defaultMissingOrgIDs, orgID)
		}
	}

	if len(storedLimitsByOrg) > 0 {
		spendRows, err := u.analytics.QueryContext(ctx, fmt.Sprintf(`
			SELECT l.org_id, l.label, l.window_ms, l.limit_usd, COALESCE(SUM(e.cost_usd), 0)
			FROM org_effective_limits l
			LEFT JOIN usage_events e
			  ON e.org_id = l.org_id
			 AND e.created_at_ms >= (? - l.window_ms)
			WHERE l.org_id IN (%s)
			GROUP BY l.org_id, l.label, l.window_ms, l.limit_usd
			ORDER BY l.org_id ASC, l.window_ms ASC`, strings.Join(placeholders, ",")), append([]any{now}, args...)...)
		if err != nil {
			return fmt.Errorf("query window spends: %w", err)
		}
		for spendRows.Next() {
			var orgID string
			var label string
			var windowMs int64
			var limitUSD float64
			var spentUSD float64
			if err := spendRows.Scan(&orgID, &label, &windowMs, &limitUSD, &spentUSD); err != nil {
				spendRows.Close()
				return fmt.Errorf("scan window spend: %w", err)
			}
			if row := resultByOrg[orgID]; row != nil {
				row.Windows = append(row.Windows, WindowSpend{
					Label:    label,
					WindowMs: windowMs,
					LimitUSD: limitUSD,
					SpentUSD: spentUSD,
					Exceeded: spentUSD >= limitUSD,
				})
			}
		}
		if err := spendRows.Close(); err != nil {
			return fmt.Errorf("close window spend rows: %w", err)
		}
		if err := spendRows.Err(); err != nil {
			return fmt.Errorf("iterate window spend rows: %w", err)
		}
	}

	if len(defaultMissingOrgIDs) == 0 {
		return nil
	}

	missingPlaceholders := make([]string, len(defaultMissingOrgIDs))
	missingArgs := make([]any, 0, len(defaultMissingOrgIDs))
	for i, orgID := range defaultMissingOrgIDs {
		missingPlaceholders[i] = "?"
		missingArgs = append(missingArgs, orgID)
	}

	for _, limit := range DefaultSpendLimits {
		queryArgs := append([]any{now - limit.Window.Milliseconds()}, missingArgs...)
		defaultWindowRows, err := u.analytics.QueryContext(ctx, fmt.Sprintf(`
			SELECT org_id, COALESCE(SUM(cost_usd), 0)
			FROM usage_events
			WHERE created_at_ms >= ? AND org_id IN (%s)
			GROUP BY org_id`, strings.Join(missingPlaceholders, ",")), queryArgs...)
		if err != nil {
			return fmt.Errorf("query default window spend: %w", err)
		}
		spendByOrg := make(map[string]float64, len(defaultMissingOrgIDs))
		for defaultWindowRows.Next() {
			var orgID string
			var spentUSD float64
			if err := defaultWindowRows.Scan(&orgID, &spentUSD); err != nil {
				defaultWindowRows.Close()
				return fmt.Errorf("scan default window spend: %w", err)
			}
			spendByOrg[orgID] = spentUSD
		}
		if err := defaultWindowRows.Close(); err != nil {
			return fmt.Errorf("close default window spend rows: %w", err)
		}
		if err := defaultWindowRows.Err(); err != nil {
			return fmt.Errorf("iterate default window spend rows: %w", err)
		}

		for _, orgID := range defaultMissingOrgIDs {
			spentUSD := spendByOrg[orgID]
			if row := resultByOrg[orgID]; row != nil {
				row.Windows = append(row.Windows, WindowSpend{
					Label:    limit.Label,
					WindowMs: limit.Window.Milliseconds(),
					LimitUSD: limit.LimitUSD,
					SpentUSD: spentUSD,
					Exceeded: spentUSD >= limit.LimitUSD,
				})
			}
		}
	}

	return nil
}
