package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// tableHeaderRe matches a TOML table header line such as "[otel]" or
// "[otel.exporter.otlp-http]", ignoring surrounding whitespace.
var tableHeaderRe = regexp.MustCompile(`^\s*\[[^\]]+\]\s*$`)

// CodexPlan is the pending change set for Codex's config.toml.
type CodexPlan struct {
	// Path is the config.toml file the plan targets.
	Path string
	// text is the full desired file contents after edits.
	text string
	// changes describes each edit for the plan output.
	changes []string
}

// Empty reports whether the plan would change nothing.
func (p *CodexPlan) Empty() bool { return len(p.changes) == 0 }

// Descriptions returns the human-readable edit lines.
func (p *CodexPlan) Descriptions() []string { return p.changes }

// PlanCodex computes the pending config.toml edits for the given receiver port.
// The four scalar [otel] keys are inserted only when absent (existing values
// preserved); otel.exporter is replaced-or-inserted; and any stale nested
// otel.exporter."otlp-http" / otel.exporter.otlp-http tables are removed.
func PlanCodex(port int) (*CodexPlan, error) {
	dir, err := CodexDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "config.toml")
	text, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/v1/logs", port)
	desiredExporter := fmt.Sprintf(
		`{ otlp-http = { endpoint = "%s", protocol = "json" } }`, endpoint)

	var changes []string
	for _, kv := range []struct{ key, value, desc string }{
		{"environment", `"control-center"`, `otel.environment = "control-center"`},
		{"metrics_exporter", `"none"`, `otel.metrics_exporter = "none"`},
		{"trace_exporter", `"none"`, `otel.trace_exporter = "none"`},
		{"log_user_prompt", "false", "otel.log_user_prompt = false"},
	} {
		var added bool
		text, added = ensureTableKey(text, "otel", kv.key, kv.value)
		if added {
			changes = append(changes, kv.desc)
		}
	}
	var changed bool
	text, changed = ensureCodexExporter(text, desiredExporter)
	if changed {
		changes = append(changes, "otel.exporter = "+desiredExporter)
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return &CodexPlan{Path: path, text: text, changes: changes}, nil
}

// Apply backs up the existing config.toml (if any) and writes the new contents.
// The backup path is returned, empty when no prior file existed.
func (p *CodexPlan) Apply() (backup string, err error) {
	backup, err = backupFile(p.Path, ".toml.bak.")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(p.Path), err)
	}
	if err := os.WriteFile(p.Path, []byte(p.text), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", p.Path, err)
	}
	return backup, nil
}

func readIfExists(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the agent's own config file
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// ensureCodexExporter replaces-or-inserts otel.exporter and strips stale nested
// exporter tables, reporting whether anything changed.
func ensureCodexExporter(text, rendered string) (string, bool) {
	text, changed := replaceOrInsertTableKey(text, "otel", "exporter", rendered)
	text, removed := removeTable(text, `otel.exporter."otlp-http"`)
	changed = changed || removed
	text, removed = removeTable(text, "otel.exporter.otlp-http")
	changed = changed || removed
	return text, changed
}

// ensureTableKey inserts "key = value" into the [table] table when no line in
// that table already starts with "key =". A missing table is appended. Existing
// values are preserved. Returns the new text and whether a line was added.
func ensureTableKey(text, table, key, value string) (string, bool) {
	lines := splitLines(text)
	start := findTable(lines, table)
	if start < 0 {
		return appendTable(lines, table, key+" = "+value), true
	}
	end := tableEnd(lines, start)
	wanted := key + " ="
	for _, line := range lines[start+1 : end] {
		if strings.HasPrefix(strings.TrimSpace(line), wanted) {
			return ensureTrailingNewline(text), false
		}
	}
	lines = insertAt(lines, end, key+" = "+value)
	return strings.Join(lines, "\n") + "\n", true
}

// replaceOrInsertTableKey sets "key = value" in the [table] table, replacing an
// existing differing value or inserting when absent, and reports whether it
// changed anything.
func replaceOrInsertTableKey(text, table, key, value string) (string, bool) {
	lines := splitLines(text)
	start := findTable(lines, table)
	if start < 0 {
		return appendTable(lines, table, key+" = "+value), true
	}
	end := tableEnd(lines, start)
	wanted := key + " = " + value
	keyPrefix := key + " ="
	for idx := start + 1; idx < end; idx++ {
		line := strings.TrimSpace(lines[idx])
		if !strings.HasPrefix(line, keyPrefix) {
			continue
		}
		if line == wanted {
			return strings.Join(lines, "\n") + "\n", false
		}
		lines[idx] = wanted
		return strings.Join(lines, "\n") + "\n", true
	}
	lines = insertAt(lines, end, wanted)
	return strings.Join(lines, "\n") + "\n", true
}

// removeTable deletes the "[table]" section (its header and body up to the next
// table header), collapsing a resulting double blank line, and reports whether
// it removed anything.
func removeTable(text, table string) (string, bool) {
	lines := splitLines(text)
	start := findTable(lines, table)
	if start < 0 {
		return ensureTrailingNewline(text), false
	}
	end := tableEnd(lines, start)
	lines = append(lines[:start], lines[end:]...)
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" &&
		start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		lines = append(lines[:start], lines[start+1:]...)
	}
	updated := strings.Join(lines, "\n")
	if updated != "" && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	return updated, true
}

// findTable returns the index of the "[table]" header line, or -1 if absent.
func findTable(lines []string, table string) int {
	header := "[" + table + "]"
	for idx, line := range lines {
		if strings.TrimSpace(line) == header {
			return idx
		}
	}
	return -1
}

// tableEnd returns the index one past the last body line of the table starting
// at start (the next table header or end of file).
func tableEnd(lines []string, start int) int {
	for idx := start + 1; idx < len(lines); idx++ {
		if tableHeaderRe.MatchString(lines[idx]) {
			return idx
		}
	}
	return len(lines)
}

// appendTable appends a blank separator (when needed), the "[table]" header, and
// a single body line, returning the joined text with a trailing newline.
func appendTable(lines []string, table, bodyLine string) string {
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
		lines = append(lines, "")
	}
	lines = append(lines, "["+table+"]", bodyLine)
	return strings.Join(lines, "\n") + "\n"
}

func insertAt(lines []string, idx int, s string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, s)
	out = append(out, lines[idx:]...)
	return out
}

// splitLines mirrors Python's str.splitlines() for "\n"-delimited text: a
// trailing newline does not yield a final empty element, and the empty string
// yields no lines.
func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func ensureTrailingNewline(text string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return text
	}
	return text + "\n"
}
