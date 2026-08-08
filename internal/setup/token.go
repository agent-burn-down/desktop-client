package setup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// tokenBytes is the amount of random entropy for the per-installation
// receiver token, hex-encoded to twice this length.
const tokenBytes = 32

// EnsureReceiverToken returns the config's per-installation receiver token,
// generating and persisting one via crypto/rand if none exists yet. A config
// that already has a token is returned unchanged, so re-running `setup` never
// rotates it and breaks agents already configured to send it. A missing
// config file is treated as empty, matching login's persistLogin behavior, so
// `setup` can run before `login`.
func EnsureReceiverToken(store config.Store) (string, error) {
	cfg, err := store.Load()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		cfg = &config.Config{}
	}
	if cfg.ReceiverToken != "" {
		return cfg.ReceiverToken, nil
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	cfg.ReceiverToken = token
	if err := store.Save(cfg); err != nil {
		return "", fmt.Errorf("save generated receiver token: %w", err)
	}
	return token, nil
}

func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate receiver token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
