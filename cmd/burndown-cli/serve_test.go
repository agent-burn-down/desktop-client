package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
)

func TestServeCommandRegistered(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "serve" {
			return
		}
	}
	t.Fatal("serve subcommand not registered on root")
}

func TestLoadServeConfigMissing(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadServeConfig(store)
	if err == nil || !strings.Contains(err.Error(), "burndown-cli login") {
		t.Fatalf("missing-config error = %v, want a login hint", err)
	}
}

func TestLoadServeConfigIncomplete(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	// Registered API URL but no collector id yet: not serve-ready.
	if err := store.Save(&config.Config{APIURL: "https://x", CollectorKey: "yaahc_k"}); err != nil {
		t.Fatal(err)
	}
	_, err = loadServeConfig(store)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete-config error = %v, want an 'incomplete' hint", err)
	}
}

// TestServeAttributesRealisticCodexOTel proves the production runServe path,
// without a static repository override, resolves conversation.id from local
// Codex 0.144.4-style session metadata before the event reaches the backend.
func TestServeAttributesRealisticCodexOTel(t *testing.T) {
	const conversationID = "019f6448-f7ea-72a0-a1c1-afaf3eabbad0"
	codexHome := t.TempDir()
	repo := filepath.Join(t.TempDir(), "serve-path-repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(codexHome, "sessions", "2026", "07", "14",
		"rollout-2026-07-14T22-38-51-"+conversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", conversationID, repo)
	if err := os.WriteFile(sessionPath, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	var (
		mu        sync.Mutex
		delivered []api.NormalizedEvent
		wireBody  string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/v1/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HeartbeatOut{
			OK: true, Policy: api.Policy{FlushIntervalSeconds: 1, MaxBatchSize: 100},
		})
	})
	mux.HandleFunc("/ingest/v1/events", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Events []api.NormalizedEvent `json:"events"`
		}
		var raw bytes.Buffer
		body := http.MaxBytesReader(w, r.Body, 1024*1024)
		if _, err := raw.ReadFrom(body); err != nil {
			t.Errorf("read ingest body: %v", err)
		}
		if err := json.Unmarshal(raw.Bytes(), &req); err != nil {
			t.Errorf("decode ingest body: %v", err)
		}
		mu.Lock()
		delivered = append(delivered, req.Events...)
		wireBody += raw.String()
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.Counts{Accepted: len(req.Events)})
	})
	backend := httptest.NewServer(mux)
	defer backend.Close()

	configDir := t.TempDir()
	t.Setenv(config.EnvConfigDir, configDir)
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&config.Config{
		APIURL: backend.URL, CollectorKey: testKey, CollectorID: 43,
		Policy: api.Policy{FlushIntervalSeconds: 1, MaxBatchSize: 100},
	}); err != nil {
		t.Fatal(err)
	}

	port := unusedPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, port, false) }()
	waitHTTP(t, "http://127.0.0.1:"+fmt.Sprint(port)+"/healthz")

	payload := realisticCodexOTel(conversationID)
	body, _ := json.Marshal(payload)
	resp, err := http.Post("http://127.0.0.1:"+fmt.Sprint(port)+"/v1/logs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runServe: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 {
		t.Fatalf("delivered %d events, want 1", len(delivered))
	}
	event := delivered[0]
	if event.Repo == nil || *event.Repo != "serve-path-repo" {
		t.Fatalf("repo = %v, want serve-path-repo", event.Repo)
	}
	if event.EventName == nil || *event.EventName != "codex.api_error" {
		t.Fatalf("event_name = %v, want codex.api_error", event.EventName)
	}
	if event.ErrorMessage == nil || *event.ErrorMessage != "request failed" {
		t.Fatalf("error_message = %v, want request failed", event.ErrorMessage)
	}
	for _, secret := range []string{"SECRET-PROMPT", "SECRET-COMPLETION", "SECRET-TOOL-PAYLOAD"} {
		if strings.Contains(wireBody, secret) {
			t.Fatalf("ingest body leaked %q", secret)
		}
	}
}

func realisticCodexOTel(conversationID string) map[string]any {
	attr := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]any{"stringValue": value}}
	}
	return map[string]any{"resourceLogs": []any{map[string]any{
		"scopeLogs": []any{map[string]any{
			"logRecords": []any{map[string]any{
				"timeUnixNano": "1750000000000000000",
				"attributes": []any{
					attr("event.name", "codex.api_error"),
					attr("conversation.id", conversationID),
					attr("model", "gpt-5"),
					attr("error.message", "request failed"),
					attr("prompt", "SECRET-PROMPT"),
					attr("completion", "SECRET-COMPLETION"),
					attr("tool_input", "SECRET-TOOL-PAYLOAD"),
				},
			}},
		}},
	}}}
}

// TestRunServePersistsReceiverPort proves serve records the port it actually
// started on into config.json, so a later doctor/status without an explicit
// --port can still find the daemon (issue #59).
func TestRunServePersistsReceiverPort(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv(config.EnvConfigDir, configDir)
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.HeartbeatOut{OK: true})
	}))
	defer backend.Close()
	if err := store.Save(&config.Config{
		APIURL: backend.URL, CollectorKey: testKey, CollectorID: 1,
	}); err != nil {
		t.Fatal(err)
	}

	port := unusedPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, port, false) }()
	waitHTTP(t, "http://127.0.0.1:"+fmt.Sprint(port)+"/healthz")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runServe: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReceiverPort != port {
		t.Errorf("ReceiverPort = %d, want %d", got.ReceiverPort, port)
	}
}

func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready", url)
}
