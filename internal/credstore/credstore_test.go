package credstore

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// fakeBackend is an in-memory Backend for hermetic tests: it never touches a
// real OS keychain. setErr/getErr simulate the failure modes a real
// keychain can hit (locked, denied ACL, headless session), per account so a
// failure on one credential can be tested without affecting the other.
type fakeBackend struct {
	mu       sync.Mutex
	data     map[Account]string
	setErr   map[Account]error
	getErr   map[Account]error
	setCalls int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		data:   map[Account]string{},
		setErr: map[Account]error{},
		getErr: map[Account]error{},
	}
}

func (f *fakeBackend) Name() string { return BackendKeychain }

func (f *fakeBackend) Get(account Account) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.getErr[account]; err != nil {
		return "", false, err
	}
	v, ok := f.data[account]
	return v, ok, nil
}

func (f *fakeBackend) Set(account Account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if err := f.setErr[account]; err != nil {
		return err
	}
	f.data[account] = secret
	return nil
}

func (f *fakeBackend) Delete(account Account) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, account)
	return nil
}

// fakeConfigStore is a controllable config.Store used only to simulate an
// inner Save failure — something the real FileStore can't be made to fail
// deterministically without relying on filesystem permission quirks.
type fakeConfigStore struct {
	cfg     *config.Config
	saveErr error
}

func (f *fakeConfigStore) Load() (*config.Config, error) {
	cp := *f.cfg
	return &cp, nil
}

func (f *fakeConfigStore) Save(c *config.Config) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	cp := *c
	f.cfg = &cp
	return nil
}

// newFileInner returns a real config.FileStore rooted at an isolated temp
// dir, so migration tests exercise genuine on-disk JSON round-trips without
// touching the developer's real config file.
func newFileInner(t *testing.T) *config.FileStore {
	t.Helper()
	t.Setenv(config.EnvConfigDir, t.TempDir())
	s, err := config.NewFileStore()
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return s
}

