package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
)

// newStatusCmd builds the `status` command: report daemon liveness (by probing
// /healthz) plus config-derived identity, as text or JSON.
func newStatusCmd() *cobra.Command {
	var (
		port   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show collector daemon state, counters, and configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, port, asJSON)
		},
	}
	cmd.Flags().IntVar(&port, "port", receiver.DefaultPort, "loopback port of the OTLP receiver")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// statusReport is the machine-readable status shape emitted with --json.
type statusReport struct {
	DaemonUp    bool             `json:"daemon_up"`
	APIURL      string           `json:"api_url"`
	Machine     string           `json:"machine"`
	KeyPrefix   string           `json:"key_prefix"`
	CollectorID int64            `json:"collector_id"`
	Counters    map[string]int64 `json:"counters,omitempty"`
}

func runStatus(cmd *cobra.Command, port int, asJSON bool) error {
	report := statusReport{}
	if cfg := loadStatusConfig(); cfg != nil {
		report.APIURL = cfg.APIURL
		report.Machine = cfg.Machine
		report.KeyPrefix = keyPrefix(cfg.CollectorKey)
		report.CollectorID = cfg.CollectorID
	}
	if hz, err := probeHealthz(cmd.Context(), port); err == nil {
		report.DaemonUp = true
		report.Counters = hz.Counters
	}
	if asJSON {
		return writeJSONReport(cmd.OutOrStdout(), report)
	}
	writeTextReport(cmd.OutOrStdout(), report)
	return nil
}

// loadStatusConfig loads config best-effort; a missing or unreadable config is
// not fatal for status, so nil is returned and identity fields stay blank.
func loadStatusConfig() *config.Config {
	store, err := config.NewFileStore()
	if err != nil {
		return nil
	}
	cfg, err := store.Load()
	if err != nil {
		return nil
	}
	return cfg
}

func writeJSONReport(w io.Writer, report statusReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func writeTextReport(w io.Writer, report statusReport) {
	if report.DaemonUp {
		outln(w, "daemon: running")
	} else {
		outln(w, "daemon: not running")
	}
	outf(w, "api_url:      %s\n", orDash(report.APIURL))
	outf(w, "machine:      %s\n", orDash(report.Machine))
	outf(w, "key:          %s\n", keyDisplay(report.KeyPrefix))
	outf(w, "collector_id: %s\n", idDisplay(report.CollectorID))
	if report.DaemonUp {
		writeCounters(w, report.Counters)
	}
}

func writeCounters(w io.Writer, c map[string]int64) {
	outf(w, "received:     %d\n", c[counters.Received])
	outf(w, "filtered:     %d\n", c[counters.Filtered])
	outf(w, "queued:       %d\n", c[counters.Queued])
	outf(w, "queue_depth:  %d\n", c[counters.QueueDepth])
	outf(w, "uploaded:     %d\n", c[counters.Uploaded])
	outf(w, "errors:       %d\n", c[counters.Errors]+c[counters.UploadErrors])
	outf(w, "last_upload:  %s\n", unixDisplay(c[counters.LastUploadAt]))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func keyDisplay(prefix string) string {
	if prefix == "" {
		return "-"
	}
	return prefix + "…"
}

func idDisplay(id int64) string {
	if id == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", id)
}

func unixDisplay(sec int64) string {
	if sec == 0 {
		return "never"
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
