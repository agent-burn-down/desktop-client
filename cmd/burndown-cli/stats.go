package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/queue"
)

// newStatsCmd builds the `stats` command: print local daily token/cost totals
// and top tools from retained (acked) queue rows. It reads the queue database
// read-only, so it works whether or not the daemon is running.
func newStatsCmd() *cobra.Command {
	var (
		days   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show local daily tokens, cost, and top tools from retained events",
		Long: "Summarize locally retained (already-uploaded) events: per-day token\n" +
			"and cost totals plus the most-used tools. The window is bounded by the\n" +
			"configured retention_days (default 7). This is a local convenience view,\n" +
			"not a mirror of the dashboard analytics.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStats(cmd, days, asJSON)
		},
	}
	cmd.Flags().IntVar(&days, "days", 0,
		"days to include (default and max: configured retention_days)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// statsTotals is the summed totals row across the reported window.
type statsTotals struct {
	Events            int64   `json:"events"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheCreateTokens int64   `json:"cache_create_tokens"`
	CostUSD           float64 `json:"cost_usd"`
}

// statsOutput is the machine-readable shape emitted with --json.
type statsOutput struct {
	Days     int               `json:"days"`
	Since    time.Time         `json:"since"`
	Daily    []queue.DailyStat `json:"daily"`
	Totals   statsTotals       `json:"totals"`
	TopTools []queue.ToolStat  `json:"top_tools"`
}

func runStats(cmd *cobra.Command, days int, asJSON bool) error {
	days = effectiveDays(days)
	report, err := loadStats(days)
	if err != nil {
		return err
	}
	out := statsOutput{
		Days:     days,
		Since:    report.Since,
		Daily:    report.Daily,
		Totals:   sumTotals(report.Daily),
		TopTools: report.Tools,
	}
	if asJSON {
		return writeStatsJSON(cmd.OutOrStdout(), out)
	}
	writeStatsText(cmd.OutOrStdout(), out)
	return nil
}

// effectiveDays clamps the requested window to [1, retention]. A zero or
// negative request defaults to the full retention window.
func effectiveDays(days int) int {
	retention := config.DefaultRetentionDays
	if cfg := loadStatusConfig(); cfg != nil {
		retention = cfg.Retention()
	}
	if days <= 0 || days > retention {
		return retention
	}
	return days
}

// loadStats opens the queue read-only and aggregates the last `days` of retained
// events. A queue database that does not exist yet yields an empty report.
func loadStats(days int) (queue.StatsReport, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	path, err := queue.DefaultPath()
	if err != nil {
		return queue.StatsReport{}, err
	}
	q, err := queue.OpenReadOnly(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return queue.StatsReport{Since: since}, nil
		}
		return queue.StatsReport{}, err
	}
	defer func() { _ = q.Close() }()
	return q.StatsSince(since, queue.DefaultTopTools)
}

func sumTotals(daily []queue.DailyStat) statsTotals {
	var t statsTotals
	for _, d := range daily {
		t.Events += d.Events
		t.InputTokens += d.InputTokens
		t.OutputTokens += d.OutputTokens
		t.CacheReadTokens += d.CacheReadTokens
		t.CacheCreateTokens += d.CacheCreateTokens
		t.CostUSD += d.CostUSD
	}
	return t
}

func writeStatsJSON(w io.Writer, out statsOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeStatsText(w io.Writer, out statsOutput) {
	outf(w, "stats: last %d day(s), since %s\n\n",
		out.Days, out.Since.UTC().Format("2006-01-02"))
	if len(out.Daily) == 0 {
		outln(w, "no local usage data in the retained window")
		return
	}
	writeStatsTable(w, out)
	writeTopTools(w, out.TopTools)
}

func writeStatsTable(w io.Writer, out statsOutput) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	outf(tw, "DATE\tEVENTS\tINPUT\tOUTPUT\tCACHE_R\tCACHE_C\tCOST\n")
	for _, d := range out.Daily {
		outf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t$%.4f\n",
			d.Date, d.Events, d.InputTokens, d.OutputTokens,
			d.CacheReadTokens, d.CacheCreateTokens, d.CostUSD)
	}
	t := out.Totals
	outf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\t$%.4f\n",
		t.Events, t.InputTokens, t.OutputTokens,
		t.CacheReadTokens, t.CacheCreateTokens, t.CostUSD)
	_ = tw.Flush()
}

func writeTopTools(w io.Writer, tools []queue.ToolStat) {
	if len(tools) == 0 {
		return
	}
	outln(w, "\ntop tools:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, t := range tools {
		outf(tw, "  %s\t%d\n", t.Tool, t.Count)
	}
	_ = tw.Flush()
}
