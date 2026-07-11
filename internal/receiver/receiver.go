// Package receiver implements the local OTLP/HTTP JSON server that local agents
// (Claude Code, Codex) post telemetry to. It binds loopback only and always
// answers 200 so an agent never observes collector failure and never retries.
package receiver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const (
	// DefaultPort is the loopback port agents post OTLP logs to.
	DefaultPort = 8765
	// maxBodyBytes caps a single request body (8 MiB).
	maxBodyBytes    = 8 * 1024 * 1024
	readHeaderLimit = 5 * time.Second
)

// LogHandler consumes a decoded OTLP/HTTP logs payload and reports how many
// records were accepted and dropped. It must not assume any payload shape.
type LogHandler func(payload map[string]any) (accepted, dropped int)

// CountersFunc returns a snapshot of pipeline counters for /healthz.
type CountersFunc func() map[string]int64

// Config configures a receiver Server.
type Config struct {
	// Host to bind; must be loopback. Empty defaults to 127.0.0.1.
	Host string
	// Port to bind; zero defaults to DefaultPort.
	Port int
	// Handler receives decoded /v1/logs payloads.
	Handler LogHandler
	// Counters, if set, contributes extra counters to /healthz.
	Counters CountersFunc
}

// Server is the local OTLP/HTTP receiver.
type Server struct {
	addr     string
	handler  LogHandler
	counters CountersFunc
	http     *http.Server
	ln       net.Listener

	logsReceived    atomic.Int64
	metricsReceived atomic.Int64
}

// New validates the bind host is loopback and returns a receiver Server.
func New(cfg Config) (*Server, error) {
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	if err := requireLoopback(host); err != nil {
		return nil, err
	}
	port := cfg.Port
	if port == 0 {
		port = DefaultPort
	}
	s := &Server{
		addr:     net.JoinHostPort(host, fmt.Sprintf("%d", port)),
		handler:  cfg.Handler,
		counters: cfg.Counters,
	}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: readHeaderLimit}
	return s, nil
}

// requireLoopback rejects any host that is not a loopback address.
func requireLoopback(host string) error {
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to bind non-loopback host %q; receiver is loopback-only", host)
	}
	return nil
}

// Start binds the listener and serves in the background. A bind failure
// (typically the port already in use) is returned synchronously.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf(
			"cannot bind %s (another burndown instance may be running): %w", s.addr, err)
	}
	s.ln = ln
	go func() { _ = s.http.Serve(ln) }()
	return nil
}

// Addr returns the actual bound address (useful when binding port 0 in tests).
func (s *Server) Addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.addr
}

// Shutdown gracefully drains in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// ServeHTTP routes the three supported endpoints; all other paths yield 404.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/logs":
		s.handleLogs(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/metrics":
		s.handleMetrics(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		s.handleHealth(w)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown path"})
	}
}

// handleLogs decodes the body and hands it to the pipeline handler, always
// answering 200 even when the handler panics.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.logsReceived.Add(1)
	payload := decodeBody(w, r)
	accepted, dropped, errStr := s.invoke(payload)
	body := map[string]any{"accepted": accepted, "dropped": dropped}
	if errStr != "" {
		body["error"] = errStr
	}
	writeJSON(w, http.StatusOK, body)
}

// invoke calls the handler, recovering from panics so the response is always
// produced.
func (s *Server) invoke(payload map[string]any) (accepted, dropped int, errStr string) {
	defer func() {
		if rec := recover(); rec != nil {
			accepted, dropped, errStr = 0, 0, fmt.Sprintf("%v", rec)
		}
	}()
	if s.handler == nil {
		return 0, 0, ""
	}
	accepted, dropped = s.handler(payload)
	return accepted, dropped, ""
}

// handleMetrics accepts and discards a metrics payload (not yet forwarded).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metricsReceived.Add(1)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	_, _ = io.Copy(io.Discard, r.Body)
	writeJSON(w, http.StatusOK, map[string]any{"accepted": 0, "dropped": 0})
}

// handleHealth returns a counters snapshot for status and doctor.
func (s *Server) handleHealth(w http.ResponseWriter) {
	counters := map[string]int64{
		"logs_received":    s.logsReceived.Load(),
		"metrics_received": s.metricsReceived.Load(),
	}
	if s.counters != nil {
		for k, v := range s.counters() {
			counters[k] = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "counters": counters})
}

// decodeBody reads the capped body and decodes it as a JSON object. Over-cap or
// unparseable bodies decode to an empty map rather than an error.
func decodeBody(w http.ResponseWriter, r *http.Request) map[string]any {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"error":"encode failure"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
