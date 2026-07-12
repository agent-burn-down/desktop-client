package version

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLatestRelease(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantTag string
		wantErr bool
	}{
		{"published", http.StatusOK, `{"tag_name":"v1.4.0"}`, "v1.4.0", false},
		{"no releases", http.StatusNotFound, `{"message":"Not Found"}`, "", false},
		{"server error", http.StatusInternalServerError, "boom", "", true},
		{"bad json", http.StatusOK, "not json", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
			defer srv.Close()
			tag, err := LatestRelease(context.Background(), srv.Client(), srv.URL)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tag != tc.wantTag {
				t.Errorf("tag = %q, want %q", tag, tc.wantTag)
			}
		})
	}
}

func TestLatestReleaseNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // force a connection failure
	client := &http.Client{Timeout: time.Second}
	if _, err := LatestRelease(context.Background(), client, url); err == nil {
		t.Fatal("expected transport error, got nil")
	}
}
