package uploader

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/queue"
	"github.com/agent-burn-down/desktop-client/internal/version"
)

// requestBody returns r.Body, transparently gunzipping it when the client set
// Content-Encoding: gzip, mirroring the real backend's decompression middleware.
// On a malformed gzip body it returns the original (unread) body's error state
// via an io.Reader that yields no bytes, same as a decode failure would.
func requestBody(r *http.Request) io.Reader {
	if r.Header.Get("Content-Encoding") != "gzip" {
		return r.Body
	}
	gr, err := gzip.NewReader(r.Body)
	if err != nil {
		return strings.NewReader("")
	}
	return gr
}

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

	// recordedMetrics counts accepted points by metric_name, mirroring
	// recorded for events.
	recordedMetrics  map[string]int
	metricsPosts     int
	recordedSessions map[string]int
	sessionPosts     int
	sessionsDown     bool

	// dedupeSeen tracks event_ids already accepted, mirroring the backend's
	// per-org dedupe window (yaah-hosted#24): a replayed event_id is counted as
	// a duplicate instead of being recorded again.
	dedupeSeen map[string]bool

	// dedupeSeenPoints mirrors dedupeSeen for metric points, keyed by point_id
	// (issue #55).
	dedupeSeenPoints map[string]bool

	// lastHeartbeat is the raw JSON body of the most recent heartbeat request,
	// so tests can assert the optional self-telemetry counters it carries.
	lastHeartbeat map[string]json.RawMessage

	// Rotation support. acceptedKeys nil means accept any key on
	// heartbeat/events (existing behavior); non-nil restricts to that set, so
	// tests can simulate an old key still valid during the overlap window vs.
	// rejected after it, and a pending key not yet accepted.
	acceptedKeys map[string]bool
	keyExpiresAt *string
	rotateOut    *api.RotateOut
	rotateStatus int // 0 = 200 with rotateOut; else write this status verbatim
	rotateCalls  int
	// transientFailKey, if non-empty, makes heartbeat return 500 (not 401) for
	// that specific key, simulating a network/server hiccup during
	// verification rather than an auth rejection.
	transientFailKey string
	// authRejectCode, if set, makes an acceptedKeys rejection on
	// heartbeat/events send {"error": authRejectCode} (the standardized 401
	// contract) instead of the legacy {"detail": ...} shape.
	authRejectCode string
}

// acceptsKey reports whether r's X-Collector-Key is acceptable, per
// mockBackend.acceptedKeys.
func acceptsKey(accepted map[string]bool, r *http.Request) bool {
	if accepted == nil {
		return true
	}
	return accepted[r.Header.Get("X-Collector-Key")]
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		recorded:         make(map[string]int),
		recordedMetrics:  make(map[string]int),
		recordedSessions: make(map[string]int),
		policy:           api.Policy{FlushIntervalSeconds: 30, MaxBatchSize: 50},
	}
}

func (m *mockBackend) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/v1/heartbeat", m.handleHeartbeat)
	mux.HandleFunc("/ingest/v1/events", m.handleEvents)
	mux.HandleFunc("/ingest/v1/metrics", m.handleMetrics)
	mux.HandleFunc("/ingest/v1/sessions", m.handleSessions)
	mux.HandleFunc("/ingest/v1/keys/rotate", m.handleRotate)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (m *mockBackend) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var body map[string]json.RawMessage
	_ = json.NewDecoder(requestBody(r)).Decode(&body)
	m.lastHeartbeat = body
	if m.transientFailKey != "" && r.Header.Get("X-Collector-Key") == m.transientFailKey {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if m.heartbeatFail || !acceptsKey(m.acceptedKeys, r) {
		m.writeAuthReject(w, "collector key revoked")
		return
	}
	_ = json.NewEncoder(w).Encode(api.HeartbeatOut{OK: true, Policy: m.policy, KeyExpiresAt: m.keyExpiresAt})
}

