// Package codexrepo resolves agent session IDs to stable repository keys using
// local, metadata-only fields in Codex and Claude session files.
package codexrepo

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxSessionLineBytes  = 16 * 1024 * 1024
	maxProjectLabelRunes = 240
)

// Resolver maps conversation IDs to repository keys. It caches both the
// session-file index and resolved repositories, but reparses active files when
// their size or modification time changes so a later turn_context.cwd wins.
type Resolver struct {
	sessionsDir       string
	archiveDir        string
	claudeProjectsDir string

	mu          sync.Mutex
	index       map[string]string
	cache       map[string]cacheEntry
	claudeIndex map[string]string
	claudeCache map[string]cacheEntry
}

type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
	cwd     string
	repo    string
}

type sessionEvent struct {
	Type    string `json:"type"`
	Payload struct {
		ID  string `json:"id"`
		CWD string `json:"cwd"`
	} `json:"payload"`
}

type claudeSessionEvent struct {
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// New returns a resolver for the configured Codex home. An empty value uses
// CODEX_HOME when set, otherwise ~/.codex.
func New(codexHome string) *Resolver {
	return NewWithHomes(codexHome, "")
}

// NewWithHomes returns a resolver with explicit Codex and Claude configuration
// roots. Empty values use the normal environment and home-directory defaults.
// The two-argument form exists primarily for isolated tests and installations
// that keep agent state outside the default locations.
func NewWithHomes(codexHome, claudeHome string) *Resolver {
	if codexHome == "" {
		codexHome = os.Getenv("CODEX_HOME")
	}
	if codexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	if claudeHome == "" {
		claudeHome = os.Getenv("BURNDOWN_CLAUDE_DIR")
	}
	if claudeHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			claudeHome = filepath.Join(home, ".claude")
		}
	}
	return &Resolver{
		sessionsDir:       filepath.Join(codexHome, "sessions"),
		archiveDir:        filepath.Join(codexHome, "archived_sessions"),
		claudeProjectsDir: filepath.Join(claudeHome, "projects"),
		index:             make(map[string]string),
		cache:             make(map[string]cacheEntry),
		claudeIndex:       make(map[string]string),
		claudeCache:       make(map[string]cacheEntry),
	}
}

// Resolve returns the stable repository key for conversationID. Missing,
// malformed, or unreadable metadata returns "" and never interrupts telemetry.
func (r *Resolver) Resolve(conversationID string) string {
	if !validConversationID(conversationID) {
		return ""
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	path := r.findSession(conversationID)
	if path == "" {
		return ""
	}

	info, err := os.Stat(path)
	if err != nil {
		delete(r.index, conversationID)
		delete(r.cache, conversationID)
		return ""
	}
	if cached, ok := r.cache[conversationID]; ok && cached.path == path &&
		cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached.repo
	}

	cwd := readLatestCWD(path, conversationID)
	repo := canonicalRepo(cwd)
	r.cache[conversationID] = cacheEntry{
		path: path, size: info.Size(), modTime: info.ModTime(), repo: repo,
	}
	return repo
}

// ResolveClaude returns the stable repository key for a Claude session ID.
// Claude stores sessions below ~/.claude/projects, with cwd metadata on the
// individual JSONL records. As with Resolve, malformed or unreadable metadata
// degrades to an empty repository without interrupting telemetry.
func (r *Resolver) ResolveClaude(sessionID string) string {
	if !validConversationID(sessionID) {
		return ""
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	path := r.findClaudeSession(sessionID)
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		delete(r.claudeIndex, sessionID)
		delete(r.claudeCache, sessionID)
		return ""
	}
	cached, cachedOK := r.claudeCache[sessionID]
	if cacheUnchanged(cached, cachedOK, path, info) {
		return cached.repo
	}

	cwd, offset := resumePoint(cached, cachedOK, path, info.Size())
	cwd = readLatestClaudeCWD(path, sessionID, cwd, offset)
	repo := canonicalRepo(cwd)
	r.claudeCache[sessionID] = cacheEntry{
		path: path, size: info.Size(), modTime: info.ModTime(), cwd: cwd, repo: repo,
	}
	return repo
}

func cacheUnchanged(cached cacheEntry, ok bool, path string, info os.FileInfo) bool {
	return ok && cached.path == path && cached.size == info.Size() &&
		cached.modTime.Equal(info.ModTime())
}

func resumePoint(cached cacheEntry, ok bool, path string, size int64) (string, int64) {
	if ok && cached.path == path && cached.size <= size {
		return cached.cwd, cached.size
	}
	return "", 0
}

