package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestSendTestRoundTrips(t *testing.T) {
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
