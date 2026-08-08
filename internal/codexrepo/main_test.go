package codexrepo

import (
	"os"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// TestMain sandboxes every test in this package to a throwaway config
// directory before any test runs. Resolve/ResolveClaude now compute RepoKey
// as part of populating their cache entry, and RepoKey's local: tier
// persists a salt file under config.Dir() -- without this, every test in the
// package would read and write the developer's real ~/.burndown directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "codexrepo-salt-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv(config.EnvConfigDir, dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
