package credstore

const (
	// BackendKeychain names the OS keychain backend.
	BackendKeychain = "keychain"
	// BackendFile names the plaintext-file fallback backend: the collector
	// key and pending key stay in config.Config's JSON file.
	BackendFile = "file"
)

// Account identifies which of the two collector credentials a Backend
// operation applies to.
type Account string

const (
	// AccountActiveKey is the currently active collector key.
	AccountActiveKey Account = "collector_key"
	// AccountPendingKey is a rotated-to key awaiting verification (set only
	// during the rotation overlap window; see config.Config.PendingKey).
	AccountPendingKey Account = "pending_key"
)

// Backend is a secret-storage backend for the collector's credentials.
// keychainBackend (darwin) and fileBackend implement it.
type Backend interface {
	// Name identifies the backend: BackendKeychain or BackendFile.
	Name() string
	// Get retrieves account's secret. found is false with a nil error when
	// the account has never been set in this backend (not an error case).
	Get(account Account) (secret string, found bool, err error)
	// Set stores secret for account, overwriting any existing value.
	Set(account Account, secret string) error
	// Delete removes account's secret. Deleting an absent account is a
	// no-op, not an error.
	Delete(account Account) error
}

// BackendStatus reports which backend served the most recently completed
// Load or Save call, surfaced by `burndown-cli doctor`.
type BackendStatus struct {
	// Name is BackendKeychain or BackendFile.
	Name string
	// Detail is a human-readable explanation for doctor/status output.
	Detail string
	// Forced is true when the file backend was intentionally selected
	// (BURNDOWN_CONFIG_DIR override, no keychain backend on this platform,
	// or no key stored yet) rather than chosen after a runtime keychain
	// failure.
	Forced bool
}

// fileBackend is the inert backend behind BackendFile: the plaintext JSON
// file is the only store, so Get always reports nothing held here and
// Set/Delete are no-ops. Store checks Name() before ever calling these; they
// exist only to satisfy Backend as a safe, explicit default.
type fileBackend struct{}

func (fileBackend) Name() string { return BackendFile }

func (fileBackend) Get(Account) (string, bool, error) { return "", false, nil }

func (fileBackend) Set(Account, string) error { return nil }

func (fileBackend) Delete(Account) error { return nil }