func (m *mockBackend) handleEvents(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventPosts++
	if m.eventsAuthFail || !acceptsKey(m.acceptedKeys, r) {
		m.writeAuthReject(w, "invalid collector key")
		return
	}
	if m.down {
		writeErr(w, http.StatusInternalServerError, "boom")
		return
	}
	var req struct {
		Events []api.NormalizedEvent `json:"events"`
	}
	_ = json.NewDecoder(requestBody(r)).Decode(&req)
	if m.dedupeSeen == nil {
		m.dedupeSeen = make(map[string]bool)
	}
	var accepted, duplicates int
	for _, ev := range req.Events {
		if ev.EventID != nil {
			if m.dedupeSeen[*ev.EventID] {
				duplicates++
				continue
			}
			m.dedupeSeen[*ev.EventID] = true
		}
		accepted++
		if ev.SessionID != nil {
			m.recorded[*ev.SessionID]++
		}
	}
	_ = json.NewEncoder(w).Encode(api.Counts{Accepted: accepted, Duplicates: duplicates})
}

func (m *mockBackend) handleMetrics(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metricsPosts++
	if m.eventsAuthFail || !acceptsKey(m.acceptedKeys, r) {
		m.writeAuthReject(w, "invalid collector key")
		return
	}
	if m.down {
		writeErr(w, http.StatusInternalServerError, "boom")
		return
	}
	var req struct {
		Points []api.MetricPoint `json:"points"`
	}
	_ = json.NewDecoder(requestBody(r)).Decode(&req)
	if m.dedupeSeenPoints == nil {
		m.dedupeSeenPoints = make(map[string]bool)
	}
	var accepted, duplicates int
	for _, p := range req.Points {
		if p.PointID != nil {
			if m.dedupeSeenPoints[*p.PointID] {
				duplicates++
				continue
			}
			m.dedupeSeenPoints[*p.PointID] = true
		}
		accepted++
		m.recordedMetrics[p.MetricName]++
	}
	_ = json.NewEncoder(w).Encode(api.Counts{Accepted: accepted, Duplicates: duplicates})
}

func (m *mockBackend) handleSessions(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionPosts++
	if m.eventsAuthFail || !acceptsKey(m.acceptedKeys, r) {
		m.writeAuthReject(w, "invalid collector key")
		return
	}
	if m.sessionsDown {
		writeErr(w, http.StatusInternalServerError, "boom")
		return
	}
	var req struct {
		Sessions []api.SessionSummary `json:"sessions"`
	}
	_ = json.NewDecoder(requestBody(r)).Decode(&req)
	for _, summary := range req.Sessions {
		m.recordedSessions[summary.SessionID]++
	}
	_ = json.NewEncoder(w).Encode(api.Counts{Accepted: len(req.Sessions)})
}

func (m *mockBackend) handleRotate(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rotateCalls++
	if !acceptsKey(m.acceptedKeys, r) {
		writeErr(w, http.StatusUnauthorized, "invalid collector key")
		return
	}
	if m.rotateStatus == http.StatusConflict {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"detail": "key already rotated"})
		return
	}
	if m.rotateStatus != 0 {
		w.WriteHeader(m.rotateStatus)
		return
	}
	_ = json.NewEncoder(w).Encode(m.rotateOut)
}

