// Package inventory discovers only display-safe metadata from supported agent
// installations. Discovery is invoked exclusively after server policy enables
// inventory consent; it never returns file paths or configuration bodies.
package inventory

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/agent-burn-down/desktop-client/internal/api"
)

const (
	maxWalkDepth = 7
	maxWalkFiles = 20_000
)

// Roots makes discovery deterministic in tests and supports the same agent
// directory overrides as setup. Empty roots are resolved from the user home.
type Roots struct {
	Codex  string
	Claude string
}

func DefaultRoots() Roots {
	home, _ := os.UserHomeDir()
	return Roots{
		Codex:  envOr("BURNDOWN_CODEX_DIR", filepath.Join(home, ".codex")),
		Claude: envOr("BURNDOWN_CLAUDE_DIR", filepath.Join(home, ".claude")),
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// Discover returns a deterministic, de-duplicated snapshot capped at the
// backend contract limit. It reads names/counts only; values from MCP config
// bodies and Skill/context file contents are never copied or returned.
func Discover(ctx context.Context, roots Roots) ([]api.InventoryItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := make([]api.InventoryItem, 0)
	for _, agent := range []struct {
		root, source, contextFile, contextName string
	}{
		{roots.Codex, "codex", "AGENTS.md", "user instructions"},
		{roots.Claude, "claude-code", "CLAUDE.md", "user instructions"},
	} {
		if agent.root == "" {
			continue
		}
		items = append(items, discoverSkills(ctx, filepath.Join(agent.root, "skills"), agent.source)...)
		items = append(items, discoverPlugins(ctx, filepath.Join(agent.root, "plugins"), agent.source)...)
		if fileExists(filepath.Join(agent.root, agent.contextFile)) {
			items = append(items, item("context", agent.contextName, agent.source))
		}
	}
	items = append(items, discoverCodexMCP(ctx, filepath.Join(roots.Codex, "config.toml"))...)
	claudeConfig := filepath.Join(filepath.Dir(roots.Claude), ".claude.json")
	items = append(items, discoverClaudeMCP(ctx, claudeConfig)...)
	items = dedupe(items)
	if len(items) > api.MaxInventoryItems {
		items = items[:api.MaxInventoryItems]
	}
	return items, ctx.Err()
}

func discoverSkills(ctx context.Context, root, source string) []api.InventoryItem {
	var out []api.InventoryItem
	_ = walkBounded(ctx, root, func(path string, entry fs.DirEntry) {
		if entry.IsDir() || !strings.EqualFold(entry.Name(), "SKILL.md") {
			return
		}
		name := safeText(filepath.Base(filepath.Dir(path)), 160)
		if name == "" {
			return
		}
		found := item("skill", name, source)
		count := countRegularFiles(ctx, filepath.Join(filepath.Dir(path), "scripts"), 100_000)
		if count > 0 {
			n := int64(count)
			found.ScriptCount = &n
		}
		out = append(out, found)
	})
	return out
}

type pluginManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

func discoverPlugins(ctx context.Context, root, source string) []api.InventoryItem {
	var out []api.InventoryItem
	_ = walkBounded(ctx, root, func(path string, entry fs.DirEntry) {
		if entry.IsDir() || !strings.EqualFold(entry.Name(), "plugin.json") {
			return
		}
		if found := pluginAt(path, source); found != nil {
			out = append(out, *found)
		}
	})
	return out
}

func pluginAt(path, source string) *api.InventoryItem {
	manifest, ok := loadPluginManifest(path)
	if !ok {
		return nil
	}
	name := pluginName(path, manifest.Name)
	if name == "" {
		return nil
	}
	found := item("plugin", name, source)
	if value := safeText(manifest.Description, 300); value != "" {
		found.Description = &value
	}
	if value := safeText(manifest.Version, 80); value != "" {
		found.Version = &value
	}
	return &found
}

func loadPluginManifest(path string) (pluginManifest, bool) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > 256*1024 {
		return pluginManifest{}, false
	}
	//nolint:gosec // G304: path is bounded beneath the selected agent plugin root.
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginManifest{}, false
	}
	var manifest pluginManifest
	if json.Unmarshal(data, &manifest) != nil {
		return pluginManifest{}, false
	}
	return manifest, true
}

