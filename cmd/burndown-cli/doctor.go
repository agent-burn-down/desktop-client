package main

import (
	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/doctor"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
)

// newDoctorCmd builds the `doctor` command: run health checks with remediation
// hints, as a text table or JSON. Exit code reflects the worst status: 0 pass,
// 1 warn, 2 fail. It runs safely with the daemon down.
func newDoctorCmd() *cobra.Command {
	var (
		port   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks and print remediation hints",
		Long: "Check version, config, backend reachability, heartbeat, daemon,\n" +
			"agent OTEL setup, queue integrity, and the background service. Each\n" +
			"failing check prints a one-line fix. Exit code is 0 (pass), 1 (warn),\n" +
			"or 2 (fail). Safe to run while the collector is stopped.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, resolvePort(cmd, port), asJSON)
		},
	}
	cmd.Flags().IntVar(&port, "port", receiver.DefaultPort, "loopback port of the OTLP receiver")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func runDoctor(cmd *cobra.Command, port int, asJSON bool) error {
	doc, err := doctor.New(doctor.Config{Port: port})
	if err != nil {
		return err
	}
	results := doc.Run(cmd.Context())
	out := cmd.OutOrStdout()
	if asJSON {
		if err := doctor.WriteJSON(out, results); err != nil {
			return err
		}
	} else {
		doctor.WriteText(out, results)
	}
	if code := doctor.ExitCode(results); code != 0 {
		return newExitError(code)
	}
	return nil
}
