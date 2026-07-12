package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// claudeKeyOrder fixes the order in which required keys are considered so plan
// descriptions are deterministic. Values come from claudeTarget.
var claudeKeyOrder = []string{
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_PROTOCOL",
	"OTEL_METRICS_EXPORTER",
	"OTEL_LOGS_EXPORTER",
	"OTEL_LOG_TOOL_DETAILS",
}

// claudeTarget returns the required env values, with the OTLP endpoint bound to
// the given receiver port.
func claudeTarget(port int) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  fmt.Sprintf("http://localhost:%d", port),
		"OTEL_EXPORTER_OTLP_PROTOCOL":  "http/json",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_LOG_TOOL_DETAILS":        "1",
	}
}

// ClaudePlan is the pending change set for Claude Code's settings.json.
type ClaudePlan struct {
	// Path is the settings.json file the plan targets.
	Path string
	// additions holds only the keys absent from the existing env object.
	additions map[string]string
	// settings is the full decoded settings document, preserved on write.
	settings map[string]any
	// env is the existing env object (never mutated in place).
	env map[string]any
}

// Empty reports whether the plan would change nothing.
func (p *ClaudePlan) Empty() bool { return len(p.additions) == 0 }

// Descriptions returns human-readable "KEY=value" lines for the pending
// additions, in a stable order.
func (p *ClaudePlan) Descriptions() []string {
	out := make([]string, 0, len(p.additions))
	for _, k := range claudeKeyOrder {
		if v, ok := p.additions[k]; ok {
			out = append(out, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return out
}

// PlanClaude computes the pending settings.json changes for the given receiver
// port. It refuses (returns an error) when the file's env field exists but is
// not a JSON object, and when the file is not valid JSON.
func PlanClaude(port int) (*ClaudePlan, error) {
	dir, err := ClaudeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "settings.json")
	settings, env, err := loadClaudeSettings(path)
	if err != nil {
		return nil, err
	}
	target := claudeTarget(port)
	additions := make(map[string]string)
	for _, k := range claudeKeyOrder {
		if _, present := env[k]; !present {
			additions[k] = target[k]
		}
	}
	return &ClaudePlan{Path: path, additions: additions, settings: settings, env: env}, nil
}

// loadClaudeSettings reads and decodes settings.json, returning the full
// document and its env object. A missing file yields empty maps. A non-object
// env or invalid JSON is an error.
func loadClaudeSettings(path string) (settings, env map[string]any, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the agent's own settings file
	if os.IsNotExist(err) {
		return map[string]any{}, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, nil, fmt.Errorf(
			"%s is not valid JSON; fix or remove it before running setup: %w", path, err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	raw, ok := settings["env"]
	if !ok || raw == nil {
		return settings, map[string]any{}, nil
	}
	env, ok = raw.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf(
			"%s: \"env\" is not a JSON object; refusing to modify it", path)
	}
	return settings, env, nil
}

// Apply backs up the existing settings.json, merges the additions into env
// (preserving every existing key and all other settings), and writes the file
// with 2-space indentation and a trailing newline. The backup path is returned,
// empty when no prior file existed.
func (p *ClaudePlan) Apply() (backup string, err error) {
	backup, err = backupFile(p.Path, ".json.bak.")
	if err != nil {
		return "", err
	}
	merged := make(map[string]any, len(p.env)+len(p.additions))
	for k, v := range p.env {
		merged[k] = v
	}
	for k, v := range p.additions {
		merged[k] = v
	}
	p.settings["env"] = merged
	data, err := json.MarshalIndent(p.settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", p.Path, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(p.Path), err)
	}
	if err := os.WriteFile(p.Path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", p.Path, err)
	}
	return backup, nil
}
