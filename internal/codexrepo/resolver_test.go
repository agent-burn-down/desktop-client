package codexrepo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const testConversationID = "019f6448-f7ea-72a0-a1c1-afaf3eabbad0"

func TestResolveUsesLatestTurnContextAndRefreshesCache(t *testing.T) {
	home := t.TempDir()
	first := filepath.Join(t.TempDir(), "first-repo")
	latest := filepath.Join(t.TempDir(), "latest-repo")
	mustMkdir(t, first)
	mustMkdir(t, latest)
	path := sessionPath(home, testConversationID)
	writeSession(t, path,
		line("session_meta", testConversationID, first)+
			line("turn_context", "", latest))

	r := New(home)
	if got := r.Resolve(testConversationID); got != "latest-repo" {
		t.Fatalf("Resolve = %q, want latest-repo", got)
	}

	next := filepath.Join(t.TempDir(), "next-repo")
	mustMkdir(t, next)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(line("turn_context", "", next))
	_ = f.Close()
	if got := r.Resolve(testConversationID); got != "next-repo" {
		t.Fatalf("Resolve after append = %q, want next-repo", got)
	}
}

func TestResolveFindsArchivedSession(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(t.TempDir(), "archived-repo")
	mustMkdir(t, repo)
	path := filepath.Join(home, "archived_sessions", "rollout-2026-07-14T00-00-00-"+testConversationID+".jsonl")
	writeSession(t, path, line("session_meta", testConversationID, repo))
	if got := New(home).Resolve(testConversationID); got != "archived-repo" {
		t.Fatalf("Resolve archived = %q, want archived-repo", got)
	}
}

func TestResolveMultipleConversationsIndependently(t *testing.T) {
	home := t.TempDir()
	ids := []string{
		"019f6448-f7ea-72a0-a1c1-afaf3eabbad0",
		"019f645b-f71a-70f0-aaea-79e7b3ba413b",
	}
	for i, id := range ids {
		repo := filepath.Join(t.TempDir(), fmt.Sprintf("simultaneous-repo-%d", i+1))
		mustMkdir(t, repo)
		writeSession(t, sessionPath(home, id), line("session_meta", id, repo))
	}

	r := New(home)
	for i, id := range ids {
		want := fmt.Sprintf("simultaneous-repo-%d", i+1)
		if got := r.Resolve(id); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestResolveCollapsesGitWorktreeToCanonicalRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	mainRepo := filepath.Join(t.TempDir(), "canonical-repo")
	mustRun(t, "git", "init", mainRepo)
	mustGitIn(t, mainRepo, "config", "user.email", "test@example.com")
	mustGitIn(t, mainRepo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(mainRepo, "README"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGitIn(t, mainRepo, "add", "README")
	mustGitIn(t, mainRepo, "commit", "-m", "init")
	worktree := filepath.Join(t.TempDir(), "feature-checkout")
	mustGitIn(t, mainRepo, "worktree", "add", "-b", "feature", worktree)
	path := sessionPath(home, testConversationID)
	writeSession(t, path, line("session_meta", testConversationID, filepath.Join(worktree, "nested")))
	mustMkdir(t, filepath.Join(worktree, "nested"))

	if got := New(home).Resolve(testConversationID); got != "canonical-repo" {
		t.Fatalf("Resolve worktree = %q, want canonical-repo", got)
	}
}

func TestResolveMissingMalformedAndUnreadableDegradesSafely(t *testing.T) {
	home := t.TempDir()
	path := sessionPath(home, testConversationID)
	writeSession(t, path, "not json\n"+line("session_meta", testConversationID, ""))
	r := New(home)
	for _, id := range []string{"", "not-a-uuid", testConversationID} {
		if got := r.Resolve(id); got != "" {
			t.Fatalf("Resolve(%q) = %q, want unknown", id, got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve(testConversationID); got != "" {
		t.Fatalf("Resolve removed file = %q, want unknown", got)
	}
}

func TestCollapseKnownWorktreePaths(t *testing.T) {
	for _, path := range []string{
		"/Users/test/code/repo/.claude/worktrees/fix/subdir",
		"/Users/test/code/repo/.codex/worktrees/fix/subdir",
		"/Users/test/code/repo/.worktrees/fix/subdir",
	} {
		if got := canonicalRepo(path); got != "repo" {
			t.Errorf("canonicalRepo(%q) = %q, want repo", path, got)
		}
	}
}

func sessionPath(home, id string) string {
	return filepath.Join(home, "sessions", "2026", "07", "14", "rollout-2026-07-14T00-00-00-"+id+".jsonl")
}

func line(kind, id, cwd string) string {
	return fmt.Sprintf("{\"type\":%q,\"payload\":{\"id\":%q,\"cwd\":%q}}\n", kind, id, cwd)
}

func writeSession(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func mustGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