func pluginName(path, configured string) string {
	name := safeText(configured, 160)
	if name == "" {
		parent := filepath.Dir(path)
		if strings.HasPrefix(filepath.Base(parent), ".") {
			parent = filepath.Dir(parent)
		}
		name = safeText(filepath.Base(parent), 160)
	}
	return name
}

func discoverCodexMCP(ctx context.Context, path string) []api.InventoryItem {
	//nolint:gosec // G304: path is the fixed config.toml beneath the selected Codex root.
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var out []api.InventoryItem
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if ctx.Err() != nil {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "[mcp_servers.") {
			continue
		}
		line = strings.TrimSuffix(strings.TrimSpace(strings.SplitN(line, "#", 2)[0]), "]")
		raw := strings.TrimPrefix(line, "[mcp_servers.")
		name, err := strconv.Unquote(raw)
		if err != nil {
			name = strings.Trim(raw, "'")
		}
		if name = safeText(name, 160); name != "" {
			out = append(out, item("mcp", name, "codex"))
		}
	}
	return out
}

func discoverClaudeMCP(ctx context.Context, path string) []api.InventoryItem {
	if ctx.Err() != nil {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 4*1024*1024 {
		return nil
	}
	//nolint:gosec // G304: path is the fixed .claude.json beside the selected Claude root.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var config struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &config) != nil {
		return nil
	}
	out := make([]api.InventoryItem, 0, len(config.MCPServers))
	for name := range config.MCPServers {
		if value := safeText(name, 160); value != "" {
			out = append(out, item("mcp", value, "claude-code"))
		}
	}
	return out
}

func item(kind, name, source string) api.InventoryItem {
	present := true
	return api.InventoryItem{Kind: kind, Name: name, Source: source, Present: &present}
}

func dedupe(items []api.InventoryItem) []api.InventoryItem {
	sort.Slice(items, func(i, j int) bool {
		a := items[i].Kind + "\x00" + items[i].Source + "\x00" + strings.ToLower(items[i].Name)
		b := items[j].Kind + "\x00" + items[j].Source + "\x00" + strings.ToLower(items[j].Name)
		return a < b
	})
	out := items[:0]
	last := ""
	for _, entry := range items {
		key := entry.Kind + "\x00" + entry.Source + "\x00" + strings.ToLower(entry.Name)
		if key != last {
			out = append(out, entry)
			last = key
		}
	}
	return out
}

func safeText(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if unsafeText(value, max) {
		return ""
	}
	return value
}

func unsafeText(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "/\\\x00\r\n") {
		return true
	}
	if hasForbiddenMarker(strings.ToLower(value)) {
		return true
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func hasForbiddenMarker(lower string) bool {
	forbidden := []string{
		"/users/", "/home/", `\users\`, `\home\`, "~/", `~\`, "-----begin ",
		"api_key", "api-key", "secret=", "token=", "password=", "authorization:",
		"bearer ", "env:", "environment=", "environment:", "config:", "config=",
		"prompt:", "system prompt", "client_secret", "aws_access_key_id",
		"aws_secret_access_key", "private key", "ghp_", "sk-proj-",
	}
	for _, marker := range forbidden {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func walkBounded(ctx context.Context, root string, visit func(string, fs.DirEntry)) error {
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	seen := 0
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			// Permission/race failures omit that path without aborting the snapshot.
			//nolint:nilerr
			return nil
		}
		seen++
		depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
		skip, decision := walkDecision(entry, depth, seen)
		if decision != nil || skip {
			return decision
		}
		visit(path, entry)
		return nil
	})
}

func walkDecision(entry fs.DirEntry, depth, seen int) (bool, error) {
	if seen > maxWalkFiles {
		return true, fs.SkipAll
	}
	if entry.IsDir() && depth > maxWalkDepth {
		return true, fs.SkipDir
	}
	if entry.Type()&os.ModeSymlink != 0 {
		if entry.IsDir() {
			return true, fs.SkipDir
		}
		return true, nil
	}
	return false, nil
}

func countRegularFiles(ctx context.Context, root string, max int) int {
	count := 0
	_ = walkBounded(ctx, root, func(_ string, entry fs.DirEntry) {
		if entry.Type().IsRegular() && count < max {
			count++
		}
	})
	return count
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
