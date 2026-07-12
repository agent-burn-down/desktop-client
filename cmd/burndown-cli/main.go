// Command burndown-cli is the Agent Burn Down desktop collector: a local
// OTLP receiver that normalizes, filters, queues, and uploads coding-agent
// telemetry metadata to the Agent Burn Down backend.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/version"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var exit *exitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "burndown-cli",
		Short: "Agent Burn Down desktop collector",
		Long: "Local telemetry collector for coding agents (Claude Code, Codex).\n" +
			"Receives OTLP on 127.0.0.1:8765, keeps metadata only, and uploads\n" +
			"to the Agent Burn Down backend.",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("burndown-cli {{.Version}}\n")
	root.AddCommand(newServeCmd())
	root.AddCommand(newLoginCmd())
	root.AddCommand(newRegisterCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newStatsCmd())
	root.AddCommand(newSendTestCmd())
	root.AddCommand(newSetupCmd())
	root.AddCommand(newServiceCmd())
	root.AddCommand(newDoctorCmd())
	return root
}
