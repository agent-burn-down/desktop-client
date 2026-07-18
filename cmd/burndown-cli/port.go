package main

import (
	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// resolvePort picks the receiver port doctor/status should probe: an
// explicit --port flag wins, then the port `serve` last persisted to
// config.json, then flagPort (already defaulted to receiver.DefaultPort by
// the flag registration).
func resolvePort(cmd *cobra.Command, flagPort int) int {
	if cmd.Flags().Changed("port") {
		return flagPort
	}
	if port, ok := persistedPort(); ok {
		return port
	}
	return flagPort
}

// persistedPort reads the receiver port serve last recorded in config.json,
// tolerating a missing or unreadable config (fresh install, no serve yet).
func persistedPort() (int, bool) {
	store, err := config.NewFileStore()
	if err != nil {
		return 0, false
	}
	cfg, err := store.Load()
	if err != nil || cfg.ReceiverPort <= 0 {
		return 0, false
	}
	return cfg.ReceiverPort, true
}
