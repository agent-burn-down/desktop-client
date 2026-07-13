package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/queue"
)

// backendMock stands in for the ingest API and records delivered events.
type backendMock struct {
	mu       sync.Mutex
	events   []api.NormalizedEvent
	metrics  []api.MetricPoint
	policy   api.Policy
	downEvts bool
}

func (b *backendMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/v1/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.HeartbeatOut{OK: true, Policy: b.policy})
	})
	mux.HandleFunc("/ingest/v1/events", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.downEvts {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Events []api.NormalizedEvent `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.events = append(b.events, req.Events...)
		_ = json.NewEncoder(w).Encode(api.Counts{Accepted: len(req.Events)})
	})
	mux.HandleFunc("/ingest/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		var req struct {
			Points []api.MetricPoint `json:"points"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.metrics = append(b.metrics, req.Points...)
		_ = json.NewEncoder(w).Encode(api.Counts{Accepted: len(req.Points)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (b *backendMock) delivered() []api.NormalizedEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]api.NormalizedEvent(nil), b.events...)
}

func (b *backendMock) deliveredMetrics() []api.MetricPoint {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]api.MetricPoint(nil), b.metrics...)
}

// otlpFixture is a realistic OTLP/HTTP logs payload: one api_request, one
// tool_use, and one noise record (no event name) that must be dropped.
func otlpFixture() map[string]any {
	attr := func(k string, v map[string]any) map[string]any {
		return map[string]any{"key": k, "value": v}
	}
	s := func(x string) map[string]any { return map[string]any{"stringValue": x} }
	iv := func(x string) map[string]any { return map[string]any{"intValue": x} }
	dv := func(x float64) map[string]any { return map[string]any{"doubleValue": x} }
	bv := func(x bool) map[string]any { return map[string]any{"boolValue": x} }
	apiReq := map[string]any{"attributes": []any{
		attr("event.name", s("api_request")),
		attr("session.id", s("sess-1")),
		attr("model", s("claude-3-5")),
		attr("input_tokens", iv("1200")),
		attr("output_tokens", iv("340")),
		attr("cost_usd", dv(0.0123)),
		attr("repo", s("myrepo")),
	}}
	toolUse := map[string]any{"attributes": []any{
		attr("event.name", s("tool_use")),
		attr("tool_name", s("bash")),
		attr("success", bv(true)),
		attr("duration_ms", dv(450.5)),
	}}
	noise := map[string]any{"attributes": []any{}}
	return map[string]any{"resourceLogs": []any{map[string]any{
		"scopeLogs": []any{map[string]any{
			"logRecords": []any{apiReq, toolUse, noise},
		}},
	}}}
}

// otlpMetricsFixture is a realistic OTLP/HTTP metrics payload: two
// allowlisted Claude Code counters (commit.count, cost.usage) and one
// unrecognized metric name that must be counted and dropped, never uploaded.
func otlpMetricsFixture() map[string]any {
	dataPoint := func(v any) map[string]any {
		return map[string]any{"asInt": v}
	}
	return map[string]any{"resourceMetrics": []any{map[string]any{
		"scopeMetrics": []any{map[string]any{
			"metrics": []any{
				map[string]any{
					"name": "claude_code.commit.count",
					"sum":  map[string]any{"dataPoints": []any{dataPoint("1")}},
				},
				map[string]any{
					"name": "claude_code.cost.usage",
					"gauge": map[string]any{
						"dataPoints": []any{map[string]any{"asDouble": "0.05"}},
					},
				},
				map[string]any{
					"name": "some.unrecognized.metric",
					"sum":  map[string]any{"dataPoints": []any{dataPoint("999")}},
				},
			},
		}},
	}}}
}

func postMetrics(t *testing.T, addr string, payload map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post("http://"+addr+"/v1/metrics", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post metrics: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post metrics status = %d, want 200", resp.StatusCode)
	}
}

func newTestDaemon(t *testing.T, mock *backendMock, backendURL string) *Daemon {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)
	cfg := &config.Config{
		APIURL: backendURL, CollectorKey: "yaahc_test", CollectorID: 1,
		Policy: mock.policy,
	}
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	d, err := New(Options{Config: cfg, Store: store, Port: freePort(t)})
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return d
}

