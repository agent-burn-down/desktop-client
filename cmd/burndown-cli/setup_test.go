package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/platform"
	"github.com/agent-burn-down/desktop-client/internal/setup"
)

// setupDirs points both agent config directories, and the collector config
// directory (which now holds the generated receiver token), at fresh temp
// dirs so tests never touch the developer's real ~/.burndown.
//
// It also stubs the platform service to a not-installed fake, so applying a
// plan never shells out to the real launchd (`newPlatformService` otherwise
// defaults to the real platform.New on darwin). Tests that care about the
// restart behavior itself override it again with stubPlatformService.
func setupDirs(t *testing.T) (claudeDir, codexDir string) {
	t.Helper()
	claudeDir = t.TempDir()
	codexDir = t.TempDir()
	t.Setenv(setup.EnvClaudeDir, claudeDir)
	t.Setenv(setup.EnvCodexDir, codexDir)
	t.Setenv(config.EnvConfigDir, t.TempDir())
	stubPlatformService(t, &fakeSetupService{status: platform.Status{State: platform.StateNotInstalled}})
	return claudeDir, codexDir
}

// fakeSetupService is an injectable platform.Service for setup's tests.
type fakeSetupService struct {
	status     platform.Status
	statusErr  error
	startErr   error
	startCalls int
}

func (f *fakeSetupService) Install() error   { return nil }
func (f *fakeSetupService) Uninstall() error { return nil }
func (f *fakeSetupService) Start() error {
	f.startCalls++
	return f.startErr
}
func (f *fakeSetupService) Stop() error { return nil }
func (f *fakeSetupService) Status() (platform.Status, error) {
	return f.status, f.statusErr
}

// stubPlatformService overrides newPlatformService for the duration of the
// test, restoring the original afterward.
func stubPlatformService(t *testing.T, svc platform.Service) {
	t.Helper()
	original := newPlatformService
	newPlatformService = func() (platform.Service, error) { return svc, nil }
	t.Cleanup(func() { newPlatformService = original })
}

func TestSetupCheckReportsPendingAndExitsNonZero(t *testing.T) {
	claudeDir, codexDir := setupDirs(t)

	out, err := runCmd(t, "setup", "--check", "--all")
	if err == nil {
		t.Fatal("expected non-zero exit (error) when changes are pending")
	}
	if !strings.Contains(out, "will add") {
		t.Errorf("expected a plan in output, got: %q", out)
	}
	// --check must not write anything.
	if _, statErr := os.Stat(filepath.Join(claudeDir, "settings.json")); statErr == nil {
		t.Error("--check wrote settings.json")
	}
	if _, statErr := os.Stat(filepath.Join(codexDir, "config.toml")); statErr == nil {
		t.Error("--check wrote config.toml")
	}
}

func TestSetupApplyThenCheckNoOp(t *testing.T) {
	claudeDir, codexDir := setupDirs(t)

	if _, err := runCmd(t, "setup", "--all", "--yes"); err != nil {
		t.Fatalf("setup apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "settings.json")); err != nil {
		t.Errorf("settings.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexDir, "config.toml")); err != nil {
		t.Errorf("config.toml not written: %v", err)
	}

	// Second run via --check must be a clean no-op (exit 0).
	out, err := runCmd(t, "setup", "--check", "--all")
	if err != nil {
		t.Fatalf("second --check should exit 0, got: %v", err)
	}
	if !strings.Contains(out, "Nothing to do") {
		t.Errorf("expected no-op message, got: %q", out)
	}
}

// TestSetupRestartsRunningCollectorService proves setup automatically
// restarts the collector daemon when it's running as the managed service, so
// it picks up the token setup just wrote instead of continuing to reject
// tokened agent requests until it happens to restart on its own.
func TestSetupRestartsRunningCollectorService(t *testing.T) {
	setupDirs(t)
	svc := &fakeSetupService{status: platform.Status{State: platform.StateRunning}}
	stubPlatformService(t, svc)

	out, err := runCmd(t, "setup", "--all", "--yes")
	if err != nil {
		t.Fatalf("setup apply: %v", err)
	}
	if svc.startCalls != 1 {
		t.Errorf("service Start() called %d times, want 1", svc.startCalls)
	}
	if !strings.Contains(out, "Collector service restarted") {
		t.Errorf("expected a restart confirmation, got: %q", out)
	}
}

// TestSetupPromptsManualRestartWhenServiceNotRunning proves that when the
// daemon isn't running as the managed service (not installed, or running
// manually via `serve` in another terminal — indistinguishable from here),
// setup falls back to telling the operator to restart it themselves, rather
// than silently leaving a stale token loaded.
func TestSetupPromptsManualRestartWhenServiceNotRunning(t *testing.T) {
	setupDirs(t) // stubs the service as StateNotInstalled by default

	out, err := runCmd(t, "setup", "--all", "--yes")
	if err != nil {
		t.Fatalf("setup apply: %v", err)
	}
	if !strings.Contains(out, "restart it") {
		t.Errorf("expected a manual-restart hint, got: %q", out)
	}
	if strings.Contains(out, "Collector service restarted") {
		t.Errorf("should not claim an automatic restart happened, got: %q", out)
	}
}

// TestSetupWarnsWhenServiceRestartFails proves a failed automatic restart
// warns and falls back to the manual hint, but never fails `setup` itself —
// the agent config files are already written by the time this runs.
func TestSetupWarnsWhenServiceRestartFails(t *testing.T) {
	setupDirs(t)
	svc := &fakeSetupService{
		status:   platform.Status{State: platform.StateRunning},
		startErr: errors.New("boom"),
	}
	stubPlatformService(t, svc)

	out, err := runCmd(t, "setup", "--all", "--yes")
	if err != nil {
		t.Fatalf("a failed service restart must not fail setup: %v", err)
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "restart it") {
		t.Errorf("expected a warning plus a manual-restart hint, got: %q", out)
	}
}

func TestSetupNotDetectedIsHonest(t *testing.T) {
	// Non-existent dirs and empty PATH: neither agent is detected.
	t.Setenv(setup.EnvClaudeDir, filepath.Join(t.TempDir(), "nope"))
	t.Setenv(setup.EnvCodexDir, filepath.Join(t.TempDir(), "nope"))
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv("PATH", "")

	out, err := runCmd(t, "setup", "--check")
	if err != nil {
		t.Fatalf("no detected agents should exit 0: %v", err)
	}
	if !strings.Contains(out, "not detected") {
		t.Errorf("expected honest 'not detected' output, got: %q", out)
	}
}
