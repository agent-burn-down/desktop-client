package counters

import (
	"sync"
	"testing"
)

func TestAddSetGetSnapshot(t *testing.T) {
	r := New()
	r.Add(Received, 3)
	r.Add(Received, 2)
	r.Set(QueueDepth, 7)
	if got := r.Get(Received); got != 5 {
		t.Fatalf("Received = %d, want 5", got)
	}
	if got := r.Get(QueueDepth); got != 7 {
		t.Fatalf("QueueDepth = %d, want 7", got)
	}
	if got := r.Get("missing"); got != 0 {
		t.Fatalf("missing = %d, want 0", got)
	}
	snap := r.Snapshot()
	snap[Received] = 999 // mutating the copy must not affect the registry
	if r.Get(Received) != 5 {
		t.Fatal("Snapshot must return an independent copy")
	}
}

func TestConcurrentAdd(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Add(Uploaded, 1)
			}
		}()
	}
	wg.Wait()
	if got := r.Get(Uploaded); got != 5000 {
		t.Fatalf("Uploaded = %d, want 5000", got)
	}
}
