package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startTest(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.Host = "127.0.0.1"
	cfg.Port = freePort(t)
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

// freePort reserves an OS-assigned loopback port and releases it so the
// receiver can bind it, avoiding collisions with the default 8765.
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

func post(t *testing.T, s *Server, path, contentType string, body []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+s.Addr()+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return resp.StatusCode, m
}

func TestLogsAlwaysReturns200(t *testing.T) {
	var lastPayload map[string]any
	s := startTest(t, Config{Handler: func(p map[string]any) (int, int) {
		lastPayload = p
		return 2, 1
	}})
	cases := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{"valid json", "application/json", []byte(`{"resourceLogs":[]}`)},
		{"malformed json", "application/json", []byte(`{not json`)},
		{"wrong content type", "text/plain", []byte(`{"resourceLogs":[]}`)},
		{"empty body", "application/json", []byte(``)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := post(t, s, "/v1/logs", tc.contentType, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if _, ok := body["accepted"]; !ok {
				t.Fatalf("response missing accepted: %v", body)
			}
		})
	}
	_ = lastPayload
}

func TestOversizedBodyReturns200(t *testing.T) {
	s := startTest(t, Config{Handler: func(p map[string]any) (int, int) {
		if len(p) != 0 {
			t.Errorf("oversized body should decode to empty payload, got %v", p)
		}
		return 0, 0
	}})
	big := append([]byte(`{"x":"`), bytes.Repeat([]byte("a"), maxBodyBytes+1024)...)
	big = append(big, []byte(`"}`)...)
	status, _ := post(t, s, "/v1/logs", "application/json", big)
	if status != http.StatusOK {
		t.Fatalf("oversized status = %d, want 200", status)
	}
}

func TestHandlerPanicStill200(t *testing.T) {
	s := startTest(t, Config{Handler: func(map[string]any) (int, int) {
		panic("boom")
	}})
	status, body := post(t, s, "/v1/logs", "application/json", []byte(`{}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["error"] == nil || !strings.Contains(fmt.Sprint(body["error"]), "boom") {
		t.Fatalf("expected recovered error in body, got %v", body)
	}
}

func TestMetricsCounted(t *testing.T) {
	s := startTest(t, Config{})
	status, body := post(t, s, "/v1/metrics", "application/json", []byte(`{"resourceMetrics":[]}`))
	if status != http.StatusOK || body["accepted"].(float64) != 0 {
		t.Fatalf("metrics response = %d %v", status, body)
	}
	_, health := healthGet(t, s)
	counters := health["counters"].(map[string]any)
	if counters["metrics_received"].(float64) != 1 {
		t.Fatalf("metrics_received = %v, want 1", counters["metrics_received"])
	}
}

func TestHealthzCountersMerged(t *testing.T) {
	s := startTest(t, Config{Counters: func() map[string]int64 {
		return map[string]int64{"queue_depth": 7}
	}})
	status, health := healthGet(t, s)
	if status != http.StatusOK || health["ok"] != true {
		t.Fatalf("health = %d %v", status, health)
	}
	counters := health["counters"].(map[string]any)
	if counters["queue_depth"].(float64) != 7 {
		t.Fatalf("merged counter missing: %v", counters)
	}
}

func healthGet(t *testing.T, s *Server) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
	return do(t, req)
}

func TestUnknownPath404(t *testing.T) {
	s := startTest(t, Config{})
	req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/nope", nil)
	status, body := do(t, req)
	if status != http.StatusNotFound || body["error"] != "unknown path" {
		t.Fatalf("unknown path = %d %v", status, body)
	}
}

func TestNonLoopbackRefused(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "192.168.1.5", "example.com"} {
		if _, err := New(Config{Host: host}); err == nil {
			t.Fatalf("host %q should be refused", host)
		}
	}
	for _, host := range []string{"127.0.0.1", "::1", "localhost", ""} {
		if _, err := New(Config{Host: host}); err != nil {
			t.Fatalf("loopback host %q should be accepted: %v", host, err)
		}
	}
}

func TestPortInUseError(t *testing.T) {
	s := startTest(t, Config{})
	port := portOf(t, s.Addr())
	dup, err := New(Config{Host: "127.0.0.1", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	if err := dup.Start(); err == nil {
		t.Fatal("expected port-in-use error")
	} else if !strings.Contains(err.Error(), "another burndown instance") {
		t.Fatalf("error should mention another instance: %v", err)
	}
}

func TestGracefulShutdown(t *testing.T) {
	s := startTest(t, Config{Handler: func(map[string]any) (int, int) { return 1, 0 }})
	status, _ := post(t, s, "/v1/logs", "application/json", []byte(`{}`))
	if status != http.StatusOK {
		t.Fatal("pre-shutdown request failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}
