package main

import (
	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/platform"
)

// newServiceCmd builds the `service` command group for managing the collector
// as an OS background service (launchd on macOS).
func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the collector background service (install, start, status...)",
		Long: "Install and control the collector as a launchd service that runs at\n" +
			"login and restarts on crash. On unsupported platforms every subcommand\n" +
			"reports that service management is unavailable.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		serviceAction("install", "Install and load the background service", svcInstall),
		serviceAction("uninstall", "Stop and remove the background service", svcUninstall),
		serviceAction("start", "Start (or restart) the background service", svcStart),
		serviceAction("stop", "Stop the background service without removing it", svcStop),
		serviceAction("status", "Show the background service state", svcStatus),
	)
	return cmd
}

// serviceAction builds a subcommand that resolves the platform service, then
// runs the given action. Resolution errors (including unsupported platforms)
// surface as a non-zero exit.
func serviceAction(
	use, short string, action func(*cobra.Command, platform.Service) error,
) *cobra.Command {
	return &cobra.Command{
		Use:          use,
		Short:        short,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := platform.New()
			if err != nil {
				return err
			}
			return action(cmd, svc)
		},
	}
}

func svcInstall(cmd *cobra.Command, svc platform.Service) error {
	if err := svc.Install(); err != nil {
		return err
	}
	outln(cmd.OutOrStdout(), "Service installed and loaded.")
	return printStatus(cmd, svc)
}

func svcUninstall(cmd *cobra.Command, svc platform.Service) error {
	status, err := svc.Status()
	if err != nil {
		return err
	}
	if status.State == platform.StateNotInstalled {
		outln(cmd.OutOrStdout(), "Service is not installed; nothing to do.")
		return nil
	}
	if err := svc.Uninstall(); err != nil {
		return err
	}
	outln(cmd.OutOrStdout(), "Service stopped and removed.")
	return nil
}

func svcStart(cmd *cobra.Command, svc platform.Service) error {
	if err := svc.Start(); err != nil {
		return err
	}
	outln(cmd.OutOrStdout(), "Service started.")
	return printStatus(cmd, svc)
}

func svcStop(cmd *cobra.Command, svc platform.Service) error {
	if err := svc.Stop(); err != nil {
		return err
	}
	outln(cmd.OutOrStdout(), "Service stopped.")
	return nil
}

func svcStatus(cmd *cobra.Command, svc platform.Service) error {
	return printStatus(cmd, svc)
}

func printStatus(cmd *cobra.Command, svc platform.Service) error {
	status, err := svc.Status()
	if err != nil {
		return err
	}
	outf(cmd.OutOrStdout(), "Status: %s\n", status)
	return nil
}
