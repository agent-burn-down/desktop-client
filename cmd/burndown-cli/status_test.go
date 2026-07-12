package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// serverPort extracts the numeric port an httptest server is bound to.
func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("atoi: %v", err)
	}
	return port
}

// fakeReceiver serves /healthz and /v1/logs like the daemon's receiver. Posting
// to /v1/logs advances the queued counter so send-test can observe the delta.
func fakeReceiver() (*httptest.Server, *int64) {
	var queued int64
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"counters": map[string]int64{
				"received": 10, "filtered": 2, "uploaded": 5,
				"queue_depth": 3, "last_upload_at": 1_700_000_000,
				"queued": atomic.LoadInt64(&queued),
			},
		})
	})
	mux.HandleFunc("/v1/logs", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&queued, 1)
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": 1, "dropped": 0})
	})
	return httptest.NewServer(mux), &queued
}

func TestStatusDaemonUpJSON(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	_ = store.Save(&config.Config{
		APIURL: "https://x", CollectorKey: testKey,
		Machine: "laptop-1", CollectorID: 123,
	})
	srv, _ := fakeReceiver()
	defer srv.Close()

	out, err := runCmd(t, "status", "--json", "--port", strconv.Itoa(serverPort(t, srv)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode status json: %v\n%s", err, out)
	}
	if !report.DaemonUp {
		t.Error("daemon_up should be true")
	}
	if report.KeyPrefix != keyPrefix(testKey) {
		t.Errorf("key_prefix = %q, want %q", report.KeyPrefix, keyPrefix(testKey))
	}
	if strings.Contains(out, testKey) {
		t.Error("full key leaked in status json")
	}
	if report.CollectorID != 123 {
		t.Errorf("collector_id = %d", report.CollectorID)
	}
	if report.Counters["received"] != 10 {
		t.Errorf("counters not surfaced: %+v", report.Counters)
	}
}

func TestStatusDaemonDownText(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	_ = store.Save(&config.Config{APIURL: "https://x", CollectorKey: testKey, Machine: "m"})

	// Bind then release a port so nothing is listening on it.
	srv, _ := fakeReceiver()
	port := serverPort(t, srv)
	srv.Close()

	out, err := runCmd(t, "status", "--port", strconv.Itoa(port))
	if err != nil {
		t.Fatalf("status (down): %v", err)
	}
	if !strings.Contains(out, "not running") {
		t.Errorf("expected 'not running', got: %q", out)
	}
	if !strings.Contains(out, keyPrefix(testKey)) {
		t.Errorf("config-derived key prefix should still show when down: %q", out)
	}
}
