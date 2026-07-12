package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

func TestDoctorJSONExitCode(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv("BURNDOWN_CLAUDE_DIR", t.TempDir())
	t.Setenv("BURNDOWN_CODEX_DIR", t.TempDir())

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	// Port 65531 has nothing listening, so the daemon check fails and the
	// aggregate is a non-zero exit even though doctor itself runs cleanly.
	root.SetArgs([]string{"doctor", "--json", "--port", "65531"})
	err := root.Execute()

	var exit *exitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected exitError, got %v", err)
	}
	if exit.Code() == 0 {
		t.Errorf("expected non-zero exit for a broken environment")
	}
	var doc struct {
		Status  string           `json:"status"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("doctor --json did not emit valid JSON: %v\n%s", err, out.String())
	}
	if len(doc.Results) == 0 {
		t.Error("expected at least one check result")
	}
}
