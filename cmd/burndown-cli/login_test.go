package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

const testKey = "yaahc_abcdefgh12345678901234567890123456"

// fakeBackend returns a register endpoint that accepts testKey and rejects
// anything else with 401.
func fakeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/v1/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Collector-Key") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "invalid collector key"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"collector_id": 123,
			"policy": map[string]any{
				"flush_interval_seconds": 30, "max_batch_size": 500,
				"refresh_cadence": "near-real-time",
			},
		})
	})
	return httptest.NewServer(mux)
}

func runCmd(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), err
}

func useLoginAPIURL(t *testing.T, apiURL string) {
	t.Helper()
	original := loginAPIURL
	loginAPIURL = apiURL
	t.Cleanup(func() { loginAPIURL = original })
}

func TestLoginDefaultsToCollectorEndpoint(t *testing.T) {
	cmd := newLoginCmd()
	if flag := cmd.Flags().Lookup("api-url"); flag != nil {
		t.Fatal("login must not expose an api-url override")
	}
	if loginAPIURL != config.DefaultAPIURL {
		t.Errorf("login endpoint = %q, want %q", loginAPIURL, config.DefaultAPIURL)
	}
}

// TestLoginHasNoPastedKeyOrEmailFlags proves the pasted-key/stdin path and
// the local email prompt are gone: device flow is the only way to log in,
// and the reporting email always comes from the server.
func TestLoginHasNoPastedKeyOrEmailFlags(t *testing.T) {
	cmd := newLoginCmd()
	for _, name := range []string{"key", "email", "device"} {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			t.Errorf("login must not expose --%s", name)
		}
	}
}