func (r *Resolver) findSession(conversationID string) string {
	if path := r.index[conversationID]; path != "" {
		return path
	}
	r.refreshIndex()
	return r.index[conversationID]
}

func (r *Resolver) refreshIndex() {
	for _, root := range []string{r.sessionsDir, r.archiveDir} {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
				return nil
			}
			if id := conversationIDFromName(entry.Name()); id != "" {
				// Live sessions are scanned first and win over archived duplicates.
				if _, exists := r.index[id]; !exists {
					r.index[id] = path
				}
			}
			return nil
		})
	}
}

func (r *Resolver) findClaudeSession(sessionID string) string {
	if path := r.claudeIndex[sessionID]; path != "" {
		return path
	}
	r.refreshClaudeIndex()
	return r.claudeIndex[sessionID]
}

func (r *Resolver) refreshClaudeIndex() {
	_ = filepath.WalkDir(r.claudeProjectsDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			return nil
		}
		id := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if validConversationID(id) {
			if _, exists := r.claudeIndex[id]; !exists {
				r.claudeIndex[id] = path
			}
		}
		return nil
	})
}

func conversationIDFromName(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return ""
	}
	// Codex filenames end in a UUID conversation ID. Joining the last five
	// components avoids accepting unrelated timestamps earlier in the name.
	id := strings.Join(parts[len(parts)-5:], "-")
	if !validConversationID(id) {
		return ""
	}
	return id
}

func validConversationID(id string) bool {
	_, err := uuid.Parse(id)
	return len(id) == 36 && err == nil
}

func readLatestCWD(path, conversationID string) string {
	// #nosec G304 -- path is discovered only beneath the configured Codex
	// sessions or archived_sessions roots, never from an OTLP attribute.
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var cwd string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxSessionLineBytes)
	for scanner.Scan() {
		var event sessionEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		cwd = applicableCWD(event, conversationID, cwd)
	}
	return cwd
}

func readLatestClaudeCWD(path, sessionID, current string, offset int64) string {
	// #nosec G304 -- path is discovered only beneath the configured Claude
	// projects root, never from an OTLP attribute.
	f, err := os.Open(path)
	if err != nil {
		return current
	}
	defer func() { _ = f.Close() }()
	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return current
		}
	}

	cwd := current
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxSessionLineBytes)
	for scanner.Scan() {
		var event claudeSessionEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		cwd = applicableClaudeCWD(event, sessionID, cwd)
	}
	return cwd
}

func applicableClaudeCWD(event claudeSessionEvent, sessionID, current string) string {
	if event.CWD == "" || (event.SessionID != "" && event.SessionID != sessionID) {
		return current
	}
	return event.CWD
}

func applicableCWD(event sessionEvent, conversationID, current string) string {
	if event.Payload.CWD == "" {
		return current
	}
	switch event.Type {
	case "session_meta":
		if event.Payload.ID == "" || event.Payload.ID == conversationID {
			return event.Payload.CWD
		}
	case "turn_context":
		return event.Payload.CWD
	}
	return current
}

func canonicalRepo(cwd string) string {
	if strings.TrimSpace(cwd) == "" {
		return ""
	}
	path := filepath.Clean(cwd)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		path = collapseWorktreePath(path)
	}

	if root := gitCommonRoot(path); root != "" {
		return projectLabel(root)
	}
	path = collapseWorktreePath(path)
	return projectLabel(path)
}

func projectLabel(path string) string {
	name := strings.TrimSpace(strings.ToValidUTF8(filepath.Base(path), ""))
	if name == "." || name == string(filepath.Separator) || strings.HasPrefix(name, ".") {
		return ""
	}
	runes := []rune(name)
	if len(runes) > maxProjectLabelRunes {
		name = string(runes[:maxProjectLabelRunes])
	}
	return name
}

func collapseWorktreePath(path string) string {
	slashPath := filepath.ToSlash(path)
	for _, marker := range []string{"/.claude/worktrees/", "/.codex/worktrees/", "/.worktrees/"} {
		if before, _, ok := strings.Cut(slashPath, marker); ok && before != "" {
			return filepath.FromSlash(before)
		}
	}
	return path
}

func gitCommonRoot(path string) string {
	// #nosec G204 -- git and every option are fixed; path is a single argument
	// to -C and is never interpreted by a shell.
	cmd := exec.Command("git", "-C", path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return ""
	}
	if filepath.Base(commonDir) == ".git" {
		return filepath.Dir(commonDir)
	}
	return ""
}
