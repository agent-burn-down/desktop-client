package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/version"
)

func TestVersionFlag(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--version: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "burndown-cli ") || !strings.Contains(got, version.Version) {
		t.Errorf("--version output = %q, want prefix %q and version %q", got, "burndown-cli ", version.Version)
	}
}
