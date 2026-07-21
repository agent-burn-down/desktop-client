package inventory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverSanitizedAgentInventory(t *testing.T) {
	home := t.TempDir()
	codex := filepath.Join(home, ".codex")
	claude := filepath.Join(home, ".claude")
	mustWrite(t, filepath.Join(codex, "skills", "deploy-check", "SKILL.md"), "SECRET PROMPT BODY")
	mustWrite(t, filepath.Join(codex, "skills", "deploy-check", "scripts", "check.sh"), "token=secret")
	mustWrite(t, filepath.Join(codex, "skills", "token=secret", "SKILL.md"), "ignored")
	mustWrite(t, filepath.Join(codex, "plugins", "cache", "release-tools", ".codex-plugin", "plugin.json"),
		`{"name":"release-tools","description":"Release helpers","version":"1.2.0","config":"SECRET"}`)
	mustWrite(t, filepath.Join(codex, "config.toml"), `[mcp_servers.github]
command = "/Users/alice/bin/server"
env = { API_KEY = "SECRET" }
[mcp_servers."filesystem"]
args = ["/Users/alice/private"]
`)
	mustWrite(t, filepath.Join(codex, "AGENTS.md"), "PRIVATE PROJECT INSTRUCTIONS")

	mustWrite(t, filepath.Join(claude, "skills", "review", "SKILL.md"), "PROMPT CONTENT")
	mustWrite(t, filepath.Join(claude, "plugins", "quality", ".claude-plugin", "plugin.json"),
		`{"name":"quality","description":"token=SECRET","version":"2.0.0"}`)
	mustWrite(t, filepath.Join(claude, "CLAUDE.md"), "PRIVATE CLAUDE INSTRUCTIONS")
	mustWrite(t, filepath.Join(home, ".claude.json"),
		`{"mcpServers":{"browser":{"command":"/secret","env":{"TOKEN":"SECRET"}},"/unsafe/path":{}}}`)

	items, err := Discover(context.Background(), Roots{Codex: codex, Claude: claude})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"context|codex|user instructions":       false,
		"context|claude-code|user instructions": false,
		"mcp|codex|github":                      false,
		"mcp|codex|filesystem":                  false,
		"mcp|claude-code|browser":               false,
		"plugin|codex|release-tools":            false,
		"plugin|claude-code|quality":            false,
		"skill|codex|deploy-check":              false,
		"skill|claude-code|review":              false,
	}
	for _, entry := range items {
		key := entry.Kind + "|" + entry.Source + "|" + entry.Name
		if _, ok := want[key]; ok {
			want[key] = true
		}
		if entry.Kind == "skill" && entry.Name == "deploy-check" &&
			(entry.ScriptCount == nil || *entry.ScriptCount != 1) {
			t.Fatalf("script_count = %v, want 1", entry.ScriptCount)
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing %s in %+v", key, items)
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"SECRET", "/Users/", "/unsafe/path", "PROMPT CONTENT", "PRIVATE", "API_KEY", "token=",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("snapshot leaked %q: %s", forbidden, raw)
		}
	}
}

func TestDiscoverHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Discover(ctx, Roots{}); err == nil {
		t.Fatal("canceled discovery should fail")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
