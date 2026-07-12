package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// assertValidTOML parses text and fails if it is not valid TOML, returning the
// decoded document for further assertions.
func assertValidTOML(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc map[string]any
	if _, err := toml.Decode(text, &doc); err != nil {
		t.Fatalf("result is not valid TOML: %v\n---\n%s", err, text)
	}
	return doc
}

// countKeyLines counts top-level "key =" assignments in text (naive, for
// duplicate detection in tests).
func countKeyLines(text, key string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+" =") {
			n++
		}
	}
	return n
}

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

// applyCodex is a test helper: plan and apply for a temp Codex dir, returning
// the resulting config.toml contents.
func applyCodex(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv(EnvCodexDir, dir)
	plan, err := PlanCodex(8765)
	if err != nil {
		t.Fatalf("PlanCodex: %v", err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return readFile(t, filepath.Join(dir, "config.toml"))
}

func TestPlanCodexMultiLineArrayNotCorrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// The exact CRITICAL repro: nested multi-line arrays, then a real key below.
	existing := "[otel]\n" +
		"weird_array = [\n" +
		"  [1, 2],\n" +
		"  [3, 4]\n" +
		"]\n" +
		"metrics_exporter = \"otlp\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	got := applyCodex(t, dir)

	doc := assertValidTOML(t, got)
	otel, _ := doc["otel"].(map[string]any)
	if otel["metrics_exporter"] != "otlp" {
		t.Errorf("user metrics_exporter changed to %v, want \"otlp\"", otel["metrics_exporter"])
	}
	if n := countKeyLines(got, "metrics_exporter"); n != 1 {
		t.Errorf("metrics_exporter appears %d times, want 1 (no duplicate)\n%s", n, got)
	}
	if otel["environment"] != "control-center" {
		t.Errorf("environment not inserted correctly: %v", otel["environment"])
	}
	if !strings.Contains(got, "weird_array") || otel["weird_array"] == nil {
		t.Errorf("user array lost:\n%s", got)
	}

	// Second run must be a byte-identical no-op.
	plan2, err := PlanCodex(8765)
	if err != nil {
		t.Fatalf("second PlanCodex: %v", err)
	}
	if !plan2.Empty() {
		t.Errorf("second run not a no-op: %v", plan2.Descriptions())
	}
	if plan2.text != got {
		t.Errorf("second-run text differs from applied file:\n--- got ---\n%s\n--- want ---\n%s",
			plan2.text, got)
	}
}

func TestPlanCodexMultiLineInlineTablePreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// A multi-line inline table inside [otel]; a real key sits after it.
	existing := "[otel]\n" +
		"nested = {\n" +
		"  a = 1,\n" +
		"  b = 2\n" +
		"}\n" +
		"trace_exporter = \"otlp\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	got := applyCodex(t, dir)

	doc := assertValidTOML(t, got)
	otel, _ := doc["otel"].(map[string]any)
	if otel["trace_exporter"] != "otlp" {
		t.Errorf("user trace_exporter changed to %v, want \"otlp\"", otel["trace_exporter"])
	}
	if n := countKeyLines(got, "trace_exporter"); n != 1 {
		t.Errorf("trace_exporter appears %d times, want 1\n%s", n, got)
	}
}

func TestPlanCodexBracketInStringValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// A '[' inside a quoted string must not open a bracket depth, and the
	// following keys must still be seen as top-level.
	existing := "[otel]\n" +
		"note = \"contains a [ bracket and a { brace\"\n" +
		"metrics_exporter = \"otlp\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	got := applyCodex(t, dir)

	doc := assertValidTOML(t, got)
	otel, _ := doc["otel"].(map[string]any)
	if otel["metrics_exporter"] != "otlp" {
		t.Errorf("user metrics_exporter changed: %v", otel["metrics_exporter"])
	}
	if n := countKeyLines(got, "metrics_exporter"); n != 1 {
		t.Errorf("metrics_exporter duplicated: %d\n%s", n, got)
	}
}

func TestPlanCodexRemovesStaleTableRegardlessOfQuoting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// The stale nested table is written with a quoted last segment and odd
	// spacing; header normalization must still match and remove it.
	existing := "[otel]\n" +
		"environment = \"control-center\"\n" +
		"metrics_exporter = \"none\"\n" +
		"trace_exporter = \"none\"\n" +
		"log_user_prompt = false\n" +
		"\n[ otel.exporter.\"otlp-http\" ]\n" +
		"endpoint = \"http://127.0.0.1:1/v1/logs\"\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	got := applyCodex(t, dir)

	assertValidTOML(t, got)
	if strings.Contains(got, "otlp-http\" ]") || strings.Contains(got, "[ otel.exporter") {
		t.Errorf("stale nested table (odd quoting/spacing) not removed:\n%s", got)
	}
}

func TestValidateCodexTOMLRejectsDuplicateKeys(t *testing.T) {
	if err := validateCodexTOML("[otel]\nx = 1\nx = 2\n"); err == nil {
		t.Error("expected duplicate-key rejection")
	}
	if err := validateCodexTOML("[otel]\nx = 1\n"); err != nil {
		t.Errorf("valid TOML rejected: %v", err)
	}
}
