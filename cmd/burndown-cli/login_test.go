package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestLoginPersistsCredentialsPrefixOnly(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv := fakeBackend(t)
	defer srv.Close()

	out, err := runCmd(t,
		"login", "--key", testKey, "--email", "dev@example.com",
		"--machine", "laptop-1", "--api-url", srv.URL)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if strings.Contains(out, testKey) {
		t.Errorf("full key leaked in output: %q", out)
	}
	if !strings.Contains(out, keyPrefix(testKey)) {
		t.Errorf("key prefix missing from output: %q", out)
	}

	store, _ := config.NewFileStore()
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load persisted config: %v", err)
	}
	if cfg.CollectorKey != testKey {
		t.Errorf("collector_key = %q, want %q", cfg.CollectorKey, testKey)
	}
	if cfg.CollectorID != 123 {
		t.Errorf("collector_id = %d, want 123", cfg.CollectorID)
	}
	if cfg.UserEmail != "dev@example.com" || cfg.Machine != "laptop-1" {
		t.Errorf("identity not persisted: %+v", cfg)
	}
	if cfg.Policy.MaxBatchSize != 500 {
		t.Errorf("policy not persisted: %+v", cfg.Policy)
	}
}

func TestLoginBadKeyExitsWithMessage(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv := fakeBackend(t)
	defer srv.Close()

	_, err := runCmd(t,
		"login", "--key", "yaahc_wrongkey", "--email", "dev@example.com",
		"--machine", "laptop-1", "--api-url", srv.URL)
	if err == nil {
		t.Fatal("expected a non-nil error for a rejected key")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %q, want a 'rejected' hint", err.Error())
	}
	store, _ := config.NewFileStore()
	if _, err := store.Load(); err == nil {
		t.Error("config should not be written when login fails")
	}
}

func TestLoginKeyFromStdin(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv := fakeBackend(t)
	defer srv.Close()

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetIn(strings.NewReader(testKey + "\n"))
	root.SetArgs([]string{
		"login", "--email", "dev@example.com", "--machine", "laptop-1", "--api-url", srv.URL,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("login via stdin: %v", err)
	}
	store, _ := config.NewFileStore()
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.CollectorKey != testKey {
		t.Errorf("key from stdin not persisted: %q", cfg.CollectorKey)
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
