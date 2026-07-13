// Package queue implements the durable disk-backed upload queue using a pure-Go
// SQLite database (modernc.org/sqlite, CGO-free). It replaces the reference
// collector's in-memory deque so buffered events survive crash and reboot.
//
// Rows move through states: pending -> leased -> acked. A lease is a temporary
// hold taken while an upload is in flight; if the process dies the lease
// expires and the row becomes leasable again, so nothing is lost until it is
// explicitly acked. Acked rows are retained (not deleted) for local stats.
package queue

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/google/uuid"
	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const (
	defaultMaxRows  int64 = 50_000
	defaultMaxBytes int64 = 100 * 1024 * 1024
	evictChunk      int64 = 512
	// timeLayout is a FIXED-WIDTH UTC layout used for every TEXT timestamp the
	// queue writes (created_at, acked_at, leased_until) and every cutoff it
	// compares against in SQL. It must stay fixed-width: time.RFC3339Nano trims
	// trailing fractional zeros, so ".1Z" and ".12Z" sort lexicographically
	// opposite to chronological order, silently corrupting SQLite TEXT range
	// comparisons (lease expiry, retention prune, stats window). A full 9-digit
	// fraction keeps string order == time order. Always format after .UTC() so
	// the zone suffix is always "Z" and every value shares one timeline.
	timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
	// DefaultTopTools is the number of tool_name rows returned by StatsSince.
	DefaultTopTools = 10
)

// Options configures queue capacity caps. Zero values fall back to defaults
// (50,000 rows / 100 MB).
type Options struct {
	MaxRows  int64
	MaxBytes int64
}

// Item is a leased queue row handed to the uploader.
type Item struct {
	ID       int64
	EventID  string
	Event    api.NormalizedEvent
	Attempts int
}

// Stats is a snapshot of queue occupancy and lifetime eviction count.
type Stats struct {
	Pending int64
	Leased  int64
	Acked   int64
	Evicted int64
}

// Queue is a durable SQLite-backed upload queue. It is safe for concurrent use.
type Queue struct {
	db       *sql.DB
	maxRows  int64
	maxBytes int64

	mu      sync.Mutex
	evicted int64
}

// DefaultPath returns the queue database path inside the config directory,
// honouring the BURNDOWN_CONFIG_DIR override.
func DefaultPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "queue.db"), nil
}

