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

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/version"
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
	if report.Inventory == nil || report.Inventory.Enabled {
		t.Fatalf("inventory status missing or unexpectedly enabled: %+v", report.Inventory)
	}
	if report.Counters["received"] != 10 {
		t.Errorf("counters not surfaced: %+v", report.Counters)
	}
	// status --json telemetry must match what the heartbeat reports: both derive
	// from counters.Report over the same /healthz snapshot.
	if report.Telemetry == nil {
		t.Fatal("telemetry missing from status json")
	}
	want := counters.Report(report.Counters, version.Version)
	if *report.Telemetry != want {
		t.Errorf("telemetry %+v != Report(counters) %+v", *report.Telemetry, want)
	}
	if report.Telemetry.Received != 10 || report.Telemetry.QueueDepth != 3 {
		t.Errorf("telemetry values wrong: %+v", *report.Telemetry)
	}
}

func TestStatusSurfacesInventoryLifecycleWithoutValues(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	_ = store.Save(&config.Config{
		APIURL: "https://x", CollectorKey: testKey,
		Policy: api.Policy{InventoryEnabled: true}, InventoryStatus: "current",
		InventoryLastUploadAt: "2026-07-21T20:00:00Z", InventoryItemCount: 7,
	})
	srv, _ := fakeReceiver()
	defer srv.Close()
	out, err := runCmd(t, "status", "--json", "--port", strconv.Itoa(serverPort(t, srv)))
	if err != nil {
		t.Fatal(err)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Inventory == nil || !report.Inventory.Enabled ||
		report.Inventory.Status != "current" || report.Inventory.ItemCount != 7 {
		t.Fatalf("inventory = %+v", report.Inventory)
	}
	for _, forbidden := range []string{"deploy-check", "github", "skill_name"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("status leaked inventory value %q: %s", forbidden, out)
		}
	}
}

func TestStatusSurfacesRotationState(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	_ = store.Save(&config.Config{
		APIURL: "https://x", CollectorKey: testKey, Machine: "laptop-1",
		KeyExpiresAt: "2026-10-09T00:00:00Z", PendingKey: "abd_pending", RotationFailures: 2,
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
	if report.KeyExpiresAt != "2026-10-09T00:00:00Z" {
		t.Errorf("key_expires_at = %q", report.KeyExpiresAt)
	}
	if !report.RotationPending {
		t.Error("rotation_pending should be true when a PendingKey is set")
	}
	if report.RotationFailures != 2 {
		t.Errorf("rotation_failures = %d, want 2", report.RotationFailures)
	}
}

func TestStatusTextShowsRotationFailureWarning(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	_ = store.Save(&config.Config{
		APIURL: "https://x", CollectorKey: testKey, Machine: "laptop-1",
		KeyExpiresAt: "2020-01-01T00:00:00Z", RotationFailures: 3,
	})
	srv, _ := fakeReceiver()
	defer srv.Close()

	out, err := runCmd(t, "status", "--port", strconv.Itoa(serverPort(t, srv)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "EXPIRED") {
		t.Errorf("expected EXPIRED for a past key_expires_at, got: %q", out)
	}
	if !strings.Contains(out, "rotation failing: 3") {
		t.Errorf("expected a rotation-failure warning, got: %q", out)
	}
}

func TestStatusShowsDegradedAuthReason(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, _ := config.NewFileStore()
	_ = store.Save(&config.Config{
		APIURL: "https://x", CollectorKey: testKey, Machine: "laptop-1",
		AuthReason: "key_revoked",
	})
	srv, _ := fakeReceiver()
	defer srv.Close()

	out, err := runCmd(t, "status", "--port", strconv.Itoa(serverPort(t, srv)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "DEGRADED") || !strings.Contains(out, "key_revoked") {
		t.Errorf("expected a DEGRADED/key_revoked line, got: %q", out)
	}
	if !strings.Contains(out, "burndown-cli login") {
		t.Errorf("expected a login hint, got: %q", out)
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
