package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a client pointed at srv with instant (no-op) backoff so
// retry tests run fast.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClient(srv.URL, "yaahc_test_key")
	c.sleep = func(time.Duration) {}
	return c
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal body %q: %v", data, err)
	}
	return m
}

func TestRegisterHappyPath(t *testing.T) {
	var gotReq *http.Request
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		gotBody = decodeBody(t, r)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"collector_id":123,"policy":`+
			`{"flush_interval_seconds":30,"max_batch_size":500,"refresh_cadence":"near-real-time"},`+
			`"key_expires_at":"2026-10-09T00:00:00Z"}`)
	}))
	defer srv.Close()

	out, err := newTestClient(t, srv).Register(context.Background(), "laptop", "user@example.com")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if out.CollectorID != 123 {
		t.Errorf("collector_id = %d, want 123", out.CollectorID)
	}
	if out.Policy.MaxBatchSize != 500 {
		t.Errorf("policy.max_batch_size = %d, want 500", out.Policy.MaxBatchSize)
	}
	if out.KeyExpiresAt == nil || *out.KeyExpiresAt != "2026-10-09T00:00:00Z" {
		t.Errorf("key_expires_at = %v, want 2026-10-09T00:00:00Z", out.KeyExpiresAt)
	}
	if gotReq.Method != http.MethodPost || gotReq.URL.Path != "/ingest/v1/register" {
		t.Errorf("request = %s %s, want POST /ingest/v1/register", gotReq.Method, gotReq.URL.Path)
	}
	if got := gotReq.Header.Get("X-Collector-Key"); got != "yaahc_test_key" {
		t.Errorf("X-Collector-Key = %q, want yaahc_test_key", got)
	}
	if ua := gotReq.Header.Get("User-Agent"); !strings.HasPrefix(ua, "burndown-cli/") {
		t.Errorf("User-Agent = %q, want burndown-cli/ prefix", ua)
	}
	if gotBody["machine"] != "laptop" || gotBody["user_email"] != "user@example.com" {
		t.Errorf("body = %v, want machine/user_email set", gotBody)
	}
	if _, ok := gotBody["version"]; !ok {
		t.Errorf("body missing version key: %v", gotBody)
	}
}

func TestHeartbeatHappyPath(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"ok":true,"policy":{"max_batch_size":500},"key_expires_at":null}`)
	}))
	defer srv.Close()

	out, err := newTestClient(t, srv).Heartbeat(context.Background(), 123, nil)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !out.OK {
		t.Error("ok = false, want true")
	}
	if gotPath != "/ingest/v1/heartbeat" {
		t.Errorf("path = %q, want /ingest/v1/heartbeat", gotPath)
	}
	if gotBody["collector_id"] != float64(123) {
		t.Errorf("collector_id = %v, want 123", gotBody["collector_id"])
	}
	if out.KeyExpiresAt != nil {
		t.Errorf("key_expires_at = %v, want nil for a legacy/never-expiring key", out.KeyExpiresAt)
	}
}

func TestSendEventsHappyPath(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"accepted":2,"dropped":0}`)
	}))
	defer srv.Close()

	name := "api_request"
	events := []NormalizedEvent{{EventName: &name}, {}}
	out, err := newTestClient(t, srv).SendEvents(context.Background(), 123, events)
	if err != nil {
		t.Fatalf("SendEvents: %v", err)
	}
	if out.Accepted != 2 || out.Dropped != 0 {
		t.Errorf("counts = %+v, want accepted=2 dropped=0", out)
	}
	if gotBody["collector_id"] != float64(123) {
		t.Errorf("collector_id = %v, want 123", gotBody["collector_id"])
	}
	evs, ok := gotBody["events"].([]any)
	if !ok || len(evs) != 2 {
		t.Fatalf("events = %v, want 2 elements", gotBody["events"])
	}
	first, ok := evs[0].(map[string]any)
	if !ok {
		t.Fatalf("event[0] not an object: %v", evs[0])
	}
	if len(first) != len(allEventKeys) {
		t.Errorf("event[0] has %d keys, want %d (all keys emitted)", len(first), len(allEventKeys))
	}
}

// TestSendEventsCarriesEventID proves the event_id idempotency key set on a
// NormalizedEvent reaches the wire, and that a duplicates count in the
// response decodes onto Counts.
func TestSendEventsCarriesEventID(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"accepted":1,"dropped":0,"duplicates":1}`)
	}))
	defer srv.Close()

	id := "01977f2e-0000-7000-8000-000000000001"
	events := []NormalizedEvent{{EventID: &id}}
	out, err := newTestClient(t, srv).SendEvents(context.Background(), 1, events)
	if err != nil {
		t.Fatalf("SendEvents: %v", err)
	}
	if out.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", out.Duplicates)
	}
	evs, ok := gotBody["events"].([]any)
	if !ok || len(evs) != 1 {
		t.Fatalf("events = %v, want 1 element", gotBody["events"])
	}
	first := evs[0].(map[string]any)
	if first["event_id"] != id {
		t.Errorf("event_id = %v, want %q", first["event_id"], id)
	}
}

