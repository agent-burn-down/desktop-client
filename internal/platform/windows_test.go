//go:build windows

package platform

import (
	"errors"
	"testing"

	"github.com/kardianos/service"
)

func TestToState(t *testing.T) {
	otherErr := errors.New("boom")
	tests := []struct {
		name    string
		st      service.Status
		err     error
		want    State
		wantErr bool
	}{
		{"not installed", service.StatusUnknown, service.ErrNotInstalled, StateNotInstalled, false},
		{"running", service.StatusRunning, nil, StateRunning, false},
		{"stopped", service.StatusStopped, nil, StateStopped, false},
		{"unknown without error is treated as not-installed", service.StatusUnknown, nil, StateNotInstalled, false},
		{"other error propagates", service.StatusUnknown, otherErr, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toState(tt.st, tt.err)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("toState(%v, %v) = nil error, want error", tt.st, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("toState(%v, %v) unexpected error: %v", tt.st, tt.err, err)
			}
			if got.State != tt.want {
				t.Errorf("toState(%v, %v) state = %q, want %q", tt.st, tt.err, got.State, tt.want)
			}
			if got.PID != 0 {
				t.Errorf("toState PID = %d, want 0 (SCM exposes no PID)", got.PID)
			}
		})
	}
}

func TestNewWindowsServiceImplementsService(t *testing.T) {
	svc, err := newWindowsService()
	if err != nil {
		t.Fatalf("newWindowsService: %v", err)
	}
	var _ Service = svc
	if svc.svc.String() != windowsDisplayName {
		t.Errorf("service name = %q, want %q", svc.svc.String(), windowsDisplayName)
	}
}
