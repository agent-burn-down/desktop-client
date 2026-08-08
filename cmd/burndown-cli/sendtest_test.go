package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
)

func TestSendTestRoundTrips(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv, queued := fakeReceiver()
	defer srv.Close()

	out, err := runCmd(t, "send-test", "--port", strconv.Itoa(serverPort(t, srv)))
	if err != nil {
		t.Fatalf("send-test: %v", err)
	}
	if *queued != 1 {
		t.Errorf("queued counter = %d, want 1", *queued)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("expected success message, got: %q", out)
	}
}

func TestSendTestDaemonDown(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv, _ := fakeReceiver()
	port := serverPort(t, srv)
	srv.Close()

	_, err := runCmd(t, "send-test", "--port", strconv.Itoa(port))
	if err == nil {
		t.Fatal("expected an error when the daemon is not running")
	}
	if !strings.Contains(err.Error(), "serve") {
		t.Errorf("error = %q, want a 'serve' hint", err.Error())
	}
}

// TestSendTestSendsConfiguredToken proves send-test carries the configured
// receiver token, exercising the same authenticated path a real agent does
// rather than only the tolerate-mode no-token path.
func TestSendTestSendsConfiguredToken(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	const token = "send-test-token"
	if err := store.Save(&config.Config{ReceiverToken: token}); err != nil {
		t.Fatal(err)
	}

	var gotToken string
	srv, _ := fakeReceiver()
	defer srv.Close()
	// Wrap the fake receiver's mux to capture the token header on /v1/logs.
	orig := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			gotToken = r.Header.Get(receiver.TokenHeader)
		}
		orig.ServeHTTP(w, r)
	})

	if _, err := runCmd(t, "send-test", "--port", strconv.Itoa(serverPort(t, srv))); err != nil {
		t.Fatalf("send-test: %v", err)
	}
	if gotToken != token {
		t.Errorf("token header = %q, want %q", gotToken, token)
	}
}