// freePort returns a currently-unused loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func postLogs(t *testing.T, addr string, payload map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	resp, err := http.Post("http://"+addr+"/v1/logs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post logs: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post logs status = %d, want 200", resp.StatusCode)
	}
}

// TestEndToEndPipeline posts the OTLP fixture through a real receiver and proves
// the normalized events reach the mock backend with correct field values, that
// /healthz exposes the counters, and that shutdown leaves a consistent queue.
func TestEndToEndPipeline(t *testing.T) {
	mock := &backendMock{policy: api.Policy{FlushIntervalSeconds: 1, MaxBatchSize: 500}}
	srv := mock.server(t)
	d := newTestDaemon(t, mock, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	addr := d.ReceiverAddr()
	postLogs(t, addr, otlpFixture())

	waitFor(t, 5*time.Second, func() bool { return len(mock.delivered()) >= 2 })
	assertFieldValues(t, mock.delivered())
	assertHealthz(t, addr)

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

// TestEndToEndMetricsPipeline posts an OTLP metrics fixture through a real
// receiver and proves the allowlisted points reach the mock backend while the
// unrecognized metric name is counted and dropped, never uploaded raw
// (issue #21 acceptance criteria).
func TestEndToEndMetricsPipeline(t *testing.T) {
	mock := &backendMock{policy: api.Policy{FlushIntervalSeconds: 1, MaxBatchSize: 500}}
	srv := mock.server(t)
	d := newTestDaemon(t, mock, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()

	addr := d.ReceiverAddr()
	postMetrics(t, addr, otlpMetricsFixture())

	waitFor(t, 5*time.Second, func() bool { return len(mock.deliveredMetrics()) >= 2 })
	delivered := mock.deliveredMetrics()
	if len(delivered) != 2 {
		t.Fatalf("delivered %d points, want exactly 2 (the unrecognized metric must never upload)", len(delivered))
	}
	var sawCommit, sawCost bool
	for _, p := range delivered {
		switch p.MetricName {
		case "claude_code.commit.count":
			sawCommit = true
			if p.Value != 1 {
				t.Errorf("commit.count value = %v, want 1", p.Value)
			}
		case "claude_code.cost.usage":
			sawCost = true
			if p.Value != 0.05 {
				t.Errorf("cost.usage value = %v, want 0.05", p.Value)
			}
		case "some.unrecognized.metric":
			t.Fatal("unrecognized metric name reached the backend raw")
		}
	}
	if !sawCommit || !sawCost {
		t.Fatalf("missing expected allowlisted points, got %+v", delivered)
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Counters["metrics_normalized"] != 3 {
		t.Errorf("metrics_normalized = %d, want 3", out.Counters["metrics_normalized"])
	}
	if out.Counters["metrics_filtered"] != 1 {
		t.Errorf("metrics_filtered = %d, want 1 (the unrecognized metric)", out.Counters["metrics_filtered"])
	}
	if out.Counters["metrics_queued"] != 2 {
		t.Errorf("metrics_queued = %d, want 2", out.Counters["metrics_queued"])
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func assertFieldValues(t *testing.T, events []api.NormalizedEvent) {
	t.Helper()
	var apiReq, toolUse *api.NormalizedEvent
	for i := range events {
		switch derefStr(events[i].EventName) {
		case "api_request":
			apiReq = &events[i]
		case "tool_use":
			toolUse = &events[i]
		}
	}
	if apiReq == nil || toolUse == nil {
		t.Fatalf("missing expected events; got %d records", len(events))
	}
	if derefI64(apiReq.InputTokens) != 1200 || derefI64(apiReq.OutputTokens) != 340 {
		t.Fatalf("api_request tokens wrong: in=%v out=%v", apiReq.InputTokens, apiReq.OutputTokens)
	}
	if derefStr(apiReq.Model) != "claude-3-5" || derefStr(apiReq.Repo) != "myrepo" {
		t.Fatalf("api_request model/repo wrong: %+v", apiReq)
	}
	if derefStr(toolUse.ToolName) != "bash" || toolUse.ToolSuccess == nil || !*toolUse.ToolSuccess {
		t.Fatalf("tool_use fields wrong: %+v", toolUse)
	}
}

func assertHealthz(t *testing.T, addr string) {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK       bool             `json:"ok"`
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatal("healthz ok = false")
	}
	for _, key := range []string{"received", "normalized", "queued", "queue_depth"} {
		if _, present := out.Counters[key]; !present {
			t.Fatalf("healthz counters missing %q: %v", key, out.Counters)
		}
	}
	if out.Counters["normalized"] < 2 {
		t.Fatalf("normalized counter = %d, want >= 2", out.Counters["normalized"])
	}
}

// TestDrainLeavesConsistentQueue drives the shutdown path directly with the
// backend down, then reopens the queue to prove integrity and no stuck leases.
func TestDrainLeavesConsistentQueue(t *testing.T) {
	mock := &backendMock{policy: api.Policy{FlushIntervalSeconds: 30}, downEvts: true}
	srv := mock.server(t)
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)
	cfg := &config.Config{APIURL: srv.URL, CollectorKey: "yaahc_test", CollectorID: 1, Policy: mock.policy}
	store, _ := config.NewFileStore()
	_ = store.Save(cfg)
	d, err := New(Options{Config: cfg, Store: store, Port: freePort(t)})
	if err != nil {
		t.Fatal(err)
	}

	if err := d.queue.Enqueue(sampleEvents(5)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); d.uploader.Run(ctx) }()
	cancel()
	if err := d.drain(&wg); err != nil {
		t.Fatalf("drain: %v", err)
	}

	reopened, err := queue.Open(filepath.Join(dir, "queue.db"), queue.Options{})
	if err != nil {
		t.Fatalf("reopen queue: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Check(); err != nil {
		t.Fatalf("integrity check after drain: %v", err)
	}
	depth, _ := reopened.Depth()
	if depth != 5 {
		t.Fatalf("depth after drain = %d, want 5 (backend down, nothing acked, nothing lost)", depth)
	}
	// No stuck leases: every row is immediately re-leasable.
	items, _ := reopened.LeaseBatch(10, time.Minute)
	if len(items) != 5 {
		t.Fatalf("re-leasable rows = %d, want 5 (no stuck leases)", len(items))
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met within deadline")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func sampleEvents(n int) []api.NormalizedEvent {
	out := make([]api.NormalizedEvent, n)
	for i := range out {
		name := "sse_event"
		out[i] = api.NormalizedEvent{EventName: &name}
	}
	return out
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// TestRetentionWiring proves the daemon takes its prune window from config and
// that pruneOnce leaves recent acked rows intact (deletion correctness itself is
// covered by the queue package's prune and soak tests).
func TestRetentionWiring(t *testing.T) {
	mock := &backendMock{policy: api.Policy{FlushIntervalSeconds: 30}}
	srv := mock.server(t)
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)
	cfg := &config.Config{
		APIURL: srv.URL, CollectorKey: "yaahc_test", CollectorID: 1,
		Policy: mock.policy, RetentionDays: 3,
	}
	store, _ := config.NewFileStore()
	_ = store.Save(cfg)
	d, err := New(Options{Config: cfg, Store: store, Port: freePort(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.close() }()

	if d.retentionDays != 3 {
		t.Fatalf("retentionDays = %d, want 3 (from config)", d.retentionDays)
	}
	if err := d.queue.Enqueue(sampleEvents(4)); err != nil {
		t.Fatal(err)
	}
	items, _ := d.queue.LeaseBatch(4, time.Minute)
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	if err := d.queue.Ack(ids); err != nil {
		t.Fatal(err)
	}
	d.pruneOnce() // recent acked rows are inside the window: nothing removed
	s, _ := d.queue.Stats()
	if s.Acked != 4 {
		t.Fatalf("acked after prune = %d, want 4 (recent rows kept)", s.Acked)
	}
}
