package codexrepo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

func TestNormalizeRemoteExamples(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git@github.com:agent-burn-down/api.git", "github.com/agent-burn-down/api"},
		{"https://github.com/agent-burn-down/api.git", "github.com/agent-burn-down/api"},
		{"https://x-token@github.com/acme/api", "github.com/acme/api"},
		{"ssh://git@gitlab.example.com:2222/team/api.git", "gitlab.example.com/team/api"},
		{"git://github.com/acme/api.git", "github.com/acme/api"},
		{"https://github.com/Acme/API.git", "github.com/acme/api"},
	}
	for _, c := range cases {
		if got := normalizeRemote(c.in); got != c.want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeRemoteStripsUserinfoToken(t *testing.T) {
	got := normalizeRemote("https://ghp_xxxxxxxxxxxxxxxxxxxx@github.com/acme/api")
	if strings.Contains(got, "ghp_") {
		t.Fatalf("normalizeRemote leaked userinfo token: %q", got)
	}
	if got != "github.com/acme/api" {
		t.Fatalf("normalizeRemote = %q, want github.com/acme/api", got)
	}
}

func TestNormalizeRemoteRejectsPathLikeRemotes(t *testing.T) {
	for _, in := range []string{
		"file:///Users/jon/code/api",
		"file://host/Users/jon/code/api",
		"/Users/jon/code/api",
		"../sibling-repo",
		`C:\Users\jon\code\api`,
		"",
		"   ",
	} {
		if got := normalizeRemote(in); got != "" {
			t.Errorf("normalizeRemote(%q) = %q, want \"\" (path-like remote rejected)", in, got)
		}
	}
}

func TestNamedRepoKeySeamAlwaysFallsThroughToday(t *testing.T) {
	// No config surface exists yet (lands in #78); RepoKey must fall through
	// to the remote/hash tiers rather than hard-coding a two-tier chain.
	if got := namedRepoKey("/any/resolved/path"); got != "" {
		t.Fatalf("namedRepoKey = %q, want \"\" (no config surface yet, see #78)", got)
	}
}

func TestRepoKeyEmptyMatchesCanonicalRepoEmpty(t *testing.T) {
	for _, cwd := range []string{"", "   ", "/", "/."} {
		if canonicalRepo(cwd) != "" {
			t.Fatalf("test setup: canonicalRepo(%q) is not empty, fix the fixture", cwd)
		}
		if got := RepoKey(cwd); got != "" {
			t.Errorf("RepoKey(%q) = %q, want \"\" (canonicalRepo is empty)", cwd, got)
		}
	}
}

func TestRepoKeyNonGitDirGetsHashedKeyNotEmpty(t *testing.T) {
	dir := t.TempDir()
	got := RepoKey(dir)
	if got == "" {
		t.Fatal("RepoKey(non-git dir) = \"\", want a local: fallback key")
	}
	if !strings.HasPrefix(got, "local:") {
		t.Fatalf("RepoKey(non-git dir) = %q, want local: prefix", got)
	}
}

func TestRepoKeyLocalFallbackStableAcrossSubdirAndRepeatedCalls(t *testing.T) {
	requireGit(t)
	repo := filepath.Join(t.TempDir(), "no-remote-repo")
	mustRun(t, "init", repo)
	sub := filepath.Join(repo, "nested", "dir")
	mustMkdir(t, sub)

	root := RepoKey(repo)
	nested := RepoKey(sub)
	again := RepoKey(repo)

	if root == "" || !strings.HasPrefix(root, "local:") {
		t.Fatalf("RepoKey(root) = %q, want local: prefix", root)
	}
	if root != nested {
		t.Fatalf("RepoKey(subdir) = %q, want %q (same as repo root)", nested, root)
	}
	if root != again {
		t.Fatalf("RepoKey repeated call = %q, want %q", again, root)
	}
}

func TestRepoKeyWorkspaceRootDistinctFromChildRepo(t *testing.T) {
	requireGit(t)
	workspace := t.TempDir()
	child := filepath.Join(workspace, "api")
	mustRun(t, "init", child)

	workspaceKey := RepoKey(workspace)
	childKey := RepoKey(child)

	if workspaceKey == "" || childKey == "" {
		t.Fatalf("expected non-empty keys, got workspace=%q child=%q", workspaceKey, childKey)
	}
	if workspaceKey == childKey {
		t.Fatalf("workspace and child repo produced the same key: %q", workspaceKey)
	}
}

func TestRepoKeySameRemoteAcrossTwoClonesProducesSameKey(t *testing.T) {
	requireGit(t)
	one := filepath.Join(t.TempDir(), "clone-one")
	two := filepath.Join(t.TempDir(), "clone-two")
	mustRun(t, "init", one)
	mustRun(t, "init", two)
	mustGitIn(t, one, "remote", "add", "origin", "git@github.com:agent-burn-down/api.git")
	mustGitIn(t, two, "remote", "add", "origin", "git@github.com:agent-burn-down/api.git")

	got1, got2 := RepoKey(one), RepoKey(two)
	if got1 != got2 {
		t.Fatalf("RepoKey differs across clones of the same remote: %q vs %q", got1, got2)
	}
	if got1 != "github.com/agent-burn-down/api" {
		t.Fatalf("RepoKey = %q, want github.com/agent-burn-down/api", got1)
	}
}

func TestRepoKeyDifferentRemotesSameNameProduceDifferentKeys(t *testing.T) {
	requireGit(t)
	acme := filepath.Join(t.TempDir(), "api")
	globex := filepath.Join(t.TempDir(), "api")
	mustRun(t, "init", acme)
	mustRun(t, "init", globex)
	mustGitIn(t, acme, "remote", "add", "origin", "https://github.com/acme/api.git")
	mustGitIn(t, globex, "remote", "add", "origin", "https://github.com/globex/api.git")

	got1, got2 := RepoKey(acme), RepoKey(globex)
	if got1 == got2 {
		t.Fatalf("different remotes produced the same key: %q", got1)
	}
}

func TestRepoKeyFileRemoteFallsThroughToHash(t *testing.T) {
	requireGit(t)
	repo := filepath.Join(t.TempDir(), "local-remote-repo")
	mustRun(t, "init", repo)
	mustGitIn(t, repo, "remote", "add", "origin", "file:///Users/jon/code/api")

	got := RepoKey(repo)
	if !strings.HasPrefix(got, "local:") {
		t.Fatalf("RepoKey with file:// remote = %q, want local: fallback", got)
	}
}

func TestRepoKeyNeverLeaksLocalPathMarkers(t *testing.T) {
	paths := []string{
		"/Users/jon/clients/unannounced-acquisition/api",
		"/home/jon/code/repo",
		"~/code/repo",
		`C:\Users\jon\code\repo`,
	}
	driveLetter := regexp.MustCompile(`(^|/)[a-z]:[\\/]`)
	for _, p := range paths {
		got := RepoKey(p)
		low := strings.ToLower(got)
		for _, forbidden := range []string{"/users/", "/home/", "~"} {
			if strings.Contains(low, forbidden) {
				t.Errorf("RepoKey(%q) = %q, leaked %q", p, got, forbidden)
			}
		}
		if driveLetter.MatchString(low) {
			t.Errorf("RepoKey(%q) = %q, looks like it leaked a drive letter", p, got)
		}
	}
}

func TestRepoKeyFallbackHashUsesPersistedSalt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)
	if err := os.WriteFile(filepath.Join(dir, saltFile), []byte("deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := "/some/never/created/repo"
	want := "local:" + fallbackHash(resolvedRepoPath(repo))
	if got := RepoKey(repo); got != want {
		t.Fatalf("RepoKey = %q, want %q (computed from the persisted salt)", got, want)
	}
}

func TestFallbackSaltPersistsAcrossSimulatedReinstall(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvConfigDir, dir)

	repo := t.TempDir()
	first := RepoKey(repo)
	if first == "" || !strings.HasPrefix(first, "local:") {
		t.Fatalf("RepoKey = %q, want local: prefix", first)
	}

	saltPath := filepath.Join(dir, saltFile)
	saltBefore, err := os.ReadFile(saltPath)
	if err != nil {
		t.Fatalf("read persisted salt: %v", err)
	}

	// A reinstall/upgrade doesn't wipe the config directory, so nothing here
	// deletes saltPath -- a fresh call must load the existing salt rather
	// than mint a new one.
	second := RepoKey(repo)
	saltAfter, err := os.ReadFile(saltPath)
	if err != nil {
		t.Fatalf("read persisted salt after second call: %v", err)
	}
	if !bytes.Equal(saltBefore, saltAfter) {
		t.Fatalf("salt file changed across calls: %q -> %q", saltBefore, saltAfter)
	}
	if first != second {
		t.Fatalf("RepoKey changed across simulated reinstall: %q -> %q", first, second)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}
