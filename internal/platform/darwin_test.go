package platform

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewResolvesDarwinService(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only constructor")
	}
	t.Setenv("BURNDOWN_CONFIG_DIR", t.TempDir())
	svc, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ds, ok := svc.(*darwinService)
	if !ok {
		t.Fatalf("New returned %T, want *darwinService", svc)
	}
	if !strings.HasSuffix(ds.plistPath, Label+".plist") {
		t.Errorf("plistPath = %s", ds.plistPath)
	}
	if !strings.HasSuffix(ds.stdoutPath, filepath.Join("logs", "collector.out.log")) {
		t.Errorf("stdoutPath = %s", ds.stdoutPath)
	}
	if ds.program == "" {
		t.Error("program (executable path) not resolved")
	}
}

// fakeRunner records launchctl invocations and returns programmed output based
// on the subcommand (first arg). It never touches the real launchctl.
type fakeRunner struct {
	calls    [][]string
	printOut string
	printErr error
	failOn   map[string]error
}

func (f *fakeRunner) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if err, ok := f.failOn[sub]; ok {
		return "", err
	}
	if sub == "print" {
		return f.printOut, f.printErr
	}
	return "", nil
}

func (f *fakeRunner) subcommands() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c[0]
	}
	return out
}

func newTestService(t *testing.T, r commandRunner) *darwinService {
	t.Helper()
	dir := t.TempDir()
	return &darwinService{
		uid:        501,
		program:    "/usr/local/bin/burndown-cli",
		plistPath:  filepath.Join(dir, "LaunchAgents", Label+".plist"),
		stdoutPath: filepath.Join(dir, "logs", "collector.out.log"),
		stderrPath: filepath.Join(dir, "logs", "collector.err.log"),
		runner:     r,
	}
}

func TestInstallWritesPlistAndBootstraps(t *testing.T) {
	r := &fakeRunner{printErr: errors.New("not loaded")}
	s := newTestService(t, r)
	if err := s.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, err := os.ReadFile(s.plistPath)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if !strings.Contains(string(data), "<string>serve</string>") {
		t.Errorf("plist missing serve arg:\n%s", data)
	}
	if !strings.Contains(string(data), s.stdoutPath) {
		t.Errorf("plist missing stdout path")
	}
	// Not loaded, so no bootout; bootstrap must run with domain + plist path.
	if got := r.subcommands(); !equalStrs(got, []string{"print", "bootstrap"}) {
		t.Errorf("calls = %v, want [print bootstrap]", got)
	}
	last := r.calls[len(r.calls)-1]
	if last[1] != s.domain() || last[2] != s.plistPath {
		t.Errorf("bootstrap args = %v, want domain %s + plist", last, s.domain())
	}
}

func TestInstallReplacesWhenAlreadyLoaded(t *testing.T) {
	r := &fakeRunner{printOut: runningPrint} // loaded
	s := newTestService(t, r)
	if err := s.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := r.subcommands(); !equalStrs(got, []string{"print", "bootout", "bootstrap"}) {
		t.Errorf("calls = %v, want [print bootout bootstrap]", got)
	}
}

func TestInstallBootstrapFailure(t *testing.T) {
	r := &fakeRunner{
		printErr: errors.New("not loaded"),
		failOn:   map[string]error{"bootstrap": errors.New("boom")},
	}
	s := newTestService(t, r)
	if err := s.Install(); err == nil {
		t.Fatal("expected Install error on bootstrap failure")
	}
}

func TestUninstallRemovesPlistAndBootsOut(t *testing.T) {
	r := &fakeRunner{printOut: runningPrint}
	s := newTestService(t, r)
	writePlaceholderPlist(t, s)
	if err := s.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if fileExists(s.plistPath) {
		t.Error("plist still present after Uninstall")
	}
	if !containsStr(r.subcommands(), "bootout") {
		t.Errorf("Uninstall did not bootout: %v", r.subcommands())
	}
}

func TestUninstallAbsentIsNoOp(t *testing.T) {
	r := &fakeRunner{printErr: errors.New("not loaded")}
	s := newTestService(t, r)
	if err := s.Uninstall(); err != nil {
		t.Fatalf("Uninstall (absent): %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no launchctl calls, got %v", r.subcommands())
	}
}

func TestStartNotInstalled(t *testing.T) {
	r := &fakeRunner{printErr: errors.New("not loaded")}
	s := newTestService(t, r)
	if err := s.Start(); err == nil {
		t.Fatal("expected Start error when not installed")
	}
}

func TestStartLoadedKickstarts(t *testing.T) {
	r := &fakeRunner{printOut: runningPrint}
	s := newTestService(t, r)
	writePlaceholderPlist(t, s)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !containsStr(r.subcommands(), "kickstart") {
		t.Errorf("Start did not kickstart: %v", r.subcommands())
	}
}

func TestStartInstalledButNotLoadedBootstraps(t *testing.T) {
	r := &fakeRunner{printErr: errors.New("not loaded")}
	s := newTestService(t, r)
	writePlaceholderPlist(t, s)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !containsStr(r.subcommands(), "bootstrap") {
		t.Errorf("Start did not bootstrap: %v", r.subcommands())
	}
}

func TestStopBootsOut(t *testing.T) {
	r := &fakeRunner{printOut: runningPrint}
	s := newTestService(t, r)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !containsStr(r.subcommands(), "bootout") {
		t.Errorf("Stop did not bootout: %v", r.subcommands())
	}
}

func TestStopWhenNotLoadedIsNoOp(t *testing.T) {
	r := &fakeRunner{printErr: errors.New("not loaded")}
	s := newTestService(t, r)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if containsStr(r.subcommands(), "bootout") {
		t.Errorf("Stop booted out an unloaded job: %v", r.subcommands())
	}
}

func TestStatusStates(t *testing.T) {
	tests := []struct {
		name      string
		printOut  string
		printErr  error
		plist     bool
		wantState State
		wantPID   int
	}{
		{"running", runningPrint, nil, true, StateRunning, 55892},
		{"crashed", crashedPrint, nil, true, StateStopped, 0},
		{"installed not loaded", "", errors.New("no such job"), true, StateStopped, 0},
		{"not installed", "", errors.New("no such job"), false, StateNotInstalled, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{printOut: tc.printOut, printErr: tc.printErr}
			s := newTestService(t, r)
			if tc.plist {
				writePlaceholderPlist(t, s)
			}
			got, err := s.Status()
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got.State != tc.wantState || got.PID != tc.wantPID {
				t.Errorf("Status = %+v, want state=%s pid=%d", got, tc.wantState, tc.wantPID)
			}
		})
	}
}

func TestStatusString(t *testing.T) {
	if s := (Status{State: StateRunning, PID: 42}).String(); s != "running (pid 42)" {
		t.Errorf("String = %q", s)
	}
	if s := (Status{State: StateNotInstalled}).String(); s != "not-installed" {
		t.Errorf("String = %q", s)
	}
}

func writePlaceholderPlist(t *testing.T, s *darwinService) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.plistPath, []byte("placeholder"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
