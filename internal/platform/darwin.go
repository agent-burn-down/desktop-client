//go:build darwin

package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	launchAgentsDirPerm = 0o755
	logDirPerm          = 0o700
	plistFilePerm       = 0o644
)

// darwinService manages the collector via a per-user launchd LaunchAgent. All
// paths and the launchctl runner are fields so tests can inject fakes; the
// production constructor resolves them from the real environment.
type darwinService struct {
	uid        int
	program    string
	plistPath  string
	stdoutPath string
	stderrPath string
	runner     commandRunner
}

// newDarwinService resolves the current executable, user id, and config-derived
// paths for a production launchd service.
func newDarwinService() (*darwinService, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate current executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w", err)
	}
	logDir, err := serviceLogDir()
	if err != nil {
		return nil, err
	}
	return &darwinService{
		uid:        os.Getuid(),
		program:    exe,
		plistPath:  filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
		stdoutPath: filepath.Join(logDir, "collector.out.log"),
		stderrPath: filepath.Join(logDir, "collector.err.log"),
		runner:     execRunner{},
	}, nil
}

func (s *darwinService) domain() string { return fmt.Sprintf("gui/%d", s.uid) }
func (s *darwinService) target() string { return fmt.Sprintf("gui/%d/%s", s.uid, Label) }

// Install writes the plist and bootstraps the job, replacing any prior load so
// a repeat install is idempotent.
func (s *darwinService) Install() error {
	if err := s.writePlist(); err != nil {
		return err
	}
	if s.loaded() {
		_, _ = s.runner.run("bootout", s.target())
	}
	if out, err := s.runner.run("bootstrap", s.domain(), s.plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w: %s", err, out)
	}
	return nil
}

// Uninstall boots out the job (if loaded) and removes the plist. Uninstalling
// when nothing is installed is a friendly no-op.
func (s *darwinService) Uninstall() error {
	if !fileExists(s.plistPath) {
		return nil
	}
	if s.loaded() {
		if out, err := s.runner.run("bootout", s.target()); err != nil {
			return fmt.Errorf("launchctl bootout failed: %w: %s", err, out)
		}
	}
	if err := os.Remove(s.plistPath); err != nil {
		return fmt.Errorf("remove plist %s: %w", s.plistPath, err)
	}
	return nil
}

// Start loads the job if needed (RunAtLoad starts it), otherwise kickstarts it.
func (s *darwinService) Start() error {
	if !fileExists(s.plistPath) {
		return fmt.Errorf("service not installed; run `burndown-cli service install` first")
	}
	if !s.loaded() {
		if out, err := s.runner.run("bootstrap", s.domain(), s.plistPath); err != nil {
			return fmt.Errorf("launchctl bootstrap failed: %w: %s", err, out)
		}
		return nil
	}
	if out, err := s.runner.run("kickstart", "-k", s.target()); err != nil {
		return fmt.Errorf("launchctl kickstart failed: %w: %s", err, out)
	}
	return nil
}

// Stop boots the job out (unloads it) so KeepAlive does not restart it, leaving
// the plist in place. Stopping an already-stopped service is a no-op.
func (s *darwinService) Stop() error {
	if !s.loaded() {
		return nil
	}
	if out, err := s.runner.run("bootout", s.target()); err != nil {
		return fmt.Errorf("launchctl bootout failed: %w: %s", err, out)
	}
	return nil
}

// Status reports the current lifecycle state, parsing `launchctl print` for a
// PID when the job is loaded.
func (s *darwinService) Status() (Status, error) {
	out, err := s.runner.run("print", s.target())
	if err != nil {
		if fileExists(s.plistPath) {
			return Status{State: StateStopped}, nil
		}
		return Status{State: StateNotInstalled}, nil
	}
	return parsePrintOutput(out), nil
}

// loaded reports whether the job is currently known to launchd.
func (s *darwinService) loaded() bool {
	_, err := s.runner.run("print", s.target())
	return err == nil
}

// writePlist creates the LaunchAgents and log directories and writes the plist.
func (s *darwinService) writePlist() error {
	if err := os.MkdirAll(filepath.Dir(s.plistPath), launchAgentsDirPerm); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.stdoutPath), logDirPerm); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	data := renderPlist(plistParams{
		Label:      Label,
		Program:    s.program,
		Args:       []string{"serve"},
		StdoutPath: s.stdoutPath,
		StderrPath: s.stderrPath,
	})
	if err := os.WriteFile(s.plistPath, data, plistFilePerm); err != nil {
		return fmt.Errorf("write plist %s: %w", s.plistPath, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
