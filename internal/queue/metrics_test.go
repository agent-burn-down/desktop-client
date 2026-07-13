package queue

import (
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func samplePoints(n int) []api.MetricPoint {
	points := make([]api.MetricPoint, n)
	for i := range points {
		points[i] = api.MetricPoint{MetricName: "claude_code.commit.count", Value: float64(i + 1)}
	}
	return points
}

func metricIDsOf(items []MetricItem) []int64 {
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func TestEnqueueLeaseAckMetricsRoundTrip(t *testing.T) {
	q := openTemp(t, Options{})
	if err := q.EnqueueMetrics(samplePoints(3)); err != nil {
		t.Fatal(err)
	}
	items, err := q.LeaseMetricsBatch(10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("leased %d, want 3", len(items))
	}
	for _, it := range items {
		if it.Point.MetricName != "claude_code.commit.count" {
			t.Fatalf("bad item: %+v", it)
		}
	}
	// Leased rows are not re-leased while the lease is valid.
	again, _ := q.LeaseMetricsBatch(10, time.Minute)
	if len(again) != 0 {
		t.Fatalf("expected no re-lease of valid leases, got %d", len(again))
	}
	if err := q.AckMetrics(metricIDsOf(items)); err != nil {
		t.Fatal(err)
	}
	// Acked rows are never re-leased.
	afterAck, _ := q.LeaseMetricsBatch(10, time.Minute)
	if len(afterAck) != 0 {
		t.Fatalf("acked rows re-leased, got %d", len(afterAck))
	}
}

func TestNackRequeuesMetricsAndCountsAttempts(t *testing.T) {
	q := openTemp(t, Options{})
	_ = q.EnqueueMetrics(samplePoints(1))
	items, _ := q.LeaseMetricsBatch(1, time.Minute)
	if err := q.NackMetrics(metricIDsOf(items)); err != nil {
		t.Fatal(err)
	}
	released, _ := q.LeaseMetricsBatch(1, time.Minute)
	if len(released) != 1 {
		t.Fatalf("nacked row should be leasable, got %d", len(released))
	}
	if released[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", released[0].Attempts)
	}
}

func TestPruneAckedMetrics(t *testing.T) {
	q := openTemp(t, Options{})
	_ = q.EnqueueMetrics(samplePoints(2))
	items, _ := q.LeaseMetricsBatch(2, time.Minute)
	_ = q.AckMetrics(metricIDsOf(items))

	deleted, err := q.PruneAckedMetrics(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	// A cutoff before acked_at prunes nothing.
	_ = q.EnqueueMetrics(samplePoints(1))
	items2, _ := q.LeaseMetricsBatch(1, time.Minute)
	_ = q.AckMetrics(metricIDsOf(items2))
	deleted2, err := q.PruneAckedMetrics(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted2 != 0 {
		t.Fatalf("deleted2 = %d, want 0 (acked_at is after the cutoff)", deleted2)
	}
}

// TestMetricsEvictionRespectsRowCap proves metrics_queue is bounded the same
// way the events queue is (issue #21 security-audit finding: without this,
// metrics_queue grows unbounded while the uploader is offline/degraded).
func TestMetricsEvictionRespectsRowCap(t *testing.T) {
	q := openTemp(t, Options{MaxRows: 10})
	for i := 0; i < 5; i++ {
		if err := q.EnqueueMetrics(samplePoints(5)); err != nil {
			t.Fatal(err)
		}
	}
	count, _ := q.scalar("SELECT COUNT(*) FROM metrics_queue")
	if count > 10 {
		t.Fatalf("metrics_queue row count %d exceeds cap 10", count)
	}
}

// TestMetricsEnqueueDoesNotEvictEvents proves eviction triggered by metrics
// growth only trims metrics_queue, never the unrelated events queue (the bug
// the shared whole-file byte cap could otherwise cause).
func TestMetricsEnqueueDoesNotEvictEvents(t *testing.T) {
	q := openTemp(t, Options{MaxRows: 10})
	if err := q.Enqueue(sampleEvents(3)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := q.EnqueueMetrics(samplePoints(5)); err != nil {
			t.Fatal(err)
		}
	}
	eventCount, _ := q.scalar("SELECT COUNT(*) FROM queue")
	if eventCount != 3 {
		t.Fatalf("events queue count = %d, want 3 (metrics eviction must not touch events)", eventCount)
	}
	metricsCount, _ := q.scalar("SELECT COUNT(*) FROM metrics_queue")
	if metricsCount > 10 {
		t.Fatalf("metrics_queue row count %d exceeds cap 10", metricsCount)
	}
}

func TestEventsAndMetricsQueuesAreIndependent(t *testing.T) {
	q := openTemp(t, Options{})
	if err := q.Enqueue(sampleEvents(2)); err != nil {
		t.Fatal(err)
	}
	if err := q.EnqueueMetrics(samplePoints(3)); err != nil {
		t.Fatal(err)
	}
	eventItems, err := q.LeaseBatch(10, time.Minute)
	if err != nil || len(eventItems) != 2 {
		t.Fatalf("LeaseBatch: %v (got %d items, want 2)", err, len(eventItems))
	}
	metricItems, err := q.LeaseMetricsBatch(10, time.Minute)
	if err != nil || len(metricItems) != 3 {
		t.Fatalf("LeaseMetricsBatch: %v (got %d items, want 3)", err, len(metricItems))
	}
	if err := q.Ack(idsOf(eventItems)); err != nil {
		t.Fatal(err)
	}
	// Acking events must not touch the metrics queue.
	stillLeased, err := q.LeaseMetricsBatch(10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillLeased) != 0 {
		t.Fatalf("metrics rows re-leased after acking unrelated events, got %d", len(stillLeased))
	}
}