func TestAuth401NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"invalid collector key"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Heartbeat(context.Background(), 1, nil)
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("error = %v, want *AuthError", err)
	}
	if authErr.Detail != "invalid collector key" {
		t.Errorf("detail = %q, want %q", authErr.Detail, "invalid collector key")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("server called %d times, want 1 (no retry on 401)", n)
	}
}

// TestAuth401CodeParsing covers the standardized error contract
// ({"error": <code>}) the live backend now sends, plus the legacy
// {"detail": ...}-only shape for backward compatibility.
func TestAuth401CodeParsing(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantCode   string
		wantDetail string
	}{
		{"key_invalid", `{"error":"key_invalid"}`, CodeKeyInvalid, ""},
		{"key_revoked", `{"error":"key_revoked"}`, CodeKeyRevoked, ""},
		{"key_expired", `{"error":"key_expired"}`, CodeKeyExpired, ""},
		{"key_rotated", `{"error":"key_rotated"}`, CodeKeyRotated, ""},
		{"legacy_detail_only", `{"detail":"invalid collector key"}`, "", "invalid collector key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv).Heartbeat(context.Background(), 1, nil)
			var authErr *AuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("error = %v, want *AuthError", err)
			}
			if authErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", authErr.Code, tc.wantCode)
			}
			if authErr.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", authErr.Detail, tc.wantDetail)
			}
		})
	}
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"policy":{}}`)
	}))
	defer srv.Close()

	out, err := newTestClient(t, srv).Heartbeat(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !out.OK {
		t.Error("ok = false, want true after retry")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("server called %d times, want 2", n)
	}
}

func TestRetryExhaustionOn500(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Heartbeat(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	var authErr *AuthError
	if errors.As(err, &authErr) {
		t.Errorf("got AuthError for 5xx: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 4 {
		t.Errorf("server called %d times, want 4 (1 + 3 retries)", n)
	}
}

func TestNetworkErrorRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // force connection-refused on every attempt

	c := NewClient(url, "yaahc_test_key")
	c.sleep = func(time.Duration) {}
	_, err := c.Heartbeat(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
	var authErr *AuthError
	if errors.As(err, &authErr) {
		t.Errorf("got AuthError for network failure: %v", err)
	}
}

func TestSendEventsRejectsOversizedBatch(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	events := make([]NormalizedEvent, MaxIngestBatch+1)
	_, err := newTestClient(t, srv).SendEvents(context.Background(), 1, events)
	if err == nil {
		t.Fatal("expected error for oversized batch, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds max") {
		t.Errorf("error = %q, want mention of exceeding max", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("server called %d times, want 0 (rejected client-side)", n)
	}
}

// largeEventBatch returns n events padded well past gzipThreshold when
// marshaled, for tests exercising the gzip-encoding path.
func largeEventBatch(n int) []NormalizedEvent {
	events := make([]NormalizedEvent, n)
	for i := range events {
		name := fmt.Sprintf("api_request_%d_%s", i, strings.Repeat("x", 80))
		events[i] = NormalizedEvent{EventName: &name}
	}
	return events
}

func TestSendEventsLargeBatchGzipped(t *testing.T) {
	var gotEncoding string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Content-Encoding")
		reader := io.Reader(r.Body)
		if gotEncoding == "gzip" {
			gr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}
			defer func() { _ = gr.Close() }()
			reader = gr
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(data, &gotBody); err != nil {
			t.Fatalf("unmarshal body %q: %v", data, err)
		}
		_, _ = io.WriteString(w, `{"accepted":50,"dropped":0}`)
	}))
	defer srv.Close()

	events := largeEventBatch(50)
	out, err := newTestClient(t, srv).SendEvents(context.Background(), 1, events)
	if err != nil {
		t.Fatalf("SendEvents: %v", err)
	}
	if out.Accepted != 50 {
		t.Errorf("accepted = %d, want 50", out.Accepted)
	}
	if gotEncoding != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", gotEncoding)
	}
	evs, ok := gotBody["events"].([]any)
	if !ok || len(evs) != 50 {
		t.Fatalf("events = %v, want 50 elements", gotBody["events"])
	}
}

// TestGzipFallbackOnRejection covers the fallback path: a 400/415 response to
// a gzip-encoded request latches identity encoding for the rest of the
// session, without retrying that failed attempt.
func TestGzipFallbackOnRejection(t *testing.T) {
	var encodings []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		_, _ = io.Copy(io.Discard, r.Body)
		if len(encodings) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"detail":"malformed gzip body"}`)
			return
		}
		_, _ = io.WriteString(w, `{"accepted":50,"dropped":0}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	events := largeEventBatch(50)
	if _, err := c.SendEvents(context.Background(), 1, events); err == nil {
		t.Fatal("expected error on first (gzip-rejected) attempt, got nil")
	}
	if _, err := c.SendEvents(context.Background(), 1, events); err != nil {
		t.Fatalf("SendEvents (after fallback): %v", err)
	}
	if len(encodings) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(encodings))
	}
	if encodings[0] != "gzip" {
		t.Errorf("first request Content-Encoding = %q, want gzip", encodings[0])
	}
	if encodings[1] != "" {
		t.Errorf("second request Content-Encoding = %q, want empty (identity fallback)", encodings[1])
	}
}

// TestGzipNotDisabledOnUnrelatedBadRequest covers the negative case: a 400
// that has nothing to do with gzip (e.g. a validation error) must not latch
// off compression, since the body isn't the backend's decompression-middleware
// error shape.
func TestGzipNotDisabledOnUnrelatedBadRequest(t *testing.T) {
	var encodings []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"unrelated validation error"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	events := largeEventBatch(50)
	for range 2 {
		if _, err := c.SendEvents(context.Background(), 1, events); err == nil {
			t.Fatal("expected error for 400 response, got nil")
		}
	}
	if len(encodings) != 2 || encodings[0] != "gzip" || encodings[1] != "gzip" {
		t.Errorf("encodings = %v, want [gzip gzip] (unrelated 400 must not disable gzip)", encodings)
	}
}

func TestSetKeySwapsSubsequentRequests(t *testing.T) {
	var gotKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeys = append(gotKeys, r.Header.Get("X-Collector-Key"))
		_, _ = io.WriteString(w, `{"ok":true,"policy":{}}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.Heartbeat(context.Background(), 1, nil); err != nil {
		t.Fatalf("Heartbeat (before swap): %v", err)
	}
	c.SetKey("new_key")
	if got := c.Key(); got != "new_key" {
		t.Errorf("Key() = %q, want new_key", got)
	}
	if _, err := c.Heartbeat(context.Background(), 1, nil); err != nil {
		t.Fatalf("Heartbeat (after swap): %v", err)
	}
	if len(gotKeys) != 2 || gotKeys[0] != "yaahc_test_key" || gotKeys[1] != "new_key" {
		t.Errorf("keys seen by server = %v, want [yaahc_test_key new_key]", gotKeys)
	}
}

func TestHealthNoAuthHeader(t *testing.T) {
	var hadKey bool
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		hadKey = r.Header.Get("X-Collector-Key") != ""
		_, _ = io.WriteString(w, `{"ok":true,"uptime_seconds":10}`)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv).Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if gotPath != "/api/health" {
		t.Errorf("path = %q, want /api/health", gotPath)
	}
	if hadKey {
		t.Error("Health sent X-Collector-Key header, want none")
	}
}
