//go:build windows

package platform

import (
	"errors"
	"fmt"

	"github.com/kardianos/service"
)

const (
	// windowsServiceName is the service control manager key (no spaces). It is
	// the Windows analogue of the launchd Label.
	windowsServiceName = "AgentBurndownCollector"
	windowsDisplayName = "Agent Burndown Collector"
	windowsDescription = "Receives OpenTelemetry from local coding agents and ships usage rollups."
)

// windowsService manages the collector through the Windows service control
// manager via github.com/kardianos/service. It mirrors darwinService: the
// interface and callers are identical; only the OS backend differs.
//
// The scaffold delegates lifecycle control to kardianos so the shape is real
// and cross-compiles, but two pieces are validated on Windows 11 hardware
// under issue #24, not here:
//   - the collector loop must run under the SCM (kardianos Interface.Run wired
//     into main.go); a binary the SCM launches must speak the SCM protocol
//     within seconds, so `serve` alone is not yet a working service body.
//   - SCM services start at boot and installing them needs admin, which
//     conflicts with the "user-level, starts at login, no admin" goal. If
//     hardware testing rejects that, Install/Start swap to an HKCU Run key or
//     a Task Scheduler logon task without touching this interface or callers.
type windowsService struct {
	svc service.Service
}

// serviceProgram is the kardianos service body. Control operations (install,
// start, status) do not invoke it; running the collector under the SCM does,
// and that wiring lands with the hardware port (#24).
type serviceProgram struct{}

func (serviceProgram) Start(service.Service) error { return nil }
func (serviceProgram) Stop(service.Service) error  { return nil }

// newWindowsService builds the kardianos-backed service from the collector's
// fixed configuration. It runs the current executable with `serve` and asks the
// SCM to restart the process on crash.
func newWindowsService() (*windowsService, error) {
	cfg := &service.Config{
		Name:        windowsServiceName,
		DisplayName: windowsDisplayName,
		Description: windowsDescription,
		Arguments:   []string{"serve"},
		Option: service.KeyValue{
			"StartType":              "automatic",
			"OnFailure":              "restart",
			"OnFailureDelayDuration": "5s",
			"OnFailureResetPeriod":   10,
		},
	}
	svc, err := service.New(serviceProgram{}, cfg)
	if err != nil {
		return nil, fmt.Errorf("build windows service: %w", err)
	}
	return &windowsService{svc: svc}, nil
}

// Install registers the service and starts it. It first removes any prior
// registration so a repeat install is idempotent, mirroring darwin's Install.
func (s *windowsService) Install() error {
	if st, _ := s.svc.Status(); st != service.StatusUnknown {
		_ = s.svc.Stop()
		_ = s.svc.Uninstall()
	}
	if err := s.svc.Install(); err != nil {
		return fmt.Errorf("install windows service: %w", err)
	}
	if err := s.svc.Start(); err != nil {
		return fmt.Errorf("start windows service: %w", err)
	}
	return nil
}

// Uninstall stops the service and removes its registration. Uninstalling when
// nothing is installed is a friendly no-op.
func (s *windowsService) Uninstall() error {
	if _, err := s.svc.Status(); errors.Is(err, service.ErrNotInstalled) {
		return nil
	}
	_ = s.svc.Stop()
	if err := s.svc.Uninstall(); err != nil {
		return fmt.Errorf("uninstall windows service: %w", err)
	}
	return nil
}

// Start starts (or restarts) the service, requiring it to be installed first.
func (s *windowsService) Start() error {
	if _, err := s.svc.Status(); errors.Is(err, service.ErrNotInstalled) {
		return fmt.Errorf("service not installed; run `burndown-cli service install` first")
	}
	if err := s.svc.Restart(); err != nil {
		return fmt.Errorf("restart windows service: %w", err)
	}
	return nil
}

// Stop stops the service without removing its registration. Stopping an
// already-stopped service is a no-op.
func (s *windowsService) Stop() error {
	st, err := s.svc.Status()
	if errors.Is(err, service.ErrNotInstalled) || st != service.StatusRunning {
		return nil
	}
	if err := s.svc.Stop(); err != nil {
		return fmt.Errorf("stop windows service: %w", err)
	}
	return nil
}

// Status reports the current lifecycle state. The SCM does not expose the
// worker PID through kardianos, so PID is always zero on Windows.
func (s *windowsService) Status() (Status, error) {
	return toState(s.svc.Status())
}

// toState maps a kardianos status result onto the platform's coarse state. It
// is a pure function so the mapping is unit-testable without the SCM.
func toState(st service.Status, err error) (Status, error) {
	if errors.Is(err, service.ErrNotInstalled) {
		return Status{State: StateNotInstalled}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("query windows service status: %w", err)
	}
	switch st {
	case service.StatusRunning:
		return Status{State: StateRunning}, nil
	case service.StatusStopped:
		return Status{State: StateStopped}, nil
	default:
		return Status{State: StateNotInstalled}, nil
	}
}
