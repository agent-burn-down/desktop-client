// Package receiver implements the local OTLP/HTTP JSON server that local agents
// (Claude Code, Codex) post telemetry to. It binds loopback only, rejects
// cross-origin and off-host callers, and otherwise always answers 200 so an
// agent never observes collector failure and never retries.
package receiver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
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
	// MetricsHandler receives decoded /v1/metrics payloads.
	MetricsHandler LogHandler
	// Counters, if set, contributes extra counters to /healthz.
	Counters CountersFunc
	// Logger receives recovered handler panics. Defaults to slog.Default().
	Logger *slog.Logger
}

// Server is the local OTLP/HTTP receiver.
type Server struct {
	addr           string
	handler        LogHandler
	metricsHandler LogHandler
	counters       CountersFunc
	logger         *slog.Logger
	http           *http.Server
	ln             net.Listener

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
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		addr:           net.JoinHostPort(host, fmt.Sprintf("%d", port)),
		handler:        cfg.Handler,
		metricsHandler: cfg.MetricsHandler,
		counters:       cfg.Counters,
		logger:         logger,
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

// isLoopbackHost reports whether an HTTP request's Host header refers to a
// loopback address, closing DNS-rebinding attacks that resolve an
// attacker-controlled name to 127.0.0.1 but send a non-loopback Host header.
// host may or may not carry a port, and an IPv6 literal may use bracket
// notation ("[::1]:8765" or bare "[::1]").
func isLoopbackHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	// Hostnames are case-insensitive, and a false 403 here silently stops an
	// agent's telemetry, so match "localhost" in any case.
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
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

// ServeHTTP guards every request against cross-origin and off-host callers
// before routing the three supported endpoints; all other paths yield 404.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.reject(w, r) {
		return
	}
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

// reject answers and reports true when r fails a cross-origin, host, or
// content-type guard; ServeHTTP must stop routing when it does.
//
// Only POST bodies (the /v1/logs and /v1/metrics writes) are required to
// declare Content-Type: application/json — this is what forces a CORS
// preflight for a browser-driven write, the actual attack surface. GET
// /healthz carries no body, so requiring a Content-Type on it would only
// break status/doctor's health probe for no security benefit.
func (s *Server) reject(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != "" {
		writeJSON(w, http.StatusForbidden,
			map[string]any{"error": "cross-origin requests are not accepted"})
		return true
	}
	if !isLoopbackHost(r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "unexpected host"})
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeJSON(w, http.StatusUnsupportedMediaType,
			map[string]any{"error": "unsupported content type"})
		return true
	}
	return false
}

// isJSONContentType reports whether ct declares exactly application/json,
// with or without parameters ("application/json; charset=utf-8").
//
// A prefix match would also accept "application/json-anything", which is not
// JSON. That is not reachable from the browser vector — any type outside the
// three CORS-simple values forces a preflight the receiver already refuses —
// but the media type is cheap to parse properly, so parse it properly.
func isJSONContentType(ct string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	return err == nil && mediaType == "application/json"
}

// handleLogs decodes the body and hands it to the pipeline handler, always
// answering 200 unless the handler panics.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	s.logsReceived.Add(1)
	payload := decodeBody(w, r)
	accepted, dropped, panicked := s.invoke(s.handler, payload)
	if panicked {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": accepted, "dropped": dropped})
}

// invoke calls handler, recovering from panics so the process never crashes.
// A recovered panic is logged locally and never echoed to the caller; a nil
// handler (no pipeline wired) reports zero accepted/dropped.
func (s *Server) invoke(
	handler LogHandler, payload map[string]any,
) (accepted, dropped int, panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("receiver handler panicked", "recover", rec)
			accepted, dropped, panicked = 0, 0, true
		}
	}()
	if handler == nil {
		return 0, 0, false
	}
	accepted, dropped = handler(payload)
	return accepted, dropped, false
}

// handleMetrics decodes the body and hands it to the metrics pipeline handler,
// always answering 200 unless the handler panics.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.metricsReceived.Add(1)
	payload := decodeBody(w, r)
	accepted, dropped, panicked := s.invoke(s.metricsHandler, payload)
	if panicked {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": accepted, "dropped": dropped})
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
