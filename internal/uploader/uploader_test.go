package uploader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/queue"
)

// mockBackend is an httptest handler standing in for the ingest API. It records
// each uploaded event by SessionID so tests can prove exactly-once delivery.
type mockBackend struct {
	mu             sync.Mutex
	recorded       map[string]int
	eventPosts     int
	policy         api.Policy
	down           bool // events return 500 (unreachable / timeout path)
	eventsAuthFail bool // events return 401
	heartbeatFail  bool // heartbeat returns 401
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		recorded: make(map[string]int),
		policy:   api.Policy{FlushIntervalSeconds: 30, MaxBatchSize: 50},
	}
}

func (m *mockBackend) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/v1/heartbeat", m.handleHeartbeat)
	mux.HandleFunc("/ingest/v1/events", m.handleEvents)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (m *mockBackend) handleHeartbeat(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.heartbeatFail {
		writeErr(w, http.StatusUnauthorized, "collector key revoked")
		return
	}
	_ = json.NewEncoder(w).Encode(api.HeartbeatOut{OK: true, Policy: m.policy})
}

func (m *mockBackend) handleEvents(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventPosts++
	if m.eventsAuthFail {
		writeErr(w, http.StatusUnauthorized, "invalid collector key")
		return
	}
	if m.down {
		writeErr(w, http.StatusInternalServerError, "boom")
		return
	}
	var req struct {
		Events []api.NormalizedEvent `json:"events"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	for _, ev := range req.Events {
		if ev.SessionID != nil {
			m.recorded[*ev.SessionID]++
		}
	}
	_ = json.NewEncoder(w).Encode(api.Counts{Accepted: len(req.Events)})
}

func writeErr(w http.ResponseWriter, code int, detail string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

func (m *mockBackend) uniqueDelivered() (unique, dupes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.recorded {
		unique++
		if n > 1 {
			dupes++
		}
	}
	return unique, dupes
}

// markedEvents returns n events each carrying a unique SessionID marker so the
// mock backend can detect duplicate delivery.
func markedEvents(n int) []api.NormalizedEvent {
	out := make([]api.NormalizedEvent, n)
	for i := range out {
		id := fmt.Sprintf("evt-%d", i)
		name := "sse_event"
		out[i] = api.NormalizedEvent{EventName: &name, SessionID: &id}
	}
	return out
}

func testDeps(t *testing.T, mock *mockBackend, baseURL string) (*Uploader, *queue.Queue, *counters.Registry) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "q.db"), queue.Options{})
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })
	reg := counters.New()
	client := api.NewClient(baseURL, "yaahc_test", api.WithSleep(func(time.Duration) {}))
	up := New(Config{
		Client: client, Queue: q, Counters: reg,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		CollectorID: 1, Policy: mock.policy,
	})
	return up, q, reg
}

// TestZeroLossNoDuplicates injects 500s across cycles and proves every event is
// eventually delivered to the mock exactly once.
func TestZeroLossNoDuplicates(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)

	const total = 250
	if err := q.Enqueue(markedEvents(total)); err != nil {
		t.Fatal(err)
	}
	// Backend down for the first few cycles: batches nack and requeue.
	mock.mu.Lock()
	mock.down = true
	mock.mu.Unlock()
	for i := 0; i < 3; i++ {
		up.FlushOnce(context.Background())
	}
	if depth, _ := q.Depth(); depth != total {
		t.Fatalf("depth while down = %d, want %d (nothing acked)", depth, total)
	}
	mock.mu.Lock()
	mock.down = false
	mock.mu.Unlock()
	// Drain to completion.
	for i := 0; i < 20; i++ {
		up.FlushOnce(context.Background())
		if d, _ := q.Depth(); d == 0 {
			break
		}
	}
	if d, _ := q.Depth(); d != 0 {
		t.Fatalf("queue not drained: depth %d", d)
	}
	unique, dupes := mock.uniqueDelivered()
	if unique != total {
		t.Fatalf("delivered %d unique events, want %d", unique, total)
	}
	if dupes != 0 {
		t.Fatalf("%d events delivered more than once; expected exactly-once", dupes)
	}
}

// TestPolicyChangeAppliedWithinOneCycle proves a heartbeat swaps the live policy
// without a restart, so the next cycle uses the new flush interval.
func TestPolicyChangeAppliedWithinOneCycle(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	store := newMemStore()
	up, _, _ := testDeps(t, mock, srv.URL)
	up.store = store

	if got := up.flushDelay(); got != 30*time.Second {
		t.Fatalf("initial flush delay = %v, want 30s", got)
	}
	mock.mu.Lock()
	mock.policy = api.Policy{FlushIntervalSeconds: 1, MaxBatchSize: 500}
	mock.mu.Unlock()

	up.HeartbeatOnce(context.Background())

	if got := up.flushDelay(); got != time.Second {
		t.Fatalf("flush delay after heartbeat = %v, want 1s (live policy swap)", got)
	}
	if store.saved == nil || store.saved.Policy.FlushIntervalSeconds != 1 {
		t.Fatal("refreshed policy was not persisted to the config store")
	}
}

// TestOfflineBoundedAttemptsNoStorm proves an unreachable backend does not spin:
// one cycle issues a bounded number of attempts, the queue is preserved, and it
// grows while offline.
func TestOfflineBoundedAttemptsNoStorm(t *testing.T) {
	mock := newMockBackend()
	mock.down = true
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)

	_ = q.Enqueue(markedEvents(10))
	up.FlushOnce(context.Background())

	mock.mu.Lock()
	posts := mock.eventPosts
	mock.mu.Unlock()
	// One leased batch × up to 4 client attempts (1 send + 3 retries).
	if posts == 0 || posts > 4 {
		t.Fatalf("event POSTs = %d, want 1..4 (bounded, no storm)", posts)
	}
	if depth, _ := q.Depth(); depth != 10 {
		t.Fatalf("depth = %d, want 10 (nothing lost)", depth)
	}
	// Queue keeps growing while offline.
	_ = q.Enqueue(markedEvents(5))
	if depth, _ := q.Depth(); depth != 15 {
		t.Fatalf("depth after more enqueue = %d, want 15", depth)
	}
}

// TestAuthFailurePausesUntilHeartbeat proves a 401 stops flushing until a
// heartbeat re-authenticates.
func TestAuthFailurePausesUntilHeartbeat(t *testing.T) {
	mock := newMockBackend()
	mock.eventsAuthFail = true
	srv := mock.server(t)
	up, q, reg := testDeps(t, mock, srv.URL)

	_ = q.Enqueue(markedEvents(4))
	up.FlushOnce(context.Background())
	if reg.Get(counters.AuthFailed) != 1 {
		t.Fatal("auth_failed counter should be set after 401")
	}
	mock.mu.Lock()
	postsAfterFirst := mock.eventPosts
	mock.mu.Unlock()

	// While paused, FlushOnce must not touch the backend.
	up.FlushOnce(context.Background())
	mock.mu.Lock()
	if mock.eventPosts != postsAfterFirst {
		t.Fatalf("flush issued POSTs while paused (%d -> %d)", postsAfterFirst, mock.eventPosts)
	}
	mock.eventsAuthFail = false
	mock.mu.Unlock()

	// A successful heartbeat re-enables flushing.
	up.HeartbeatOnce(context.Background())
	if reg.Get(counters.AuthFailed) != 0 {
		t.Fatal("auth_failed should clear after successful heartbeat")
	}
	up.FlushOnce(context.Background())
	if depth, _ := q.Depth(); depth != 0 {
		t.Fatalf("depth after re-auth = %d, want 0", depth)
	}
}

// TestRunAppliesPolicyLive runs the loop with a fast policy and confirms events
// upload without a restart.
func TestRunAppliesPolicyLive(t *testing.T) {
	mock := newMockBackend()
	mock.policy = api.Policy{FlushIntervalSeconds: 1, MaxBatchSize: 500}
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)
	up.policy = mock.policy

	_ = q.Enqueue(markedEvents(5))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); up.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for {
		if d, _ := q.Depth(); d == 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("events not uploaded by running loop")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// memStore is an in-memory config.Store for policy-persist assertions.
type memStore struct {
	mu    sync.Mutex
	saved *config.Config
}

func newMemStore() *memStore { return &memStore{saved: &config.Config{}} }

func (m *memStore) Load() (*config.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *m.saved
	return &cp, nil
}

func (m *memStore) Save(c *config.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *c
	m.saved = &cp
	return nil
}