// Open opens (creating if needed) the queue database at path with WAL enabled
// and the schema applied.
func Open(path string, opts Options) (*Queue, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open queue db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	q := &Queue{db: db, maxRows: opts.MaxRows, maxBytes: opts.MaxBytes}
	if q.maxRows <= 0 {
		q.maxRows = defaultMaxRows
	}
	if q.maxBytes <= 0 {
		q.maxBytes = defaultMaxBytes
	}
	if err := q.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := q.initMetrics(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return q, nil
}

// OpenReadOnly opens an existing queue database read-only for the stats reader.
// It does not create or migrate the schema and never takes a write lock, so it
// is safe to run concurrently with the daemon under WAL. A missing database
// yields an error satisfying errors.Is(err, os.ErrNotExist).
func OpenReadOnly(path string) (*Queue, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("queue db %s: %w", path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("stat queue db %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open queue db %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return &Queue{db: db, maxRows: defaultMaxRows, maxBytes: defaultMaxBytes}, nil
}

// Close closes the underlying database.
func (q *Queue) Close() error { return q.db.Close() }

func (q *Queue) init() error {
	stmts := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA auto_vacuum=INCREMENTAL",
		`CREATE TABLE IF NOT EXISTS queue (
			id INTEGER PRIMARY KEY,
			event_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL DEFAULT 'pending',
			leased_until TEXT,
			acked_at TEXT
		)`,
		"CREATE INDEX IF NOT EXISTS idx_queue_state ON queue(state, id)",
	}
	for _, s := range stmts {
		if _, err := q.db.Exec(s); err != nil {
			return fmt.Errorf("init queue schema: %w", err)
		}
	}
	return nil
}

// Enqueue marshals each event and appends it as a pending row with a fresh
// UUIDv7 event id, then evicts oldest rows if a capacity cap is exceeded.
func (q *Queue) Enqueue(events []api.NormalizedEvent) error {
	if len(events) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC().Format(timeLayout)
	tx, err := q.db.Begin()
	if err != nil {
		return fmt.Errorf("begin enqueue: %w", err)
	}
	if err := insertEvents(tx, events, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return q.evictIfNeeded()
}

func insertEvents(tx *sql.Tx, events []api.NormalizedEvent, now string) error {
	stmt, err := tx.Prepare(
		"INSERT INTO queue(event_id, payload, created_at) VALUES(?, ?, ?)")
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for i := range events {
		payload, err := json.Marshal(events[i])
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("mint event id: %w", err)
		}
		if _, err := stmt.Exec(id.String(), string(payload), now); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
	}
	return nil
}

// LeaseBatch marks up to n pending (or lease-expired) rows leased for leaseDur
// and returns them oldest-first. Acked rows are never returned.
func (q *Queue) LeaseBatch(n int, leaseDur time.Duration) ([]Item, error) {
	if n <= 0 {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC()
	ids, items, err := q.selectLeasable(now, n)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	until := now.Add(leaseDur).Format(timeLayout)
	if err := q.markLeased(ids, until); err != nil {
		return nil, err
	}
	return items, nil
}

func (q *Queue) selectLeasable(now time.Time, n int) ([]int64, []Item, error) {
	rows, err := q.db.Query(
		`SELECT id, event_id, payload, attempts FROM queue
		 WHERE state='pending' OR (state='leased' AND (leased_until IS NULL OR leased_until < ?))
		 ORDER BY id ASC LIMIT ?`,
		now.Format(timeLayout), n)
	if err != nil {
		return nil, nil, fmt.Errorf("select leasable: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	var items []Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, item.ID)
		items = append(items, item)
	}
	return ids, items, rows.Err()
}

func scanItem(rows *sql.Rows) (Item, error) {
	var it Item
	var payload string
	if err := rows.Scan(&it.ID, &it.EventID, &payload, &it.Attempts); err != nil {
		return Item{}, fmt.Errorf("scan item: %w", err)
	}
	if err := json.Unmarshal([]byte(payload), &it.Event); err != nil {
		return Item{}, fmt.Errorf("unmarshal payload for row %d: %w", it.ID, err)
	}
	return it, nil
}

func (q *Queue) markLeased(ids []int64, until string) error {
	return q.updateByIDs("queue", "state='leased', leased_until=?", []any{until}, ids)
}

// updateByIDs runs "UPDATE <table> SET <setClause> WHERE id IN (...)" with the
// leading SET-clause args followed by the row ids. table is always a fixed
// internal literal ("queue" or "metrics_queue"), never caller/user input.
func (q *Queue) updateByIDs(table, setClause string, leadingArgs []any, ids []int64) error {
	in := placeholders(len(ids))
	//nolint:gosec // G202: only "?" bind markers are concatenated; values bind separately
	query := "UPDATE " + table + " SET " + setClause + " WHERE id IN (" + in + ")"
	args := append(append([]any{}, leadingArgs...), idArgs(ids)...)
	if _, err := q.db.Exec(query, args...); err != nil {
		return fmt.Errorf("update rows: %w", err)
	}
	return nil
}

// Ack marks the given rows acked with an acked_at timestamp. Acked rows are
// retained for local stats and never re-leased.
func (q *Queue) Ack(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC().Format(timeLayout)
	return q.updateByIDs("queue", "state='acked', acked_at=?, leased_until=NULL", []any{now}, ids)
}

// Nack clears the lease and increments the attempt count so the rows become
// leasable again.
func (q *Queue) Nack(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.updateByIDs("queue", "state='pending', leased_until=NULL, attempts=attempts+1", nil, ids)
}

// PruneAcked deletes acked rows whose acked_at is older than cutoff and, when
// any were removed, reclaims freed pages via incremental vacuum so the on-disk
// file stays bounded. Pending and leased rows are never touched. It returns the
// number of rows deleted.
func (q *Queue) PruneAcked(cutoff time.Time) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(
		"DELETE FROM queue WHERE state='acked' AND acked_at IS NOT NULL AND acked_at < ?",
		cutoff.UTC().Format(timeLayout))
	if err != nil {
		return 0, fmt.Errorf("prune acked rows: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune rows affected: %w", err)
	}
	if deleted > 0 {
		if _, err := q.db.Exec("PRAGMA incremental_vacuum"); err != nil {
			return deleted, fmt.Errorf("incremental vacuum after prune: %w", err)
		}
	}
	return deleted, nil
}

// Depth returns the number of outstanding (not-yet-acked) rows.
func (q *Queue) Depth() (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.scalar("SELECT COUNT(*) FROM queue WHERE state != 'acked'")
}

// Stats returns queue occupancy by state plus the lifetime eviction count.
func (q *Queue) Stats() (Stats, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var s Stats
	rows, err := q.db.Query("SELECT state, COUNT(*) FROM queue GROUP BY state")
	if err != nil {
		return s, fmt.Errorf("stats query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return s, fmt.Errorf("scan stats: %w", err)
		}
		assignState(&s, state, n)
	}
	s.Evicted = q.evicted
	return s, rows.Err()
}

func assignState(s *Stats, state string, n int64) {
	switch state {
	case "pending":
		s.Pending = n
	case "leased":
		s.Leased = n
	case "acked":
		s.Acked = n
	}
}

// Check runs a SQLite integrity check and a trivial query for the doctor
// command. It returns an error if the database is corrupt or unqueryable.
func (q *Queue) Check() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	var result string
	if err := q.db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity check failed: %s", result)
	}
	if _, err := q.scalar("SELECT COUNT(*) FROM queue"); err != nil {
		return fmt.Errorf("queryability check: %w", err)
	}
	return nil
}

// evictIfNeeded drops rows until both the row and byte caps are satisfied:
// acked (already-delivered) history first, then the oldest un-acked rows
// (mirroring the reference deque's overflow). Callers must hold q.mu.
func (q *Queue) evictIfNeeded() error {
	for {
		over, rows, err := q.overCap()
		if err != nil {
			return err
		}
		if !over {
			return nil
		}
		done, err := q.evictRound(rows)
		if err != nil || done {
			return err
		}
	}
}

// overCap reports whether either capacity cap is exceeded, along with the
// current row count.
func (q *Queue) overCap() (bool, int64, error) {
	rows, err := q.scalar("SELECT COUNT(*) FROM queue")
	if err != nil {
		return false, 0, err
	}
	size, err := q.sizeBytes()
	if err != nil {
		return false, 0, err
	}
	return rows > q.maxRows || size > q.maxBytes, rows, nil
}

// evictRound deletes one batch of victims and vacuums. It reports done=true
// when nothing more can be evicted.
func (q *Queue) evictRound(rows int64) (bool, error) {
	deleted, err := q.deleteVictims(evictTarget(rows, q.maxRows))
	if err != nil {
		return false, err
	}
	if deleted == 0 {
		return true, nil
	}
	q.evicted += deleted
	if _, err := q.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		return false, fmt.Errorf("incremental vacuum: %w", err)
	}
	return false, nil
}

func evictTarget(rows, maxRows int64) int64 {
	if over := rows - maxRows; over > evictChunk {
		return over
	}
	return evictChunk
}

func (q *Queue) deleteVictims(n int64) (int64, error) {
	// Acked rows are delivered history retained only for local stats, so they
	// are sacrificed first; undelivered rows go oldest-first only after that.
	res, err := q.db.Exec(
		`DELETE FROM queue WHERE id IN (
			SELECT id FROM queue ORDER BY (state='acked') DESC, id ASC LIMIT ?)`, n)
	if err != nil {
		return 0, fmt.Errorf("evict rows: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("evict rows affected: %w", err)
	}
	return deleted, nil
}

func (q *Queue) sizeBytes() (int64, error) {
	pageCount, err := q.scalar("PRAGMA page_count")
	if err != nil {
		return 0, err
	}
	pageSize, err := q.scalar("PRAGMA page_size")
	if err != nil {
		return 0, err
	}
	return pageCount * pageSize, nil
}

func (q *Queue) scalar(query string) (int64, error) {
	var n int64
	if err := q.db.QueryRow(query).Scan(&n); err != nil {
		return 0, fmt.Errorf("scalar query %q: %w", query, err)
	}
	return n, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func idArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}
