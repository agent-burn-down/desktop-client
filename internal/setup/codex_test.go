package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanCodexFreshCreatesTable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCodexDir, dir)

	plan, err := PlanCodex(8765)
	if err != nil {
		t.Fatalf("PlanCodex: %v", err)
	}
	if plan.Empty() {
		t.Fatal("fresh install: expected pending changes")
	}
	backup, err := plan.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if backup != "" {
		t.Fatalf("no prior file, expected no backup, got %q", backup)
	}
	got := readFile(t, filepath.Join(dir, "config.toml"))
	for _, want := range []string{
		"[otel]",
		`environment = "control-center"`,
		`metrics_exporter = "none"`,
		`trace_exporter = "none"`,
		"log_user_prompt = false",
		`exporter = { otlp-http = { endpoint = "http://127.0.0.1:8765/v1/logs", protocol = "json" } }`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.toml missing %q\n---\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("config.toml must end with a newline")
	}
}

func TestPlanCodexPreservesExistingOtel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCodexDir, dir)
	path := filepath.Join(dir, "config.toml")
	existing := "[otel]\n" +
		"environment = \"my-custom-env\"\n" + // user value, must survive
		"metrics_exporter = \"otlp\"\n" + // user value, must survive
		"\n[other]\nkey = \"value\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCodex(8765)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, `environment = "my-custom-env"`) {
		t.Error("user environment value overwritten")
	}
	if !strings.Contains(got, `metrics_exporter = "otlp"`) {
		t.Error("user metrics_exporter value overwritten")
	}
	if strings.Contains(got, `environment = "control-center"`) {
		t.Error("added a second environment line instead of leaving the user value")
	}
	if !strings.Contains(got, "[other]") || !strings.Contains(got, `key = "value"`) {
		t.Error("unrelated [other] table lost")
	}
	// The missing scalar keys and exporter should still be added.
	if !strings.Contains(got, "trace_exporter = \"none\"") {
		t.Error("missing trace_exporter not added")
	}
}

func TestPlanCodexRemovesStaleExporterTables(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCodexDir, dir)
	path := filepath.Join(dir, "config.toml")
	existing := "[otel]\n" +
		"environment = \"control-center\"\n" +
		"metrics_exporter = \"none\"\n" +
		"trace_exporter = \"none\"\n" +
		"log_user_prompt = false\n" +
		"\n[otel.exporter.otlp-http]\n" +
		"endpoint = \"http://127.0.0.1:9999/v1/logs\"\n" +
		"protocol = \"json\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCodex(8765)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("expected changes: stale table removal + exporter insert")
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "[otel.exporter.otlp-http]") {
		t.Errorf("stale nested exporter table not removed:\n%s", got)
	}
	if !strings.Contains(got,
		`exporter = { otlp-http = { endpoint = "http://127.0.0.1:8765/v1/logs", protocol = "json" } }`) {
		t.Errorf("inline exporter not inserted:\n%s", got)
	}
}

func TestPlanCodexReplacesDifferingExporter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCodexDir, dir)
	path := filepath.Join(dir, "config.toml")
	existing := "[otel]\n" +
		"environment = \"control-center\"\n" +
		"metrics_exporter = \"none\"\n" +
		"trace_exporter = \"none\"\n" +
		"log_user_prompt = false\n" +
		`exporter = { otlp-http = { endpoint = "http://127.0.0.1:1234/v1/logs", protocol = "json" } }` + "\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCodex(8765)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("expected exporter replacement")
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "1234") {
		t.Errorf("old exporter endpoint not replaced:\n%s", got)
	}
	if !strings.Contains(got, "8765/v1/logs") {
		t.Errorf("new exporter endpoint missing:\n%s", got)
	}
}

func TestCodexSecondRunNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCodexDir, dir)
	path := filepath.Join(dir, "config.toml")

	first, err := PlanCodex(8765)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Apply(); err != nil {
		t.Fatal(err)
	}
	afterFirst := readFile(t, path)
	backupsBefore := countBackups(t, dir, ".toml.bak.")

	second, err := PlanCodex(8765)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Empty() {
		t.Fatalf("second run not a no-op: %v", second.Descriptions())
	}
	if got := readFile(t, path); got != afterFirst {
		t.Error("file bytes changed on second (no-op) run")
	}
	if got := countBackups(t, dir, ".toml.bak."); got != backupsBefore {
		t.Errorf("backups grew on no-op run: %d -> %d", backupsBefore, got)
	}
}

func TestCodexBackupMatchesOriginal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCodexDir, dir)
	path := filepath.Join(dir, "config.toml")
	original := "[otel]\nenvironment = \"control-center\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanCodex(8765)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := plan.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("expected a backup over an existing file")
	}
	if got := readFile(t, backup); got != original {
		t.Errorf("backup content = %q, want %q", got, original)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestSplitLinesMatchesPython(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"\n", []string{""}},
		{"a\n", []string{"a"}},
		{"a\nb", []string{"a", "b"}},
		{"a\n\nb", []string{"a", "", "b"}},
		{"a\n\n", []string{"a", ""}},
	}
	for _, c := range cases {
		got := splitLines(c.in)
		if !equalLines(got, c.want) {
			t.Errorf("splitLines(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func equalLines(a, b []string) bool {
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

func TestEnsureTableKeyIdempotent(t *testing.T) {
	text, added := ensureTableKey("", "otel", "environment", `"control-center"`)
	if !added {
		t.Fatal("first insert should add")
	}
	if _, added := ensureTableKey(text, "otel", "environment", `"control-center"`); added {
		t.Error("second insert should be a no-op")
	}
}