func TestFileBackendIsInert(t *testing.T) {
	var b fileBackend
	if b.Name() != BackendFile {
		t.Errorf("Name = %s, want %s", b.Name(), BackendFile)
	}
	if err := b.Set(AccountActiveKey, "x"); err != nil {
		t.Errorf("Set: %v", err)
	}
	if v, found, err := b.Get(AccountActiveKey); err != nil || found || v != "" {
		t.Errorf("Get = (%q, %v, %v), want (\"\", false, nil)", v, found, err)
	}
	if err := b.Delete(AccountActiveKey); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestPathDelegatesToInner(t *testing.T) {
	inner := newFileInner(t)
	s := &Store{inner: inner, backend: newFakeBackend()}
	if s.Path() != inner.Path() {
		t.Errorf("Path = %q, want %q", s.Path(), inner.Path())
	}
}

func TestOpenForcesFileBackendOnOverride(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.backend.Name() != BackendFile {
		t.Errorf("backend = %s, want %s", s.backend.Name(), BackendFile)
	}
	if !s.forced {
		t.Error("forced should be true with BURNDOWN_CONFIG_DIR set")
	}
}

// TestOpenResolvesPlatformBackendWithoutOverride exercises the "no override"
// backend-selection branch without ever touching a real OS keychain: HOME is
// redirected to a scratch dir (so Open never reaches the developer's real
// config file), and only Name()/forced are inspected — Get/Set/Delete are
// never called on the resolved backend.
func TestOpenResolvesPlatformBackendWithoutOverride(t *testing.T) {
	t.Setenv(config.EnvConfigDir, "")
	t.Setenv("HOME", t.TempDir())
	s, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.forced {
		t.Error("forced should be false without BURNDOWN_CONFIG_DIR")
	}
	if want := newKeychainBackend().Name(); s.backend.Name() != want {
		t.Errorf("backend = %s, want %s", s.backend.Name(), want)
	}
}

// TestLoadWithFileBackendLeavesKeyInPlace covers the explicit fallback: on a
// Store resolved to the file backend (no keychain on this platform, or
// BURNDOWN_CONFIG_DIR forced it), Load must neither touch a backend nor
// rewrite the file — the plaintext key is the store.
func TestLoadWithFileBackendLeavesKeyInPlace(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CollectorKey: "abd_secret", Machine: "m"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	before, err := os.ReadFile(inner.Path())
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	s := &Store{inner: inner, backend: fileBackend{}, forced: true}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollectorKey != "abd_secret" {
		t.Errorf("CollectorKey = %q, want abd_secret", cfg.CollectorKey)
	}
	if status := s.LastBackend(); status.Name != BackendFile || !status.Forced {
		t.Errorf("LastBackend = %+v, want a forced file status", status)
	}
	after, err := os.ReadFile(inner.Path())
	if err != nil {
		t.Fatalf("read after Load: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("Load rewrote the file on a pure file-backend read:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestSaveWithFileBackendPersistsPlaintext(t *testing.T) {
	inner := newFileInner(t)
	s := &Store{inner: inner, backend: fileBackend{}, forced: true}

	if err := s.Save(&config.Config{CollectorKey: "abd_new", Machine: "m"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	onDisk, err := inner.Load()
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if onDisk.CollectorKey != "abd_new" {
		t.Errorf("CollectorKey = %q, want abd_new persisted in plaintext", onDisk.CollectorKey)
	}
	if status := s.LastBackend(); status.Name != BackendFile || !status.Forced {
		t.Errorf("LastBackend = %+v, want a forced file status", status)
	}
}

func TestLoadMigratesPlaintextKeyToBackend(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CollectorKey: "abd_secret", Machine: "m"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollectorKey != "abd_secret" {
		t.Errorf("CollectorKey = %q, want the real secret preserved in memory", cfg.CollectorKey)
	}
	if cfg.CredentialBackend != BackendKeychain {
		t.Errorf("CredentialBackend = %q, want %q", cfg.CredentialBackend, BackendKeychain)
	}
	if backend.data[AccountActiveKey] != "abd_secret" {
		t.Errorf("backend active key = %q, want abd_secret", backend.data[AccountActiveKey])
	}
	if status := s.LastBackend(); status.Name != BackendKeychain {
		t.Errorf("LastBackend = %+v, want keychain", status)
	}

	persisted := readPersisted(t, inner.Path())
	if string(persisted["collector_key"]) != `""` {
		t.Errorf("collector_key not scrubbed from disk: %s", persisted["collector_key"])
	}
	if string(persisted["credential_backend"]) != `"keychain"` {
		t.Errorf("credential_backend = %s, want %q", persisted["credential_backend"], "keychain")
	}
}

func TestLoadMigratesPendingKey(t *testing.T) {
	inner := newFileInner(t)
	seed := &config.Config{CollectorKey: "active", PendingKey: "pending", PendingKeyID: 9}
	if err := inner.Save(seed); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollectorKey != "active" || cfg.PendingKey != "pending" {
		t.Errorf("in-memory cfg lost a value: %+v", cfg)
	}
	if backend.data[AccountActiveKey] != "active" || backend.data[AccountPendingKey] != "pending" {
		t.Errorf("backend = %+v, want both keys migrated", backend.data)
	}

	onDisk, err := inner.Load() // raw FileStore.Load bypasses credstore hydration
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if onDisk.CollectorKey != "" || onDisk.PendingKey != "" {
		t.Errorf("disk copy not scrubbed: %+v", onDisk)
	}
}

func TestLoadHydratesFromBackendAfterMigration(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CredentialBackend: BackendKeychain, Machine: "m"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	backend.data[AccountActiveKey] = "abd_secret"
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollectorKey != "abd_secret" {
		t.Errorf("CollectorKey = %q, want abd_secret", cfg.CollectorKey)
	}
	if backend.setCalls != 0 {
		t.Errorf("hydrate should never write to the backend, got %d Set calls", backend.setCalls)
	}
}

func TestLoadHydratesPendingKey(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CredentialBackend: BackendKeychain, PendingKeyID: 9}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	backend.data[AccountActiveKey] = "active"
	backend.data[AccountPendingKey] = "pending"
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CollectorKey != "active" || cfg.PendingKey != "pending" {
		t.Errorf("cfg = %+v, want active/pending hydrated", cfg)
	}
}

func TestLoadHydrateMissingActiveKeyIsHardError(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CredentialBackend: BackendKeychain}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	s := &Store{inner: inner, backend: newFakeBackend()} // empty backend: key "lost"

	_, err := s.Load()
	if err == nil {
		t.Fatal("expected an error when the migrated key is missing from the backend")
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error should hint at re-login: %v", err)
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("error should match ErrBackendUnavailable so callers can distinguish it from a fresh install: %v", err)
	}
}

// TestLoadHydrateMissingPendingKeyDiscardsAndContinues is the H-3 regression
// test: losing only the ephemeral pending key must not take Load down —
// only losing the active key is fatal. It must also self-heal on disk so
// the discard is not repeated on every subsequent Load.
func TestLoadHydrateMissingPendingKeyDiscardsAndContinues(t *testing.T) {
	inner := newFileInner(t)
	seed := &config.Config{
		CredentialBackend: BackendKeychain, PendingKeyID: 3,
		PendingKeyExpires: "2026-01-01T00:00:00Z", OldKeyValidUntil: "2026-01-02T00:00:00Z",
	}
	if err := inner.Save(seed); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	backend.data[AccountActiveKey] = "active" // active present, pending missing
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load should succeed on the active key alone: %v", err)
	}
	if cfg.CollectorKey != "active" {
		t.Errorf("CollectorKey = %q, want active", cfg.CollectorKey)
	}
	if cfg.PendingKey != "" || cfg.PendingKeyID != 0 ||
		cfg.PendingKeyExpires != "" || cfg.OldKeyValidUntil != "" {
		t.Errorf("pending rotation state not cleared: %+v", cfg)
	}

	onDisk, err := inner.Load()
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if onDisk.PendingKeyID != 0 {
		t.Errorf("stale pending marker not cleared on disk, would repeat the discard every Load: %+v", onDisk)
	}
}

// TestLoadHydratePendingKeyReadErrorDiscardsAndContinues covers the other
// pending-key failure mode: a real backend error (locked mid-session), not
// just "not found". Per H-3, only the active key's absence is fatal.
func TestLoadHydratePendingKeyReadErrorDiscardsAndContinues(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CredentialBackend: BackendKeychain, PendingKeyID: 3}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	backend.data[AccountActiveKey] = "active"
	backend.getErr[AccountPendingKey] = errors.New("keychain locked mid-session")
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("a pending-key read error must not fail Load: %v", err)
	}
	if cfg.CollectorKey != "active" || cfg.PendingKey != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadFallsBackToFileOnKeychainFailure(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CollectorKey: "abd_secret", Machine: "m"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	backend.setErr[AccountActiveKey] = errors.New("keychain locked")
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load should degrade, not fail: %v", err)
	}
	if cfg.CollectorKey != "abd_secret" {
		t.Errorf("CollectorKey = %q, want abd_secret unchanged", cfg.CollectorKey)
	}
	if cfg.CredentialBackend != "" {
		t.Errorf("CredentialBackend = %q, want empty (still file-backed)", cfg.CredentialBackend)
	}
	if status := s.LastBackend(); status.Name != BackendFile || status.Forced {
		t.Errorf("LastBackend = %+v, want an unforced file fallback", status)
	}

	onDisk, err := inner.Load()
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if onDisk.CollectorKey != "abd_secret" {
		t.Errorf("disk copy changed unexpectedly: %+v", onDisk)
	}
}

func TestSaveFallsBackToPlaintextOnKeychainFailure(t *testing.T) {
	inner := newFileInner(t)
	backend := newFakeBackend()
	backend.setErr[AccountActiveKey] = errors.New("keychain denied")
	s := &Store{inner: inner, backend: backend}

	if err := s.Save(&config.Config{CollectorKey: "abd_new", Machine: "m"}); err != nil {
		t.Fatalf("Save should degrade, not fail: %v", err)
	}

	onDisk, err := inner.Load()
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if onDisk.CollectorKey != "abd_new" {
		t.Errorf("plaintext fallback not persisted: %+v", onDisk)
	}
}

// TestSaveWithPendingWriteFailureKeepsActiveKeyInBackend is the H-2
// regression test: writeVerified re-writes the active key on every Save
// once keychain-backed, not just the first migration, so it is very often a
// value that already existed and is durably relied on. A failure writing
// the unrelated pending key must not delete that already-good active key —
// the old rollback did exactly that, and a crash between the delete and the
// plaintext fallback write could strand the user with no working key
// anywhere (keychain empty, file scrubbed).
func TestSaveWithPendingWriteFailureKeepsActiveKeyInBackend(t *testing.T) {
	inner := newFileInner(t)
	backend := newFakeBackend()
	backend.data[AccountActiveKey] = "abd_active" // already durably present from a prior Save
	backend.setErr[AccountPendingKey] = errors.New("pending write denied")
	s := &Store{inner: inner, backend: backend}

	err := s.Save(&config.Config{
		CollectorKey: "abd_active", PendingKey: "abd_pending", PendingKeyID: 9,
	})
	if err != nil {
		t.Fatalf("Save should degrade to the plaintext fallback, not fail: %v", err)
	}
	if got := backend.data[AccountActiveKey]; got != "abd_active" {
		t.Errorf("active key was removed from the backend by an unrelated pending-key failure: got %q", got)
	}
}

// TestInterruptedMigrationCompletesOnNextLoad simulates a crash between
// writeSecrets succeeding and the disk scrub completing: the backend already
// durably holds the key, but the file still shows the pre-migration state.
// The next Load must resume and complete the migration (never leaving the
// key removed from disk without a durable keychain copy), and a further
// Load must be a pure, idempotent hydrate.
func TestInterruptedMigrationCompletesOnNextLoad(t *testing.T) {
	inner := newFileInner(t)
	if err := inner.Save(&config.Config{CollectorKey: "abd_secret", Machine: "m"}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	backend := newFakeBackend()
	backend.data[AccountActiveKey] = "abd_secret" // as if the prior run's write already landed
	s := &Store{inner: inner, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CredentialBackend != BackendKeychain {
		t.Errorf("resumed migration did not complete: %+v", cfg)
	}
	onDisk, err := inner.Load()
	if err != nil {
		t.Fatalf("raw Load: %v", err)
	}
	if onDisk.CollectorKey != "" || onDisk.CredentialBackend != BackendKeychain {
		t.Errorf("disk not scrubbed after resumed migration: %+v", onDisk)
	}

	backend.setCalls = 0
	cfg2, err := s.Load()
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if cfg2.CollectorKey != "abd_secret" {
		t.Errorf("second Load lost the key: %+v", cfg2)
	}
	if backend.setCalls != 0 {
		t.Errorf("hydrate path should not call Set, got %d", backend.setCalls)
	}
}

// TestMigrateOnLoadSurvivesScrubPersistFailure covers the other interrupted
// case: the backend write succeeds and is verified, but persisting the
// scrubbed JSON fails (e.g. disk full). The plaintext key must stay in
// memory and on disk — never removed before the keychain copy is durable
// *and* the disk agrees — and a later retry must complete the migration.
func TestMigrateOnLoadSurvivesScrubPersistFailure(t *testing.T) {
	fake := &fakeConfigStore{cfg: &config.Config{CollectorKey: "abd_secret"}, saveErr: errors.New("disk full")}
	backend := newFakeBackend()
	s := &Store{inner: fake, backend: backend}

	cfg, err := s.Load()
	if err != nil {
		t.Fatalf("Load should not fail when only the housekeeping scrub fails: %v", err)
	}
	if cfg.CollectorKey != "abd_secret" {
		t.Errorf("CollectorKey = %q, want abd_secret preserved", cfg.CollectorKey)
	}
	if backend.data[AccountActiveKey] != "abd_secret" {
		t.Errorf("backend should already hold the verified key: %+v", backend.data)
	}
	if fake.cfg.CredentialBackend == BackendKeychain {
		t.Error("on-disk marker must not flip to keychain when the scrub write failed")
	}

	fake.saveErr = nil // disk writable again
	cfg2, err := s.Load()
	if err != nil {
		t.Fatalf("retry Load: %v", err)
	}
	if cfg2.CredentialBackend != BackendKeychain {
		t.Errorf("retry should complete the migration: %+v", cfg2)
	}
}

func TestSaveDropsStalePendingKeyFromBackend(t *testing.T) {
	inner := newFileInner(t)
	backend := newFakeBackend()
	backend.data[AccountPendingKey] = "stale-pending"
	s := &Store{inner: inner, backend: backend}

	if err := s.Save(&config.Config{CollectorKey: "abd_active"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, ok := backend.data[AccountPendingKey]; ok {
		t.Error("a stale pending key should be removed from the backend")
	}
}

func TestSaveWithoutKeyDoesNotTouchBackend(t *testing.T) {
	inner := newFileInner(t)
	backend := newFakeBackend()
	s := &Store{inner: inner, backend: backend}

	if err := s.Save(&config.Config{Machine: "m"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if backend.setCalls != 0 {
		t.Errorf("Save without a key should not call backend.Set, got %d", backend.setCalls)
	}
}

func readPersisted(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	return m
}
