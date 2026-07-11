package setup

import (
	"path/filepath"
	"testing"
)

func TestDetectClaudeByDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)
	detected, got, err := DetectClaude()
	if err != nil {
		t.Fatal(err)
	}
	if !detected {
		t.Error("existing config dir should be detected")
	}
	if got != dir {
		t.Errorf("dir = %q, want %q", got, dir)
	}
}

func TestDetectClaudeNotPresent(t *testing.T) {
	// Point at a non-existent dir and clear PATH so the binary lookup fails too.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv(EnvClaudeDir, missing)
	t.Setenv("PATH", "")
	detected, _, err := DetectClaude()
	if err != nil {
		t.Fatal(err)
	}
	if detected {
		t.Error("missing dir and empty PATH should report not detected")
	}
}

func TestDetectCodexNotPresent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv(EnvCodexDir, missing)
	t.Setenv("PATH", "")
	detected, _, err := DetectCodex()
	if err != nil {
		t.Fatal(err)
	}
	if detected {
		t.Error("missing dir and empty PATH should report not detected")
	}
}
