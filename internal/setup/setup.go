package setup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Environment overrides for the agent config directories. Tests point these at
// temporary directories so the real ~/.claude and ~/.codex are never touched.
const (
	EnvClaudeDir = "BURNDOWN_CLAUDE_DIR"
	EnvCodexDir  = "BURNDOWN_CODEX_DIR"
)

// ClaudeDir returns the Claude Code config directory, honouring the
// BURNDOWN_CLAUDE_DIR override and otherwise defaulting to ~/.claude.
func ClaudeDir() (string, error) {
	return agentDir(EnvClaudeDir, ".claude")
}

// CodexDir returns the Codex config directory, honouring the BURNDOWN_CODEX_DIR
// override and otherwise defaulting to ~/.codex.
func CodexDir() (string, error) {
	return agentDir(EnvCodexDir, ".codex")
}

func agentDir(envKey, base string) (string, error) {
	if d := os.Getenv(envKey); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, base), nil
}

// DetectClaude reports whether Claude Code appears installed: its config
// directory exists or the `claude` binary is on PATH. The resolved directory is
// returned regardless so callers can name the path in messages.
func DetectClaude() (detected bool, dir string, err error) {
	return detect(ClaudeDir, "claude")
}

// DetectCodex reports whether Codex appears installed: its config directory
// exists or the `codex` binary is on PATH.
func DetectCodex() (detected bool, dir string, err error) {
	return detect(CodexDir, "codex")
}

func detect(dirFn func() (string, error), binary string) (bool, string, error) {
	dir, err := dirFn()
	if err != nil {
		return false, "", err
	}
	if dirExists(dir) {
		return true, dir, nil
	}
	if _, err := exec.LookPath(binary); err == nil {
		return true, dir, nil
	}
	return false, dir, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// backupFile copies path to "path+suffix+<timestamp>" preserving its mode, then
// returns the backup path. A missing source is not an error: an empty string is
// returned and no backup is written.
func backupFile(path, suffix string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is an agent config file we manage
	if err != nil {
		return "", err
	}
	backup := path + suffix + time.Now().Format("20060102-150405")
	//nolint:gosec // backup path derives from an agent config path we already manage
	if err := os.WriteFile(backup, data, info.Mode().Perm()); err != nil {
		return "", err
	}
	return backup, nil
}
