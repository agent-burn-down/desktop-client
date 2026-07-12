package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeviceAuthorizeNoAuthHeader(t *testing.T) {
	var hadKey bool
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadKey = r.Header.Get("X-Collector-Key") != ""
		gotBody = decodeBody(t, r)
		_, _ = io.WriteString(w, `{"device_code":"dc1","user_code":"ABCD-1234",`+
			`"verification_uri":"https://app.example/activate",`+
			`"verification_uri_complete":"https://app.example/activate?code=ABCD-1234",`+
			`"interval":1,"expires_in":900}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	da, err := c.DeviceAuthorize(context.Background(), "laptop-1")
	if err != nil {
		t.Fatalf("DeviceAuthorize: %v", err)
	}
	if hadKey {
		t.Error("DeviceAuthorize sent X-Collector-Key header, want none")
	}
	if gotBody["device_name"] != "laptop-1" {
		t.Errorf("device_name = %v, want laptop-1", gotBody["device_name"])
	}
	if da.DeviceCode != "dc1" || da.UserCode != "ABCD-1234" {
		t.Errorf("device auth = %+v, want dc1/ABCD-1234", da)
	}
	if da.Interval != 1 || da.ExpiresIn != 900 {
		t.Errorf("interval/expires_in = %d/%d, want 1/900", da.Interval, da.ExpiresIn)
	}
}

func TestDeviceTokenCodes(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantErr  bool
	}{
		{"pending", http.StatusBadRequest, `{"error":"authorization_pending"}`, DeviceCodeAuthorizationPending, true},
		{"slow_down", http.StatusBadRequest, `{"error":"slow_down"}`, DeviceCodeSlowDown, true},
		{"denied", http.StatusBadRequest, `{"error":"access_denied"}`, DeviceCodeAccessDenied, true},
		{"expired", http.StatusBadRequest, `{"error":"expired_token"}`, DeviceCodeExpiredToken, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "")
			_, err := c.DeviceToken(context.Background(), "dc1")
			var tokErr *DeviceTokenError
			if !errors.As(err, &tokErr) {
				t.Fatalf("error = %v, want *DeviceTokenError", err)
			}
			if tokErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", tokErr.Code, tc.wantCode)
			}
		})
	}
}

func TestDeviceTokenApprovedNoAuthHeader(t *testing.T) {
	var hadKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadKey = r.Header.Get("X-Collector-Key") != ""
		_, _ = io.WriteString(w, `{"collector_key":"abd_issued","key_id":"42",`+
			`"key_expires_at":"2026-10-09T00:00:00Z"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	dt, err := c.DeviceToken(context.Background(), "dc1")
	if err != nil {
		t.Fatalf("DeviceToken: %v", err)
	}
	if hadKey {
		t.Error("DeviceToken sent X-Collector-Key header, want none")
	}
	if dt.CollectorKey != "abd_issued" || dt.KeyID != "42" {
		t.Errorf("token = %+v, want abd_issued/42", dt)
	}
	if dt.KeyExpiresAt == nil || *dt.KeyExpiresAt != "2026-10-09T00:00:00Z" {
		t.Errorf("key_expires_at = %v", dt.KeyExpiresAt)
	}
}

func TestDeviceTokenDoesNotRetryOn400(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	_, err := c.DeviceToken(context.Background(), "dc1")
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("server called %d times, want 1 (polling owns its own cadence, no client retry)", calls)
	}
}
