package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// tableHeaderRe matches a TOML table header line such as "[otel]" or
// "[otel.exporter.otlp-http]", ignoring surrounding whitespace. A line only
// counts as a header when the bracket scanner is also at depth 0 (see
// lineDepths), so array elements like "[3, 4]" are never mistaken for a table.
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
//
// When the edit would change anything, the resulting text is parsed with a real
// TOML parser and rejected (the file is left untouched) if it is not valid TOML
// or would contain duplicate keys, so a hand-edit can never corrupt the file.
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
	text, changes := editCodex(text, port)
	plan := &CodexPlan{Path: path, text: text, changes: changes}
	if !plan.Empty() {
		if err := validateCodexTOML(text); err != nil {
			return nil, fmt.Errorf(
				"refusing to edit %s: the change would produce invalid TOML (%w); "+
					"file left unchanged", path, err)
		}
	}
	return plan, nil
}

// editCodex applies the four scalar-key insertions and the exporter edit,
// returning the new text and a description of each change made.
func editCodex(text string, port int) (string, []string) {
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
	return text, changes
}

// validateCodexTOML parses text with a real TOML parser. BurntSushi rejects
// duplicate keys within a table (a TOML spec violation), so a nil error implies
// both syntactic validity and no duplicate keys.
func validateCodexTOML(text string) error {
	var doc map[string]any
	_, err := toml.Decode(text, &doc)
	return err
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

// ensureTableKey inserts "key = value" into the [table] table when no top-level
// "key =" assignment already exists in it. A missing table is appended. Existing
// values are preserved. Returns the new text and whether a line was added.
func ensureTableKey(text, table, key, value string) (string, bool) {
	lines := splitLines(text)
	depths := lineDepths(lines)
	start := findTable(lines, depths, table)
	if start < 0 {
		return appendTable(lines, table, key+" = "+value), true
	}
	end := tableEnd(lines, depths, start)
	if keyPresent(lines, depths, start+1, end, key) {
		return ensureTrailingNewline(text), false
	}
	lines = insertAt(lines, end, key+" = "+value)
	return strings.Join(lines, "\n") + "\n", true
}

// replaceOrInsertTableKey sets "key = value" in the [table] table, replacing an
// existing differing value or inserting when absent, and reports whether it
// changed anything.
func replaceOrInsertTableKey(text, table, key, value string) (string, bool) {
	lines := splitLines(text)
	depths := lineDepths(lines)
	start := findTable(lines, depths, table)
	if start < 0 {
		return appendTable(lines, table, key+" = "+value), true
	}
	end := tableEnd(lines, depths, start)
	wanted := key + " = " + value
	keyPrefix := key + " ="
	for idx := start + 1; idx < end; idx++ {
		if depths[idx] != 0 {
			continue
		}
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
	depths := lineDepths(lines)
	start := findTable(lines, depths, table)
	if start < 0 {
		return ensureTrailingNewline(text), false
	}
	end := tableEnd(lines, depths, start)
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

// keyPresent reports whether a top-level "key =" assignment exists among body
// lines [from, to). Lines inside multi-line arrays or inline tables (depth > 0)
// never count.
func keyPresent(lines []string, depths []int, from, to int, key string) bool {
	wanted := key + " ="
	for idx := from; idx < to; idx++ {
		if depths[idx] != 0 {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(lines[idx]), wanted) {
			return true
		}
	}
	return false
}

// findTable returns the index of the header line for the given dotted table
// path (comparing unquoted key paths so [a."b"] and [a.b] match), or -1.
func findTable(lines []string, depths []int, table string) int {
	for idx, line := range lines {
		if depths[idx] == 0 && headerMatches(line, table) {
			return idx
		}
	}
	return -1
}

// tableEnd returns the index one past the last body line of the table starting
// at start: the next top-level table header, or end of file.
func tableEnd(lines []string, depths []int, start int) int {
	for idx := start + 1; idx < len(lines); idx++ {
		if depths[idx] == 0 && tableHeaderRe.MatchString(lines[idx]) {
			return idx
		}
	}
	return len(lines)
}

// headerMatches reports whether line is a table header whose dotted key path
// equals table's, treating quoted and bare segments as equivalent.
func headerMatches(line, table string) bool {
	path, ok := headerKeyPath(line)
	if !ok {
		return false
	}
	return equalPath(path, splitDottedKey(table))
}

// headerKeyPath extracts the dotted key path of a table header line.
func headerKeyPath(line string) ([]string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") || !tableHeaderRe.MatchString(s) {
		return nil, false
	}
	return splitDottedKey(s[1 : len(s)-1]), true
}

// splitDottedKey splits a dotted TOML key path on '.', trimming whitespace and
// stripping surrounding quotes from each segment.
func splitDottedKey(s string) []string {
	raw := strings.Split(s, ".")
	parts := make([]string, len(raw))
	for i, seg := range raw {
		parts[i] = unquoteSegment(strings.TrimSpace(seg))
	}
	return parts
}

func unquoteSegment(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func equalPath(a, b []string) bool {
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

// lineDepths returns the bracket nesting depth ('[' and '{') at the start of
// each line, ignoring brackets inside quoted strings and after '#' comments.
// A line at depth 0 can be a table header or a top-level key; a line at depth
// > 0 is inside a multi-line array or inline table.
func lineDepths(lines []string) []int {
	depths := make([]int, len(lines))
	depth := 0
	for i, line := range lines {
		depths[i] = depth
		depth += lineDelta(line)
		if depth < 0 {
			depth = 0
		}
	}
	return depths
}

// lineDelta is the net change in bracket depth contributed by a line, counting
// only brackets outside strings and comments.
func lineDelta(line string) int {
	delta := 0
	for _, c := range stripStringsAndComments(line) {
		switch c {
		case '[', '{':
			delta++
		case ']', '}':
			delta--
		}
	}
	return delta
}

// stripStringsAndComments returns line with the contents of quoted strings and
// any trailing '#' comment removed, so bracket counting only sees structural
// brackets. Escapes inside basic strings are not decoded (minimal handling);
// the post-edit TOML parse is the real correctness guarantee.
func stripStringsAndComments(line string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '#':
			return b.String()
		case '"', '\'':
			quote = c
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
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
