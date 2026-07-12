package platform

import (
	"errors"
	"fmt"
)

// Label is the launchd job label for the collector service. It doubles as the
// plist filename stem (com.agentburndown.collector.plist).
const Label = "com.agentburndown.collector"

// ErrUnsupported is returned by New on platforms without a service backend
// (currently everything except darwin). A Windows implementation can slot in
// later without changing callers.
var ErrUnsupported = errors.New("service management is not supported on this platform")

// State is a coarse lifecycle state for the collector service.
type State string

const (
	// StateNotInstalled means no service definition exists on disk.
	StateNotInstalled State = "not-installed"
	// StateRunning means the service is installed, loaded, and has a live PID.
	StateRunning State = "running"
	// StateStopped means the service is installed but not currently running
	// (deliberately stopped, or crashed and not restarting).
	StateStopped State = "stopped"
)

// Status describes the current service state and, when derivable, the PID of
// the running process (zero otherwise).
type Status struct {
	State State
	PID   int
}

// Service manages the lifecycle of the collector as an OS-managed background
// service. Implementations are OS-specific; the interface is shaped so a
// Windows service backend can be added without changing callers.
type Service interface {
	// Install writes the service definition and loads it. Installing over an
	// existing definition replaces it.
	Install() error
	// Uninstall stops the service and removes its definition. Uninstalling when
	// nothing is installed is a no-op.
	Uninstall() error
	// Start starts (or restarts) the service, loading it first if needed.
	Start() error
	// Stop stops the service without removing its definition.
	Stop() error
	// Status reports the current lifecycle state.
	Status() (Status, error)
}

// String renders a Status for humans, appending the PID when running.
func (s Status) String() string {
	if s.State == StateRunning && s.PID > 0 {
		return fmt.Sprintf("%s (pid %d)", s.State, s.PID)
	}
	return string(s.State)
}