func writeErr(w http.ResponseWriter, code int, detail string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

// writeAuthReject writes a 401 using the standardized {"error": <code>} shape
// when authRejectCode is set, else falls back to the legacy {"detail": ...}
// body existing tests rely on.
func (m *mockBackend) writeAuthReject(w http.ResponseWriter, legacyDetail string) {
	if m.authRejectCode != "" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": m.authRejectCode})
		return
	}
	writeErr(w, http.StatusUnauthorized, legacyDetail)
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

// samplePoints returns n metric points suitable for enqueueing in tests.
func samplePoints(n int) []api.MetricPoint {
	points := make([]api.MetricPoint, n)
	for i := range points {
		points[i] = api.MetricPoint{MetricName: "claude_code.commit.count", Value: float64(i + 1)}
	}
	return points
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

// TestFlushMetricsOnceDrainsQueueToBackend proves metric points enqueued via
// EnqueueMetrics reach the backend through the same flush/ack machinery as
// events (issue #21).
func TestFlushMetricsOnceDrainsQueueToBackend(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)

	points := []api.MetricPoint{
		{MetricName: "claude_code.commit.count", Value: 1},
		{MetricName: "claude_code.commit.count", Value: 1},
		{MetricName: "claude_code.cost.usage", Value: 0.05},
	}
	if err := q.EnqueueMetrics(points); err != nil {
		t.Fatal(err)
	}
	up.FlushMetricsOnce(context.Background())

	depth, err := q.LeaseMetricsBatch(10, leaseDuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(depth) != 0 {
		t.Fatalf("metrics queue not drained: %d rows still leasable", len(depth))
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.recordedMetrics["claude_code.commit.count"] != 2 {
		t.Errorf("commit.count delivered = %d, want 2", mock.recordedMetrics["claude_code.commit.count"])
	}
	if mock.recordedMetrics["claude_code.cost.usage"] != 1 {
		t.Errorf("cost.usage delivered = %d, want 1", mock.recordedMetrics["claude_code.cost.usage"])
	}
	if mock.metricsPosts != 1 {
		t.Errorf("metrics posts = %d, want 1 (one batch)", mock.metricsPosts)
	}
}

func TestFlushSessionsRetriesDurablyThenAcks(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)
	session, model := "session-1", "gpt-5.6-sol"
	input := int64(10)
	if _, err := q.UpsertSessionEvents([]api.NormalizedEvent{{
		SessionID: &session, Model: &model, InputTokens: &input,
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	mock.mu.Lock()
	mock.sessionsDown = true
	mock.mu.Unlock()
	up.FlushSessionsOnce(context.Background())
	mock.mu.Lock()
	mock.sessionsDown = false
	mock.mu.Unlock()
	up.FlushSessionsOnce(context.Background())

	remaining, err := q.LeaseSessionsBatch(10, leaseDuration)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining sessions = %d, %v", len(remaining), err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if mock.recordedSessions[session] != 1 || mock.sessionPosts < 2 {
		t.Fatalf("recorded/posts = %#v/%d", mock.recordedSessions, mock.sessionPosts)
	}
}

// TestReplayDedupedByEventID proves a crash-after-send-before-ack replay is not
// double-counted: the queue never re-mints event_id across leases (issue #20),
// so when a delivered batch never gets acked and is later re-leased and
// resent, the backend's event_id dedupe (simulated by mockBackend) recognizes
// the replay and neither the accepted count nor the recorded events double.
func TestReplayDedupedByEventID(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)
	ctx := context.Background()

	const total = 10
	if err := q.Enqueue(markedEvents(total)); err != nil {
		t.Fatal(err)
	}

	items, err := q.LeaseBatch(total, leaseDuration)
	if err != nil || len(items) != total {
		t.Fatalf("LeaseBatch: %v (got %d items)", err, len(items))
	}
	ids, events := split(items)
	counts, err := up.client.SendEvents(ctx, up.collectorID, events)
	if err != nil {
		t.Fatalf("first SendEvents: %v", err)
	}
	if counts.Accepted != total || counts.Duplicates != 0 {
		t.Fatalf("first send counts = %+v, want accepted=%d duplicates=0", counts, total)
	}

	// Simulate a crash after the backend recorded the batch but before the
	// queue Ack landed: the row never acks, so it becomes leasable again.
	if err := q.Nack(ids); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	replayItems, err := q.LeaseBatch(total, leaseDuration)
	if err != nil || len(replayItems) != total {
		t.Fatalf("replay LeaseBatch: %v (got %d items)", err, len(replayItems))
	}
	_, replayEvents := split(replayItems)
	replayCounts, err := up.client.SendEvents(ctx, up.collectorID, replayEvents)
	if err != nil {
		t.Fatalf("replay SendEvents: %v", err)
	}
	if replayCounts.Accepted != 0 || replayCounts.Duplicates != total {
		t.Fatalf("replay counts = %+v, want accepted=0 duplicates=%d", replayCounts, total)
	}

	unique, dupes := mock.uniqueDelivered()
	if unique != total {
		t.Fatalf("delivered %d unique events, want %d", unique, total)
	}
	if dupes != 0 {
		t.Fatalf("%d events recorded more than once; replay must not double-count", dupes)
	}
}

// TestReplayDedupedByPointID proves a crash-after-send-before-ack replay of a
// metrics batch is not double-counted, mirroring TestReplayDedupedByEventID
// (issue #55): the queue never re-mints point_id across leases, so when a
// delivered batch never gets acked and is later re-leased and resent, the
// backend's point_id dedupe (simulated by mockBackend) recognizes the replay.
func TestReplayDedupedByPointID(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)
	ctx := context.Background()

	const total = 10
	if err := q.EnqueueMetrics(samplePoints(total)); err != nil {
		t.Fatal(err)
	}

	items, err := q.LeaseMetricsBatch(total, leaseDuration)
	if err != nil || len(items) != total {
		t.Fatalf("LeaseMetricsBatch: %v (got %d items)", err, len(items))
	}
	ids, points := splitMetrics(items)
	counts, err := up.client.SendMetrics(ctx, up.collectorID, points)
	if err != nil {
		t.Fatalf("first SendMetrics: %v", err)
	}
	if counts.Accepted != total || counts.Duplicates != 0 {
		t.Fatalf("first send counts = %+v, want accepted=%d duplicates=0", counts, total)
	}

	// Simulate a crash after the backend recorded the batch but before the
	// queue AckMetrics landed: the row never acks, so it becomes leasable again.
	if err := q.NackMetrics(ids); err != nil {
		t.Fatalf("NackMetrics: %v", err)
	}

	replayItems, err := q.LeaseMetricsBatch(total, leaseDuration)
	if err != nil || len(replayItems) != total {
		t.Fatalf("replay LeaseMetricsBatch: %v (got %d items)", err, len(replayItems))
	}
	_, replayPoints := splitMetrics(replayItems)
	replayCounts, err := up.client.SendMetrics(ctx, up.collectorID, replayPoints)
	if err != nil {
		t.Fatalf("replay SendMetrics: %v", err)
	}
	if replayCounts.Accepted != 0 || replayCounts.Duplicates != total {
		t.Fatalf("replay counts = %+v, want accepted=0 duplicates=%d", replayCounts, total)
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

// TestDegradedNoRetryStormAndKeepsQueueing proves a key_revoked/key_invalid
// 401 stops uploads entirely (zero POSTs beyond the initial rejection) while
// new events keep enqueueing, and the cycle cadence slows to the probe
// interval.
func TestDegradedNoRetryStormAndKeepsQueueing(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{} // nothing accepted
	mock.authRejectCode = api.CodeKeyRevoked
	srv := mock.server(t)
	up, q, reg := testDeps(t, mock, srv.URL)

	_ = q.Enqueue(markedEvents(3))
	up.HeartbeatOnce(context.Background())
	if up.state() != stateDegraded {
		t.Fatal("expected Degraded after a key_revoked heartbeat")
	}
	if reg.Get(counters.AuthFailed) != 1 {
		t.Fatal("auth_failed counter should be set")
	}

	for i := 0; i < 5; i++ {
		up.FlushOnce(context.Background())
	}
	mock.mu.Lock()
	posts := mock.eventPosts
	mock.mu.Unlock()
	if posts != 0 {
		t.Fatalf("event POSTs while degraded = %d, want 0 (no retry storm)", posts)
	}

	_ = q.Enqueue(markedEvents(2))
	if d, _ := q.Depth(); d != 5 {
		t.Fatalf("depth = %d, want 5 (events preserved and still accepted while degraded)", d)
	}
	if got := up.cycleDelay(); got != degradedProbeInterval {
		t.Fatalf("cycleDelay while degraded = %v, want the slow probe interval %v",
			got, degradedProbeInterval)
	}
}

// TestReLoginRecoversWithoutRestartAndDrainsBacklog proves an out-of-band
// `burndown-cli login` (rewriting config's key while the daemon keeps
// running) is picked up by the next Degraded probe cycle, without a restart,
// and the queued backlog drains immediately after.
func TestReLoginRecoversWithoutRestartAndDrainsBacklog(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{}
	mock.authRejectCode = api.CodeKeyRevoked
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	up.store = store

	_ = q.Enqueue(markedEvents(3))
	up.runCycle(context.Background()) // heartbeat 401s -> Degraded; flush/rotate skipped
	if up.state() != stateDegraded {
		t.Fatal("expected Degraded")
	}
	mock.mu.Lock()
	posts := mock.eventPosts
	mock.mu.Unlock()
	if posts != 0 {
		t.Fatalf("flush should have been skipped entirely while degraded; event posts = %d", posts)
	}

	// Simulate `burndown-cli login` writing a fresh key while the daemon runs.
	mock.mu.Lock()
	mock.acceptedKeys["abd_fresh"] = true
	mock.mu.Unlock()
	store.saved.CollectorKey = "abd_fresh"

	up.runCycle(context.Background()) // reloads the key, probes, recovers, drains
	if up.state() != stateActive {
		t.Fatal("expected Active after re-login")
	}
	if got := up.client.Key(); got != "abd_fresh" {
		t.Fatalf("client key = %q, want abd_fresh", got)
	}
	if d, _ := q.Depth(); d != 0 {
		t.Fatalf("backlog not drained after recovery: depth %d", d)
	}
}

// TestKeyRotatedSwapsToPendingKey proves a key_rotated 401 on the old key
// adopts a cached pending key (from a rotation this instance already started)
// without needing a re-login.
func TestKeyRotatedSwapsToPendingKey(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{"abd_newkey": true} // old key now dead
	mock.authRejectCode = api.CodeKeyRotated
	srv := mock.server(t)
	up, _, _ := testDeps(t, mock, srv.URL) // client starts on "yaahc_test"
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	store.saved.PendingKey = "abd_newkey"
	store.saved.PendingKeyID = 5
	store.saved.PendingKeyExpires = "2027-01-01T00:00:00Z"
	up.store = store

	up.HeartbeatOnce(context.Background())

	if up.state() != stateActive {
		t.Fatal("expected Active after adopting the cached pending key")
	}
	if got := up.client.Key(); got != "abd_newkey" {
		t.Fatalf("client key = %q, want abd_newkey", got)
	}
	if store.saved.CollectorKey != "abd_newkey" || store.saved.PendingKey != "" {
		t.Fatalf("config not committed: %+v", store.saved)
	}
}

// TestKeyRotatedWithoutPendingDegrades proves a key_rotated 401 with no
// cached pending key (e.g. another instance rotated it) degrades, since there
// is no way to recover the new key without a fresh login.
func TestKeyRotatedWithoutPendingDegrades(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{}
	mock.authRejectCode = api.CodeKeyRotated
	srv := mock.server(t)
	up, _, _ := testDeps(t, mock, srv.URL)
	store := newMemStore() // no PendingKey
	store.saved.CollectorKey = "yaahc_test"
	up.store = store

	up.HeartbeatOnce(context.Background())

	if up.state() != stateDegraded {
		t.Fatal("expected Degraded when key_rotated has no pending key to adopt")
	}
}

// TestKeyExpiredRotateFailsDegrades proves the realistic case: an already-
// expired key cannot authenticate the rotate call either (the backend
// requires a currently-valid key to rotate), so the one immediate rotate
// attempt fails and the daemon degrades. #17's proactive T-7d rotation is
// what normally prevents this path from firing at all.
func TestKeyExpiredRotateFailsDegrades(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{} // the expired key can't rotate either
	mock.authRejectCode = api.CodeKeyExpired
	srv := mock.server(t)
	up, _, _ := testDeps(t, mock, srv.URL)
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	up.store = store

	up.HeartbeatOnce(context.Background())

	if up.state() != stateDegraded {
		t.Fatal("expected Degraded when the expired key cannot rotate either")
	}
	if mock.rotateCalls != 1 {
		t.Fatalf("rotate calls = %d, want exactly 1 (a single immediate attempt)", mock.rotateCalls)
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

// testRotationNow is the fixed "current time" every rotation test anchors to,
// so cases can express expiry/last-attempt timestamps as offsets from it.
func testRotationNow(t *testing.T) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse fixed test time: %v", err)
	}
	return tm
}

// TestRotationDueGating unit-tests the T-7-day trigger and the once/day gate
// without needing a client or store.
func TestRotationDueGating(t *testing.T) {
	up := &Uploader{now: func() time.Time { return testRotationNow(t) }}
	cases := []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"far out", config.Config{KeyExpiresAt: "2026-06-01T00:00:00Z"}, false},
		{"never expires (empty)", config.Config{}, false},
		{"within lead time, never attempted", config.Config{KeyExpiresAt: "2026-01-05T00:00:00Z"}, true},
		{
			"within lead time, attempted today",
			config.Config{KeyExpiresAt: "2026-01-05T00:00:00Z", LastRotationAt: "2026-01-01T00:00:00Z"},
			false,
		},
		{
			"within lead time, last attempt over a day ago",
			config.Config{KeyExpiresAt: "2026-01-05T00:00:00Z", LastRotationAt: "2025-12-30T00:00:00Z"},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := up.rotationDue(&tc.cfg); got != tc.want {
				t.Errorf("rotationDue(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

// TestRotationNearExpiryCommitsAndDrainsQueue proves the full happy path:
// rotate -> pending persisted -> verify succeeds -> commit, with zero events
// lost across the rotation.
func TestRotationNearExpiryCommitsAndDrainsQueue(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{"yaahc_test": true}
	const newExpiry = "2027-01-01T00:00:00Z"
	mock.rotateOut = &api.RotateOut{
		CollectorKey: "abd_newkey", KeyID: 99,
		KeyExpiresAt: newExpiry, OldKeyValidUntil: "2026-01-02T00:00:00Z",
	}
	srv := mock.server(t)
	up, q, _ := testDeps(t, mock, srv.URL)
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	store.saved.KeyExpiresAt = "2026-01-05T00:00:00Z" // within the 7d lead time
	up.store = store
	up.now = func() time.Time { return testRotationNow(t) }

	_ = q.Enqueue(markedEvents(3))

	// Cycle 1: rotate is attempted; the new key is persisted as pending, not
	// yet live.
	up.RotateCheckOnce(context.Background())
	if mock.rotateCalls != 1 {
		t.Fatalf("rotate calls = %d, want 1", mock.rotateCalls)
	}
	if store.saved.PendingKey != "abd_newkey" || store.saved.CollectorKey != "yaahc_test" {
		t.Fatalf("expected pending=abd_newkey, current=yaahc_test; got %+v", store.saved)
	}

	// The backend now accepts the new key too (its overlap window).
	mock.mu.Lock()
	mock.acceptedKeys["abd_newkey"] = true
	mock.mu.Unlock()

	// Cycle 2: verification succeeds and commits.
	up.RotateCheckOnce(context.Background())
	if store.saved.CollectorKey != "abd_newkey" {
		t.Fatalf("collector_key = %q, want abd_newkey", store.saved.CollectorKey)
	}
	if store.saved.PendingKey != "" {
		t.Fatalf("pending key not cleared: %q", store.saved.PendingKey)
	}
	if store.saved.KeyExpiresAt != newExpiry {
		t.Fatalf("key_expires_at = %q, want %q", store.saved.KeyExpiresAt, newExpiry)
	}

	// Zero event loss: the queue still drains cleanly after rotation.
	up.FlushOnce(context.Background())
	if d, _ := q.Depth(); d != 0 {
		t.Fatalf("queue not drained after rotation: depth %d", d)
	}
	unique, dupes := mock.uniqueDelivered()
	if unique != 3 || dupes != 0 {
		t.Fatalf("delivered unique=%d dupes=%d, want 3/0", unique, dupes)
	}
}

// TestRotationVerifyTransientFailureKeepsPending proves a network/server
// hiccup during verification leaves the old key live and the pending key
// retained for another attempt next cycle.
func TestRotationVerifyTransientFailureKeepsPending(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{"yaahc_test": true}
	mock.transientFailKey = "abd_newkey"
	mock.rotateOut = &api.RotateOut{
		CollectorKey: "abd_newkey", KeyID: 99,
		KeyExpiresAt: "2027-01-01T00:00:00Z", OldKeyValidUntil: "2026-01-02T00:00:00Z",
	}
	srv := mock.server(t)
	up, _, _ := testDeps(t, mock, srv.URL)
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	store.saved.KeyExpiresAt = "2026-01-05T00:00:00Z"
	up.store = store
	up.now = func() time.Time { return testRotationNow(t) }

	up.RotateCheckOnce(context.Background()) // attemptRotate: persists pending
	up.RotateCheckOnce(context.Background()) // verifyPendingKey: transient failure

	if store.saved.PendingKey != "abd_newkey" {
		t.Fatalf("pending key should be retained after a transient failure: %+v", store.saved)
	}
	if store.saved.CollectorKey != "yaahc_test" {
		t.Fatalf("collector key should still be the old key: %+v", store.saved)
	}
	if got := up.client.Key(); got != "yaahc_test" {
		t.Fatalf("live client key = %q, want reverted to yaahc_test", got)
	}
}

// TestRotationVerifyRejectedDiscardsPending proves an actual auth rejection of
// the pending key (not just a transient failure) discards it so the next
// rotationDue check starts fresh, rather than retrying a dead key forever.
func TestRotationVerifyRejectedDiscardsPending(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{"yaahc_test": true} // abd_bad is never accepted
	mock.rotateOut = &api.RotateOut{
		CollectorKey: "abd_bad", KeyID: 99,
		KeyExpiresAt: "2027-01-01T00:00:00Z", OldKeyValidUntil: "2026-01-02T00:00:00Z",
	}
	srv := mock.server(t)
	up, _, _ := testDeps(t, mock, srv.URL)
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	store.saved.KeyExpiresAt = "2026-01-05T00:00:00Z"
	up.store = store
	up.now = func() time.Time { return testRotationNow(t) }

	up.RotateCheckOnce(context.Background())
	up.RotateCheckOnce(context.Background())

	if store.saved.PendingKey != "" {
		t.Fatalf("pending key should be discarded after rejection: %+v", store.saved)
	}
	if store.saved.RotationFailures != 1 {
		t.Fatalf("rotation_failures = %d, want 1", store.saved.RotationFailures)
	}
	if got := up.client.Key(); got != "yaahc_test" {
		t.Fatalf("live client key = %q, want reverted to yaahc_test", got)
	}
}

// TestRotationRestartResumesVerification proves a fresh Uploader instance
// (simulating a restart) that never called RotateKey itself still resumes and
// completes verification purely from PendingKey persisted in config.
func TestRotationRestartResumesVerification(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{"yaahc_test": true, "abd_newkey": true}
	srv := mock.server(t)

	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	store.saved.KeyExpiresAt = "2026-01-05T00:00:00Z"
	store.saved.PendingKey = "abd_newkey"
	store.saved.PendingKeyID = 99
	store.saved.PendingKeyExpires = "2027-01-01T00:00:00Z"
	store.saved.OldKeyValidUntil = "2026-01-02T00:00:00Z"

	up, _, _ := testDeps(t, mock, srv.URL)
	up.store = store
	up.now = func() time.Time { return testRotationNow(t) }

	up.RotateCheckOnce(context.Background())

	if mock.rotateCalls != 0 {
		t.Fatalf("rotate should not be re-called when resuming from a pending key; calls = %d", mock.rotateCalls)
	}
	if store.saved.CollectorKey != "abd_newkey" {
		t.Fatalf("collector_key = %q, want abd_newkey (resumed verification should commit)", store.saved.CollectorKey)
	}
	if store.saved.PendingKey != "" {
		t.Fatalf("pending key not cleared: %q", store.saved.PendingKey)
	}
}

// TestRotationConflictNoCrashNoKeyChange proves a 409 (another instance
// already rotated) is handled gracefully: no panic, no key change, and it
// does not count as a rotation failure (it isn't one).
func TestRotationConflictNoCrashNoKeyChange(t *testing.T) {
	mock := newMockBackend()
	mock.acceptedKeys = map[string]bool{"yaahc_test": true}
	mock.rotateStatus = http.StatusConflict
	srv := mock.server(t)
	up, _, _ := testDeps(t, mock, srv.URL)
	store := newMemStore()
	store.saved.CollectorKey = "yaahc_test"
	store.saved.KeyExpiresAt = "2026-01-05T00:00:00Z"
	up.store = store
	up.now = func() time.Time { return testRotationNow(t) }

	up.RotateCheckOnce(context.Background())

	if store.saved.CollectorKey != "yaahc_test" {
		t.Fatalf("collector key should be unchanged after a 409: %+v", store.saved)
	}
	if store.saved.PendingKey != "" {
		t.Fatalf("pending key should not be set after a 409: %+v", store.saved)
	}
	if store.saved.RotationFailures != 0 {
		t.Fatalf("rotation_failures should not increment on a 409 conflict: %d", store.saved.RotationFailures)
	}
	if store.saved.LastRotationAt == "" {
		t.Fatal("last_rotation_at should be set even on conflict, to back off until tomorrow")
	}
}

// TestHeartbeatCarriesSelfTelemetry proves the heartbeat request body includes
// the optional "counters" object and that its values equal counters.Report over
// the same registry snapshot and live queue depth that /healthz reports, i.e.
// `status --json` and the heartbeat are a single source of truth.
func TestHeartbeatCarriesSelfTelemetry(t *testing.T) {
	mock := newMockBackend()
	srv := mock.server(t)
	up, q, reg := testDeps(t, mock, srv.URL)

	reg.Set(counters.Received, 40)
	reg.Set(counters.Filtered, 6)
	reg.Set(counters.Uploaded, 30)
	reg.Set(counters.UploadDropped, 2)
	reg.Set(counters.Errors, 1)
	reg.Set(counters.UploadErrors, 3)
	reg.Set(counters.HeartbeatErrors, 1)
	if err := q.Enqueue(markedEvents(5)); err != nil {
		t.Fatal(err)
	}

	up.HeartbeatOnce(context.Background())

	mock.mu.Lock()
	raw, ok := mock.lastHeartbeat["counters"]
	mock.mu.Unlock()
	if !ok {
		t.Fatalf("heartbeat body missing counters object: %v", mock.lastHeartbeat)
	}
	var got counters.Telemetry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode telemetry: %v", err)
	}

	depth, _ := q.Depth()
	snap := reg.Snapshot()
	snap[counters.QueueDepth] = depth
	want := counters.Report(snap, version.Version)
	if got != want {
		t.Fatalf("heartbeat telemetry %+v != /healthz-derived %+v", got, want)
	}
	// Spot-check the mapping the acceptance cares about.
	if got.Received != 40 || got.Filtered != 6 || got.Uploaded != 30 {
		t.Fatalf("pipeline counters wrong: %+v", got)
	}
	if got.Dropped != 2 || got.Errors != 5 || got.QueueDepth != 5 {
		t.Fatalf("dropped/errors/depth wrong: %+v", got)
	}
}
