package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/platform"
)

// fakeService is an injectable platform.Service for tests.
type fakeService struct {
	status    platform.Status
	statusErr error
}

func (f *fakeService) Install() error   { return nil }
func (f *fakeService) Uninstall() error { return nil }
func (f *fakeService) Start() error     { return nil }
func (f *fakeService) Stop() error      { return nil }
func (f *fakeService) Status() (platform.Status, error) {
	return f.status, f.statusErr
}

// newDoctor builds a Doctor with an isolated config dir. Callers override
// injectable fields (URLs, HTTP client, service, queue path) via Config.
func newDoctor(t *testing.T, c Config) *Doctor {
	t.Helper()
	if os.Getenv("BURNDOWN_CONFIG_DIR") == "" {
		t.Setenv("BURNDOWN_CONFIG_DIR", t.TempDir())
	}
	d, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestAggregateAndExitCode(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    Status
		code    int
	}{
		{"all pass", []Result{{Status: Pass}, {Status: Pass}}, Pass, 0},
		{"skip only", []Result{{Status: Pass}, {Status: Skip}}, Pass, 0},
		{"warn wins over pass", []Result{{Status: Pass}, {Status: Warn}}, Warn, 1},
		{"fail wins over warn", []Result{{Status: Warn}, {Status: Fail}}, Fail, 2},
		{"empty", nil, Pass, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Aggregate(tc.results); got != tc.want {
				t.Errorf("Aggregate = %s, want %s", got, tc.want)
			}
			if got := ExitCode(tc.results); got != tc.code {
				t.Errorf("ExitCode = %d, want %d", got, tc.code)
			}
		})
	}
}

// TestRunEveryNonPassHasHint is the acceptance guard: any warn or fail result
// must carry a remediation hint. Run is exercised with a broken environment
// (no config, backend down, daemon down, service not installed).
func TestRunEveryNonPassHasHint(t *testing.T) {
	t.Setenv("BURNDOWN_CONFIG_DIR", t.TempDir())
	t.Setenv("BURNDOWN_CLAUDE_DIR", t.TempDir())
	t.Setenv("BURNDOWN_CODEX_DIR", t.TempDir())
	d := newDoctor(t, Config{
		Port:       65533, // nothing listening
		GitHubURL:  "http://127.0.0.1:65533/none",
		Service:    &fakeService{status: platform.Status{State: platform.StateNotInstalled}},
		HealthzURL: "http://127.0.0.1:65533/healthz",
	})
	results := d.Run(context.Background())
	if len(results) != 8 {
		t.Fatalf("expected 8 checks, got %d", len(results))
	}
	for _, r := range results {
		if (r.Status == Warn || r.Status == Fail) && r.Hint == "" {
			t.Errorf("check %q is %s but has no remediation hint", r.Name, r.Status)
		}
	}
}

func TestRunSafeWithDaemonDown(t *testing.T) {
	t.Setenv("BURNDOWN_CONFIG_DIR", t.TempDir())
	t.Setenv("BURNDOWN_CLAUDE_DIR", t.TempDir())
	t.Setenv("BURNDOWN_CODEX_DIR", t.TempDir())
	d := newDoctor(t, Config{
		Port:       65533,
		GitHubURL:  "http://127.0.0.1:65533/none",
		Service:    &fakeService{status: platform.Status{State: platform.StateNotInstalled}},
		HealthzURL: "http://127.0.0.1:65533/healthz",
	})
	// Must not panic and must produce a result for every named check.
	results := d.Run(context.Background())
	names := map[string]bool{}
	for _, r := range results {
		names[r.Name] = true
	}
	for _, want := range []string{
		"version", "config", "backend", "heartbeat", "daemon", "agents", "queue", "service",
	} {
		if !names[want] {
			t.Errorf("missing check %q", want)
		}
	}
}

func TestWriteJSONShape(t *testing.T) {
	results := []Result{
		{Name: "version", Status: Pass, Detail: "up to date (v1.0.0)"},
		{Name: "config", Status: Fail, Detail: "missing", Hint: "run `burndown-cli login`"},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, results); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var doc struct {
		Status  string `json:"status"`
		Results []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
			Hint   string `json:"hint"`
		} `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("emitted JSON does not parse: %v", err)
	}
	if doc.Status != "fail" {
		t.Errorf("aggregate status = %q, want fail", doc.Status)
	}
	if len(doc.Results) != 2 {
		t.Fatalf("results len = %d, want 2", len(doc.Results))
	}
	if doc.Results[1].Hint != "run `burndown-cli login`" {
		t.Errorf("hint not emitted: %+v", doc.Results[1])
	}
	// A pass result omits the hint field entirely (omitempty).
	if strings.Contains(strings.Split(buf.String(), "config")[0], `"hint"`) {
		t.Error("pass result should not include a hint field")
	}
}

func TestWriteText(t *testing.T) {
	results := []Result{
		{Name: "daemon", Status: Fail, Detail: "down", Hint: "run serve"},
	}
	var buf bytes.Buffer
	WriteText(&buf, results)
	out := buf.String()
	if !strings.Contains(out, "fix: run serve") {
		t.Errorf("text output missing hint line:\n%s", out)
	}
	if !strings.Contains(out, "overall: fail") {
		t.Errorf("text output missing overall line:\n%s", out)
	}
}
