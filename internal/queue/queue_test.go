package queue

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

func openTemp(t *testing.T, opts Options) *Queue {
	t.Helper()
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func sampleEvents(n int) []api.NormalizedEvent {
	events := make([]api.NormalizedEvent, n)
	for i := range events {
		name := "sse_event"
		events[i] = api.NormalizedEvent{EventName: &name}
	}
	return events
}

func TestEnqueueLeaseAckRoundTrip(t *testing.T) {
	q := openTemp(t, Options{})
	if err := q.Enqueue(sampleEvents(3)); err != nil {
		t.Fatal(err)
	}
	depth, _ := q.Depth()
	if depth != 3 {
		t.Fatalf("depth = %d, want 3", depth)
	}
	items, err := q.LeaseBatch(10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("leased %d, want 3", len(items))
	}
	for _, it := range items {
		if it.EventID == "" || it.Event.EventName == nil || *it.Event.EventName != "sse_event" {
			t.Fatalf("bad item: %+v", it)
		}
	}
	// Leased rows are not re-leased while the lease is valid.
	again, _ := q.LeaseBatch(10, time.Minute)
	if len(again) != 0 {
		t.Fatalf("expected no re-lease of valid leases, got %d", len(again))
	}
	if err := q.Ack(idsOf(items)); err != nil {
		t.Fatal(err)
	}
	depth, _ = q.Depth()
	if depth != 0 {
		t.Fatalf("depth after ack = %d, want 0", depth)
	}
	s, _ := q.Stats()
	if s.Acked != 3 {
		t.Fatalf("acked stat = %d, want 3", s.Acked)
	}
}

func TestNackRequeuesAndCountsAttempts(t *testing.T) {
	q := openTemp(t, Options{})
	_ = q.Enqueue(sampleEvents(1))
	items, _ := q.LeaseBatch(1, time.Minute)
	if err := q.Nack(idsOf(items)); err != nil {
		t.Fatal(err)
	}
	relesed, _ := q.LeaseBatch(1, time.Minute)
	if len(relesed) != 1 {
		t.Fatalf("nacked row should be leasable, got %d", len(relesed))
	}
	if relesed[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", relesed[0].Attempts)
	}
}

func TestExpiredLeaseReLeasedAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(sampleEvents(2))
	// Lease with an already-expired duration to simulate a crash mid-upload.
	items, _ := q.LeaseBatch(2, -time.Second)
	if len(items) != 2 {
		t.Fatalf("leased %d, want 2", len(items))
	}
	// Ack one; the other stays leased-but-expired.
	_ = q.Ack(idsOf(items[:1]))
	_ = q.Close()

	// Reopen (simulated restart) and confirm the expired lease is re-leasable
	// and the acked row is never returned.
	q2, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q2.Close() }()
	reLeased, _ := q2.LeaseBatch(10, time.Minute)
	if len(reLeased) != 1 {
		t.Fatalf("re-leased %d, want 1 (expired lease only)", len(reLeased))
	}
	if reLeased[0].ID == items[0].ID {
		t.Fatal("acked row must never be re-leased")
	}
}

func TestEvictionRespectsRowCap(t *testing.T) {
	q := openTemp(t, Options{MaxRows: 10})
	for i := 0; i < 5; i++ {
		if err := q.Enqueue(sampleEvents(5)); err != nil {
			t.Fatal(err)
		}
	}
	count, _ := q.scalar("SELECT COUNT(*) FROM queue")
	if count > 10 {
		t.Fatalf("row count %d exceeds cap 10", count)
	}
	s, _ := q.Stats()
	if s.Evicted == 0 {
		t.Fatal("expected eviction counter > 0")
	}
}

func TestEvictionRespectsByteCap(t *testing.T) {
	// Tiny byte cap forces eviction regardless of row count.
	q := openTemp(t, Options{MaxBytes: 32 * 1024})
	for i := 0; i < 40; i++ {
		if err := q.Enqueue(sampleEvents(50)); err != nil {
			t.Fatal(err)
		}
	}
	size, _ := q.sizeBytes()
	// Allow one chunk of slack: eviction runs after each enqueue commit.
	if size > 32*1024+evictChunk*512 {
		t.Fatalf("db size %d far exceeds byte cap", size)
	}
	s, _ := q.Stats()
	if s.Evicted == 0 {
		t.Fatal("expected eviction under byte cap")
	}
}

func TestCheckHealthy(t *testing.T) {
	q := openTemp(t, Options{})
	_ = q.Enqueue(sampleEvents(2))
	if err := q.Check(); err != nil {
		t.Fatalf("healthy db failed integrity check: %v", err)
	}
}

func TestConcurrentEnqueueLease(t *testing.T) {
	q := openTemp(t, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = q.Enqueue(sampleEvents(3))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				items, _ := q.LeaseBatch(5, time.Minute)
				_ = q.Ack(idsOf(items))
			}
		}()
	}
	wg.Wait()
	if err := q.Check(); err != nil {
		t.Fatalf("post-concurrency integrity: %v", err)
	}
}

func idsOf(items []Item) []int64 {
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}
