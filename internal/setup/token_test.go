package setup

import (
	"testing"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

func newTestStore(t *testing.T) config.Store {
	t.Helper()
	t.Setenv(config.EnvConfigDir, t.TempDir())
	store, err := config.NewFileStore()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestEnsureReceiverTokenGeneratesAndPersists proves a fresh install (no
// config file yet) gets a random token that is saved for next time.
func TestEnsureReceiverTokenGeneratesAndPersists(t *testing.T) {
	store := newTestStore(t)

	token, err := EnsureReceiverToken(store)
	if err != nil {
		t.Fatalf("EnsureReceiverToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty generated token")
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReceiverToken != token {
		t.Fatalf("persisted receiver_token = %q, want %q", cfg.ReceiverToken, token)
	}
}

// TestEnsureReceiverTokenIdempotent proves a second call returns the same
// token instead of rotating it, so agents configured on a previous run keep
// working.
func TestEnsureReceiverTokenIdempotent(t *testing.T) {
	store := newTestStore(t)

	first, err := EnsureReceiverToken(store)
	if err != nil {
		t.Fatalf("first EnsureReceiverToken: %v", err)
	}
	second, err := EnsureReceiverToken(store)
	if err != nil {
		t.Fatalf("second EnsureReceiverToken: %v", err)
	}
	if first != second {
		t.Fatalf("token rotated across calls: %q != %q", first, second)
	}
}

// TestEnsureReceiverTokenPreservesExistingConfig proves generating the token
// does not clobber unrelated fields already in the config.
func TestEnsureReceiverTokenPreservesExistingConfig(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(&config.Config{APIURL: "https://api.example.com", CollectorID: 7}); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureReceiverToken(store); err != nil {
		t.Fatalf("EnsureReceiverToken: %v", err)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != "https://api.example.com" || cfg.CollectorID != 7 {
		t.Fatalf("unrelated config fields lost: %+v", cfg)
	}
	if cfg.ReceiverToken == "" {
		t.Fatal("expected a receiver token to be generated")
	}
}

// TestEnsureReceiverTokenGeneratesDistinctValues is a basic sanity check that
// generateToken isn't returning a constant.
func TestEnsureReceiverTokenGeneratesDistinctValues(t *testing.T) {
	a, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two generated tokens were identical")
	}
	if len(a) != tokenBytes*2 {
		t.Fatalf("token length = %d, want %d (hex-encoded)", len(a), tokenBytes*2)
	}
}
