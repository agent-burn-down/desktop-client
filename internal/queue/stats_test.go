package queue

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

// ackedEvent builds a NormalizedEvent carrying the fields stats reads.
func ackedEvent(ts string, in, out int64, cost float64, tool string) api.NormalizedEvent {
	ev := api.NormalizedEvent{Timestamp: &ts, InputTokens: &in, OutputTokens: &out, CostUSD: &cost}
	if tool != "" {
		ev.ToolName = &tool
	}
	return ev
}

// insertAcked inserts one acked row with an explicit acked_at, bypassing the
// normal enqueue/lease/ack path so tests can plant synthetic timestamps.
func insertAcked(t *testing.T, q *Queue, ackedAt time.Time, ev api.NormalizedEvent) {
	t.Helper()
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	ts := ackedAt.UTC().Format(timeLayout)
	_, err = q.db.Exec(
		`INSERT INTO queue(event_id, payload, created_at, state, acked_at)
		 VALUES(?,?,?,'acked',?)`, "e", string(payload), ts, ts)
	if err != nil {
		t.Fatalf("insert acked: %v", err)
	}
}

func TestPruneAckedDeletesOnlyOldAcked(t *testing.T) {
	q := openTemp(t, Options{})
	now := time.Now().UTC()
	// Old acked row: must be pruned.
	insertAcked(t, q, now.Add(-10*24*time.Hour), ackedEvent("", 1, 1, 0, ""))
	// Recent acked row: must survive.
	insertAcked(t, q, now.Add(-1*24*time.Hour), ackedEvent("", 1, 1, 0, ""))
	// Pending and leased rows: never touched by retention.
	if err := q.Enqueue(sampleEvents(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := q.LeaseBatch(1, time.Minute); err != nil {
		t.Fatal(err)
	}

	deleted, err := q.PruneAcked(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the old acked row)", deleted)
	}
	acked, _ := q.scalar("SELECT COUNT(*) FROM queue WHERE state='acked'")
	if acked != 1 {
		t.Fatalf("acked survivors = %d, want 1", acked)
	}
	depth, _ := q.Depth()
	if depth != 2 {
		t.Fatalf("non-acked rows = %d, want 2 (pending+leased untouched)", depth)
	}
}

// TestRetentionSoakKeepsDBBounded simulates 30 days of daily acked batches with a
// moving 7-day retention cutoff and asserts the row count never grows unbounded.
func TestRetentionSoakKeepsDBBounded(t *testing.T) {
	q := openTemp(t, Options{})
	base := time.Now().UTC().Add(-30 * 24 * time.Hour)
	const perDay = 300
	var maxRows int64
	for d := 0; d < 30; d++ {
		day := base.AddDate(0, 0, d)
		for i := 0; i < perDay; i++ {
			insertAcked(t, q, day, ackedEvent(day.Format(timeLayout), 10, 5, 0.01, "Bash"))
		}
		if _, err := q.PruneAcked(day.Add(-7 * 24 * time.Hour)); err != nil {
			t.Fatalf("prune day %d: %v", d, err)
		}
		rows, _ := q.scalar("SELECT COUNT(*) FROM queue")
		if rows > maxRows {
			maxRows = rows
		}
	}
	// A 7-day window over daily batches holds at most ~8 batches (window edges),
	// far below the 30*perDay = 9000 rows inserted overall.
	if maxRows > perDay*8 {
		t.Fatalf("retention did not bound the db: peak %d rows over %d inserted",
			maxRows, 30*perDay)
	}
}

func TestStatsConservationMatchesAcked(t *testing.T) {
	q := openTemp(t, Options{})
	today := time.Now().UTC().Format("2006-01-02") + "T12:00:00Z"
	events := []api.NormalizedEvent{
		ackedEvent(today, 100, 20, 0.50, "Bash"),
		ackedEvent(today, 200, 40, 1.25, "Read"),
		ackedEvent(today, 50, 10, 0.05, "Bash"),
	}
	if err := q.Enqueue(events); err != nil {
		t.Fatal(err)
	}
	items, err := q.LeaseBatch(len(events), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(idsOf(items)); err != nil {
		t.Fatal(err)
	}

	report, err := q.StatsSince(time.Now().Add(-7*24*time.Hour), DefaultTopTools)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if len(report.Daily) != 1 {
		t.Fatalf("daily rows = %d, want 1", len(report.Daily))
	}
	d := report.Daily[0]
	if d.Events != 3 || d.InputTokens != 350 || d.OutputTokens != 70 {
		t.Fatalf("token conservation broken: %+v", d)
	}
	if got := d.CostUSD; got < 1.799 || got > 1.801 {
		t.Fatalf("cost sum = %v, want 1.80", got)
	}
	if len(report.Tools) != 2 || report.Tools[0].Tool != "Bash" || report.Tools[0].Count != 2 {
		t.Fatalf("top tools wrong: %+v", report.Tools)
	}
}

func TestStatsWindowExcludesOlderThanCutoff(t *testing.T) {
	q := openTemp(t, Options{})
	now := time.Now().UTC()
	insertAcked(t, q, now.Add(-2*24*time.Hour),
		ackedEvent(now.Add(-2*24*time.Hour).Format(timeLayout), 10, 0, 0, "Bash"))
	insertAcked(t, q, now.Add(-20*24*time.Hour),
		ackedEvent(now.Add(-20*24*time.Hour).Format(timeLayout), 999, 0, 0, "Read"))

	report, err := q.StatsSince(now.Add(-7*24*time.Hour), DefaultTopTools)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, d := range report.Daily {
		total += d.InputTokens
	}
	if total != 10 {
		t.Fatalf("input tokens in window = %d, want 10 (older row excluded)", total)
	}
}

func TestStatsEmptyDB(t *testing.T) {
	q := openTemp(t, Options{})
	report, err := q.StatsSince(time.Now().Add(-7*24*time.Hour), DefaultTopTools)
	if err != nil {
		t.Fatalf("stats on empty db: %v", err)
	}
	if len(report.Daily) != 0 || len(report.Tools) != 0 {
		t.Fatalf("empty db should yield no rows: %+v", report)
	}
}

func TestOpenReadOnlyReadsAndRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.db")

	if _, err := OpenReadOnly(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing db error = %v, want os.ErrNotExist", err)
	}

	q, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format("2006-01-02") + "T09:00:00Z"
	insertAcked(t, q, time.Now(), ackedEvent(ts, 42, 8, 0.1, "Bash"))
	_ = q.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()
	report, err := ro.StatsSince(time.Now().Add(-7*24*time.Hour), DefaultTopTools)
	if err != nil {
		t.Fatalf("read-only stats: %v", err)
	}
	if len(report.Daily) != 1 || report.Daily[0].InputTokens != 42 {
		t.Fatalf("read-only stats wrong: %+v", report.Daily)
	}
}