func TestRegisterRefreshesFromStoredConfig(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv := fakeBackend(t)
	defer srv.Close()

	store, _ := config.NewFileStore()
	if err := store.Save(&config.Config{
		APIURL: srv.URL, CollectorKey: testKey,
		UserEmail: "dev@example.com", Machine: "laptop-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, "register"); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg, _ := store.Load()
	if cfg.CollectorID != 123 {
		t.Errorf("collector_id not refreshed: %d", cfg.CollectorID)
	}
}

// deviceMock backs /api/device/{authorize,token} and /ingest/v1/register for
// the device-login tests: it returns authorization_pending for
// pollsBeforeResolve polls, then resolves per outcome.
type deviceMock struct {
	mu                 sync.Mutex
	pollsBeforeResolve int
	polls              int
	outcome            string // "approved" (default), "denied", "expired"
	issuedKey          string
	// userEmail is sent as the token response's user_email. omitUserEmail
	// simulates an older API that predates the field.
	userEmail     string
	omitUserEmail bool
}

func (m *deviceMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/device/authorize", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc1", "user_code": "ABCD-1234",
			"verification_uri":          "https://app.example/activate",
			"verification_uri_complete": "https://app.example/activate?code=ABCD-1234",
			"interval":                  1, "expires_in": 900,
		})
	})
	mux.HandleFunc("/api/device/token", m.handleToken)
	mux.HandleFunc("/ingest/v1/register", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		issuedKey := m.issuedKey
		m.mu.Unlock()
		if r.Header.Get("X-Collector-Key") != issuedKey {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "key_invalid"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"collector_id": 42,
			"policy": map[string]any{
				"flush_interval_seconds": 30, "max_batch_size": 500, "refresh_cadence": "near-real-time",
			},
			"key_expires_at": "2026-10-09T00:00:00Z",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (m *deviceMock) handleToken(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.polls++
	if m.polls <= m.pollsBeforeResolve {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		return
	}
	switch m.outcome {
	case "denied":
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	case "expired":
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "expired_token"})
	default:
		resp := map[string]any{
			"collector_key": m.issuedKey, "key_id": 1, "key_expires_at": "2026-10-09T00:00:00Z",
		}
		if !m.omitUserEmail {
			resp["user_email"] = m.userEmail
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// noOpenURL stubs the browser-open step so device-login tests never launch a
// real browser; it records the URL it was asked to open.
func noOpenURL(t *testing.T) *string {
	t.Helper()
	var got string
	orig := openURL
	openURL = func(url string) error { got = url; return nil }
	t.Cleanup(func() { openURL = orig })
	return &got
}

// noSleep makes pollDeviceToken's waits instant for tests.
func noSleep(t *testing.T) {
	t.Helper()
	orig := devicePollSleep
	devicePollSleep = func(time.Duration) {}
	t.Cleanup(func() { devicePollSleep = orig })
}

func TestDeviceLoginHappyPath(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	noSleep(t)
	openedURL := noOpenURL(t)
	mock := &deviceMock{
		pollsBeforeResolve: 2, issuedKey: "abd_devicekey123456", userEmail: "dev@example.com",
	}
	srv := mock.server(t)
	useLoginAPIURL(t, srv.URL)

	out, err := runCmd(t, "login", "--machine", "laptop-1")
	if err != nil {
		t.Fatalf("device login: %v", err)
	}
	if strings.Contains(out, mock.issuedKey) {
		t.Errorf("full key leaked in stdout: %q", out)
	}
	if *openedURL != "https://app.example/activate?code=ABCD-1234" {
		t.Errorf("opened URL = %q, want the verification_uri_complete", *openedURL)
	}

	store, _ := config.NewFileStore()
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if cfg.CollectorKey != mock.issuedKey {
		t.Errorf("collector_key = %q, want %q", cfg.CollectorKey, mock.issuedKey)
	}
	if cfg.CollectorID != 42 {
		t.Errorf("collector_id = %d, want 42", cfg.CollectorID)
	}
	if cfg.UserEmail != mock.userEmail {
		t.Errorf("user_email = %q, want %q (must come from the token response)", cfg.UserEmail, mock.userEmail)
	}
	if cfg.KeyExpiresAt != "2026-10-09T00:00:00Z" {
		t.Errorf("key_expires_at = %q, want 2026-10-09T00:00:00Z", cfg.KeyExpiresAt)
	}
	if mock.polls <= mock.pollsBeforeResolve {
		t.Errorf("polls = %d, want more than %d (pending then approved)", mock.polls, mock.pollsBeforeResolve)
	}
}

func TestDeviceLoginMissingUserEmailFails(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	noSleep(t)
	noOpenURL(t)
	mock := &deviceMock{issuedKey: "abd_devicekey123456", omitUserEmail: true}
	srv := mock.server(t)
	useLoginAPIURL(t, srv.URL)

	_, err := runCmd(t, "login", "--machine", "laptop-1")
	if err == nil || !strings.Contains(err.Error(), "reporting email") {
		t.Fatalf("error = %v, want a reporting-email hint", err)
	}
	store, _ := config.NewFileStore()
	if _, err := store.Load(); err == nil {
		t.Error("config should not be written when the token response has no user_email")
	}
}

func TestDeviceLoginDenied(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	noSleep(t)
	noOpenURL(t)
	mock := &deviceMock{outcome: "denied"}
	srv := mock.server(t)
	useLoginAPIURL(t, srv.URL)

	_, err := runCmd(t, "login", "--machine", "laptop-1")
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want a denial message", err)
	}
	store, _ := config.NewFileStore()
	if _, err := store.Load(); err == nil {
		t.Error("config should not be written when the device login is denied")
	}
}

func TestDeviceLoginExpired(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	noSleep(t)
	noOpenURL(t)
	mock := &deviceMock{outcome: "expired"}
	srv := mock.server(t)
	useLoginAPIURL(t, srv.URL)

	_, err := runCmd(t, "login", "--machine", "laptop-1")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v, want an expiry message", err)
	}
	store, _ := config.NewFileStore()
	if _, err := store.Load(); err == nil {
		t.Error("config should not be written when the device code expires")
	}
}

// TestDeviceLoginCtrlCSafe proves an already-cancelled context stops the poll
// loop promptly and leaves no config behind (device login never persists
// anything until every step, including register, succeeds).
func TestDeviceLoginCtrlCSafe(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	noSleep(t)
	noOpenURL(t)
	// Never resolves: the cancelled context must stop polling before it would.
	mock := &deviceMock{pollsBeforeResolve: 1 << 20}
	srv := mock.server(t)
	useLoginAPIURL(t, srv.URL)

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"login", "--machine", "laptop-1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: simulates Ctrl-C landing before/at the start of the poll
	err := root.ExecuteContext(ctx)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	store, _ := config.NewFileStore()
	if _, err := store.Load(); err == nil {
		t.Error("config should not be written when login is cancelled")
	}
}

func TestRegisterWithoutConfigFails(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	_, err := runCmd(t, "register")
	if err == nil {
		t.Fatal("expected error when no config is present")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error = %q, want a login hint", err.Error())
	}
}
