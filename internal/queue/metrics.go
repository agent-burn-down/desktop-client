package queue

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

// MetricItem is a leased metrics_queue row handed to the uploader.
type MetricItem struct {
	ID       int64
	Point    api.MetricPoint
	Attempts int
}

// initMetrics creates the metrics_queue table and its index, mirroring the
// queue table's state machine (pending -> leased -> acked). Metric points
// carry no server-side idempotency key (unlike events' event_id), so there is
// no equivalent column here.
func (q *Queue) initMetrics() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS metrics_queue (
			id INTEGER PRIMARY KEY,
			payload TEXT NOT NULL,
			created_at TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL DEFAULT 'pending',
			leased_until TEXT,
			acked_at TEXT
		)`,
		"CREATE INDEX IF NOT EXISTS idx_metrics_queue_state ON metrics_queue(state, id)",
	}
	for _, s := range stmts {
		if _, err := q.db.Exec(s); err != nil {
			return fmt.Errorf("init metrics_queue schema: %w", err)
		}
	}
	return nil
}

// EnqueueMetrics marshals each point and appends it as a pending metrics_queue
// row, then evicts oldest rows if a capacity cap is exceeded.
func (q *Queue) EnqueueMetrics(points []api.MetricPoint) error {
	if len(points) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC().Format(timeLayout)
	tx, err := q.db.Begin()
	if err != nil {
		return fmt.Errorf("begin enqueue metrics: %w", err)
	}
	if err := insertMetrics(tx, points, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue metrics: %w", err)
	}
	return q.evictMetricsIfNeeded()
}

func insertMetrics(tx *sql.Tx, points []api.MetricPoint, now string) error {
	stmt, err := tx.Prepare("INSERT INTO metrics_queue(payload, created_at) VALUES(?, ?)")
	if err != nil {
		return fmt.Errorf("prepare insert metrics: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for i := range points {
		payload, err := json.Marshal(points[i])
		if err != nil {
			return fmt.Errorf("marshal metric point: %w", err)
		}
		if _, err := stmt.Exec(string(payload), now); err != nil {
			return fmt.Errorf("insert metric point: %w", err)
		}
	}
	return nil
}

// LeaseMetricsBatch marks up to n pending (or lease-expired) metrics_queue rows
// leased for leaseDur and returns them oldest-first. Acked rows are never
// returned.
func (q *Queue) LeaseMetricsBatch(n int, leaseDur time.Duration) ([]MetricItem, error) {
	if n <= 0 {
		return nil, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC()
	ids, items, err := q.selectLeasableMetrics(now, n)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	until := now.Add(leaseDur).Format(timeLayout)
	setClause := "state='leased', leased_until=?"
	if err := q.updateByIDs("metrics_queue", setClause, []any{until}, ids); err != nil {
		return nil, err
	}
	return items, nil
}

func (q *Queue) selectLeasableMetrics(now time.Time, n int) ([]int64, []MetricItem, error) {
	rows, err := q.db.Query(
		`SELECT id, payload, attempts FROM metrics_queue
		 WHERE state='pending' OR (state='leased' AND (leased_until IS NULL OR leased_until < ?))
		 ORDER BY id ASC LIMIT ?`,
		now.Format(timeLayout), n)
	if err != nil {
		return nil, nil, fmt.Errorf("select leasable metrics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	var items []MetricItem
	for rows.Next() {
		item, err := scanMetricItem(rows)
		if err != nil {
			return nil, nil, err
		}
		ids = append(ids, item.ID)
		items = append(items, item)
	}
	return ids, items, rows.Err()
}

func scanMetricItem(rows *sql.Rows) (MetricItem, error) {
	var it MetricItem
	var payload string
	if err := rows.Scan(&it.ID, &payload, &it.Attempts); err != nil {
		return MetricItem{}, fmt.Errorf("scan metric item: %w", err)
	}
	if err := json.Unmarshal([]byte(payload), &it.Point); err != nil {
		return MetricItem{}, fmt.Errorf("unmarshal payload for metrics row %d: %w", it.ID, err)
	}
	return it, nil
}

// AckMetrics marks the given metrics_queue rows acked with an acked_at
// timestamp.
func (q *Queue) AckMetrics(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UTC().Format(timeLayout)
	setClause := "state='acked', acked_at=?, leased_until=NULL"
	return q.updateByIDs("metrics_queue", setClause, []any{now}, ids)
}

// NackMetrics clears the lease and increments the attempt count so the rows
// become leasable again.
func (q *Queue) NackMetrics(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.updateByIDs(
		"metrics_queue", "state='pending', leased_until=NULL, attempts=attempts+1", nil, ids)
}

// PruneAckedMetrics deletes acked metrics_queue rows whose acked_at is older
// than cutoff, mirroring PruneAcked so the metrics backlog is bounded by the
// same retention window as events.
func (q *Queue) PruneAckedMetrics(cutoff time.Time) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	res, err := q.db.Exec(
		"DELETE FROM metrics_queue WHERE state='acked' AND acked_at IS NOT NULL AND acked_at < ?",
		cutoff.UTC().Format(timeLayout))
	if err != nil {
		return 0, fmt.Errorf("prune acked metrics: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune metrics rows affected: %w", err)
	}
	if deleted > 0 {
		if _, err := q.db.Exec("PRAGMA incremental_vacuum"); err != nil {
			return deleted, fmt.Errorf("incremental vacuum after metrics prune: %w", err)
		}
	}
	return deleted, nil
}

// evictMetricsIfNeeded drops metrics_queue rows until both the row and byte
// caps are satisfied, mirroring evictIfNeeded for the events queue. Byte size
// is measured on the whole database file (both tables share one file), so
// either queue's Enqueue path can trigger eviction of its own backlog rather
// than the other table's. Callers must hold q.mu.
func (q *Queue) evictMetricsIfNeeded() error {
	for {
		over, rows, err := q.overCapMetrics()
		if err != nil {
			return err
		}
		if !over {
			return nil
		}
		done, err := q.evictMetricsRound(rows)
		if err != nil || done {
			return err
		}
	}
}

// overCapMetrics reports whether either capacity cap is exceeded, along with
// the current metrics_queue row count.
func (q *Queue) overCapMetrics() (bool, int64, error) {
	rows, err := q.scalar("SELECT COUNT(*) FROM metrics_queue")
	if err != nil {
		return false, 0, err
	}
	size, err := q.sizeBytes()
	if err != nil {
		return false, 0, err
	}
	return rows > q.maxRows || size > q.maxBytes, rows, nil
}

// evictMetricsRound deletes one batch of victims and vacuums. It reports
// done=true when nothing more can be evicted.
func (q *Queue) evictMetricsRound(rows int64) (bool, error) {
	deleted, err := q.deleteMetricsVictims(evictTarget(rows, q.maxRows))
	if err != nil {
		return false, err
	}
	if deleted == 0 {
		return true, nil
	}
	if _, err := q.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		return false, fmt.Errorf("incremental vacuum: %w", err)
	}
	return false, nil
}

func (q *Queue) deleteMetricsVictims(n int64) (int64, error) {
	// Acked rows are delivered history retained only for local stats, so they
	// are sacrificed first; undelivered rows go oldest-first only after that.
	res, err := q.db.Exec(
		`DELETE FROM metrics_queue WHERE id IN (
			SELECT id FROM metrics_queue ORDER BY (state='acked') DESC, id ASC LIMIT ?)`, n)
	if err != nil {
		return 0, fmt.Errorf("evict metrics rows: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("evict metrics rows affected: %w", err)
	}
	return deleted, nil
}
