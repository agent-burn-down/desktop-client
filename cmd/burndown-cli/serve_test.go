package main

import (
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

func TestServeCommandRegistered(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "serve" {
			return
		}
	}
	t.Fatal("serve subcommand not registered on root")
}

func TestLoadServeConfigMissing(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadServeConfig(store)
	if err == nil || !strings.Contains(err.Error(), "burndown-cli login") {
		t.Fatalf("missing-config error = %v, want a login hint", err)
	}
}

func TestLoadServeConfigIncomplete(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	// Registered API URL but no collector id yet: not serve-ready.
	if err := store.Save(&config.Config{APIURL: "https://x", CollectorKey: "yaahc_k"}); err != nil {
		t.Fatal(err)
	}
	_, err = loadServeConfig(store)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete-config error = %v, want an 'incomplete' hint", err)
	}
}
