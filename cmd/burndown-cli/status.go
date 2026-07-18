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
	"github.com/agent-burn-down/desktop-client/internal/version"
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
			return runStatus(cmd, resolvePort(cmd, port), asJSON)
		},
	}
	cmd.Flags().IntVar(&port, "port", receiver.DefaultPort, "loopback port of the OTLP receiver")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// statusReport is the machine-readable status shape emitted with --json.
//
// Telemetry is the same self-telemetry the daemon sends in its heartbeat,
// derived from the live /healthz snapshot via counters.Report, so `status
// --json` and the heartbeat always report identical counter values.
type statusReport struct {
	DaemonUp         bool                `json:"daemon_up"`
	APIURL           string              `json:"api_url"`
	Machine          string              `json:"machine"`
	KeyPrefix        string              `json:"key_prefix"`
	CollectorID      int64               `json:"collector_id"`
	KeyExpiresAt     string              `json:"key_expires_at,omitempty"`
	RotationPending  bool                `json:"rotation_pending,omitempty"`
	RotationFailures int                 `json:"rotation_failures,omitempty"`
	LastRotationAt   string              `json:"last_rotation_at,omitempty"`
	AuthReason       string              `json:"auth_reason,omitempty"`
	Counters         map[string]int64    `json:"counters,omitempty"`
	Telemetry        *counters.Telemetry `json:"telemetry,omitempty"`
}

func runStatus(cmd *cobra.Command, port int, asJSON bool) error {
	report := statusReport{}
	if cfg := loadStatusConfig(); cfg != nil {
		report.APIURL = cfg.APIURL
		report.Machine = cfg.Machine
		report.KeyPrefix = keyPrefix(cfg.CollectorKey)
		report.CollectorID = cfg.CollectorID
		report.KeyExpiresAt = cfg.KeyExpiresAt
		report.RotationPending = cfg.PendingKey != ""
		report.RotationFailures = cfg.RotationFailures
		report.LastRotationAt = cfg.LastRotationAt
		report.AuthReason = cfg.AuthReason
	}
	if hz, err := probeHealthz(cmd.Context(), port); err == nil {
		report.DaemonUp = true
		report.Counters = hz.Counters
		tel := counters.Report(hz.Counters, version.Version)
		report.Telemetry = &tel
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
	if report.KeyPrefix != "" {
		outf(w, "key_expiry:   %s\n", rotationDisplay(report))
	}
	if report.AuthReason != "" {
		outf(w, "auth:         DEGRADED (%s) — run `burndown-cli login`\n", report.AuthReason)
	}
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

// rotationDisplay summarizes key expiry and rotation health for status text.
func rotationDisplay(report statusReport) string {
	base := "never expires"
	if report.KeyExpiresAt != "" {
		base = expiryDisplay(report.KeyExpiresAt)
	}
	if report.RotationPending {
		base += " (rotation pending verification)"
	}
	if report.RotationFailures > 0 {
		base += fmt.Sprintf(" (rotation failing: %d attempt(s), run `burndown-cli login`"+
			" if this continues)", report.RotationFailures)
	}
	return base
}

func expiryDisplay(iso string) string {
	expires, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	remaining := time.Until(expires)
	if remaining <= 0 {
		return "EXPIRED"
	}
	days := int(remaining.Hours() / 24)
	if days < 1 {
		return "expires in <1d"
	}
	return fmt.Sprintf("expires in %dd", days)
}

func unixDisplay(sec int64) string {
	if sec == 0 {
		return "never"
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
