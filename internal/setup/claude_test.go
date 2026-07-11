package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readClaudeEnv decodes settings.json and returns its env object.
func readClaudeEnv(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	env, _ := settings["env"].(map[string]any)
	return env
}

func TestPlanClaudeFreshAddsAllKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)

	plan, err := PlanClaude(9000)
	if err != nil {
		t.Fatalf("PlanClaude: %v", err)
	}
	if plan.Empty() {
		t.Fatal("fresh install: expected pending additions")
	}
	if got := len(plan.Descriptions()); got != len(claudeKeyOrder) {
		t.Fatalf("descriptions = %d, want %d", got, len(claudeKeyOrder))
	}
	backup, err := plan.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if backup != "" {
		t.Fatalf("no prior file, expected no backup, got %q", backup)
	}
	env := readClaudeEnv(t, filepath.Join(dir, "settings.json"))
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://localhost:9000" {
		t.Fatalf("endpoint = %v, want port-bound value", env["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}
	for _, k := range claudeKeyOrder {
		if _, ok := env[k]; !ok {
			t.Errorf("key %q missing after apply", k)
		}
	}
}

func TestPlanClaudePreservesUserValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)
	path := filepath.Join(dir, "settings.json")
	seed := map[string]any{
		"model": "opus", // unrelated field must survive
		"env": map[string]any{
			"OTEL_METRICS_EXPORTER": "prometheus", // user value, must not change
			"MY_OWN_VAR":            "keepme",
		},
	}
	data, _ := json.MarshalIndent(seed, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanClaude(8765)
	if err != nil {
		t.Fatalf("PlanClaude: %v", err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	env := readClaudeEnv(t, path)
	if env["OTEL_METRICS_EXPORTER"] != "prometheus" {
		t.Errorf("user OTEL_METRICS_EXPORTER overwritten: %v", env["OTEL_METRICS_EXPORTER"])
	}
	if env["MY_OWN_VAR"] != "keepme" {
		t.Errorf("user MY_OWN_VAR lost: %v", env["MY_OWN_VAR"])
	}
	if env["OTEL_LOGS_EXPORTER"] != "otlp" {
		t.Errorf("missing key not added: %v", env["OTEL_LOGS_EXPORTER"])
	}
	settings := readWholeSettings(t, path)
	if settings["model"] != "opus" {
		t.Errorf("unrelated field lost: %v", settings["model"])
	}
}

func readWholeSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPlanClaudeRefusesNonObjectEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"env": "not-an-object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanClaude(8765); err == nil {
		t.Fatal("expected refusal for non-object env")
	}
}

func TestPlanClaudeRefusesInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{not valid`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanClaude(8765); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestClaudeSecondRunNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)
	path := filepath.Join(dir, "settings.json")
	// Seed a partial file so the first apply writes a backup.
	if err := os.WriteFile(path, []byte(`{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := PlanClaude(8765)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := first.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("expected a backup on first apply over an existing file")
	}
	afterFirst, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backupsBefore := countBackups(t, dir, ".json.bak.")

	// Second plan must be a no-op: nothing to add.
	second, err := PlanClaude(8765)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Empty() {
		t.Fatalf("second run not a no-op: %v", second.Descriptions())
	}
	afterSecond, _ := os.ReadFile(path)
	if string(afterFirst) != string(afterSecond) {
		t.Error("file bytes changed on second (no-op) run")
	}
	if got := countBackups(t, dir, ".json.bak."); got != backupsBefore {
		t.Errorf("backups grew on no-op run: %d -> %d", backupsBefore, got)
	}
}

func TestClaudeBackupMatchesOriginal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvClaudeDir, dir)
	path := filepath.Join(dir, "settings.json")
	original := []byte(`{"env":{"MY_OWN_VAR":"keepme"}}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanClaude(8765)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := plan.Apply()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("backup content = %q, want original %q", got, original)
	}
}

func countBackups(t *testing.T, dir, marker string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), marker) {
			n++
		}
	}
	return n
}
