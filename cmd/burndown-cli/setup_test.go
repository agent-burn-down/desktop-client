package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/setup"
)

// setupDirs points both agent config directories at fresh temp dirs.
func setupDirs(t *testing.T) (claudeDir, codexDir string) {
	t.Helper()
	claudeDir = t.TempDir()
	codexDir = t.TempDir()
	t.Setenv(setup.EnvClaudeDir, claudeDir)
	t.Setenv(setup.EnvCodexDir, codexDir)
	return claudeDir, codexDir
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

func TestSetupNotDetectedIsHonest(t *testing.T) {
	// Non-existent dirs and empty PATH: neither agent is detected.
	t.Setenv(setup.EnvClaudeDir, filepath.Join(t.TempDir(), "nope"))
	t.Setenv(setup.EnvCodexDir, filepath.Join(t.TempDir(), "nope"))
	t.Setenv("PATH", "")

	out, err := runCmd(t, "setup", "--check")
	if err != nil {
		t.Fatalf("no detected agents should exit 0: %v", err)
	}
	if !strings.Contains(out, "not detected") {
		t.Errorf("expected honest 'not detected' output, got: %q", out)
	}
}
