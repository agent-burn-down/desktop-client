package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/daemon"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
)

// newServeCmd builds the `serve` subcommand that runs the collector daemon.
func newServeCmd() *cobra.Command {
	var (
		port    int
		verbose bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the collector daemon (receiver, normalize, filter, queue, upload)",
		Long: "Run the local collector: bind the loopback OTLP receiver, normalize and\n" +
			"filter incoming telemetry, queue it durably, and upload to the backend on\n" +
			"the server policy cadence. Runs until SIGTERM/SIGINT, then drains cleanly.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), port, verbose)
		},
	}
	cmd.Flags().IntVar(&port, "port", receiver.DefaultPort, "loopback port for the OTLP receiver")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "also log to stderr")
	return cmd
}

// runServe loads and validates config, builds the daemon, and runs it until a
// termination signal arrives.
func runServe(ctx context.Context, port int, verbose bool) error {
	store, err := config.NewFileStore()
	if err != nil {
		return err
	}
	cfg, err := loadServeConfig(store)
	if err != nil {
		return err
	}
	d, err := daemon.New(daemon.Options{
		Config: cfg, Store: store, Port: port, Verbose: verbose,
	})
	if err != nil {
		return err
	}
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return d.Run(sigCtx)
}

// loadServeConfig loads the collector config and verifies it is registered,
// pointing the user at login when it is not.
func loadServeConfig(store config.Store) (*config.Config, error) {
	cfg, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("%w\nrun `burndown-cli login` to register this collector first", err)
	}
	if cfg.APIURL == "" || cfg.CollectorKey == "" || cfg.CollectorID == 0 {
		return nil, fmt.Errorf(
			"config is incomplete (need api_url, collector_key, collector_id); " +
				"run `burndown-cli login` to register this collector first")
	}
	return cfg, nil
}
