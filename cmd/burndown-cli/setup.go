package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/agent-burn-down/desktop-client/internal/receiver"
	"github.com/agent-burn-down/desktop-client/internal/setup"
)

// setupFlags holds the parsed `setup` command flags.
type setupFlags struct {
	port                      int
	check, all, claude, codex bool
	yes                       bool
}

// newSetupCmd builds the `setup` command: write OTEL config for detected agents
// idempotently, with a --check dry-run and per-agent force flags.
func newSetupCmd() *cobra.Command {
	f := &setupFlags{}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure Claude Code and Codex to export telemetry to the collector",
		Long: "Detect installed agents and add the OTEL settings that point them at\n" +
			"the local collector. Only missing keys are added; existing values are\n" +
			"preserved; changed files are backed up first. A second run is a no-op.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetup(cmd, f)
		},
	}
	cmd.Flags().IntVar(&f.port, "port", receiver.DefaultPort, "receiver port the agents export to")
	cmd.Flags().BoolVar(&f.check, "check", false, "dry run: print changes, nonzero exit if pending")
	cmd.Flags().BoolVar(&f.all, "all", false, "configure both agents regardless of detection")
	cmd.Flags().BoolVar(&f.claude, "claude", false, "configure Claude Code (even if not detected)")
	cmd.Flags().BoolVar(&f.codex, "codex", false, "configure Codex (even if not detected)")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// agentPlan pairs an agent name with its computed plan for uniform handling.
type agentPlan struct {
	name string
	plan interface {
		Empty() bool
		Descriptions() []string
		Apply() (string, error)
	}
}

func runSetup(cmd *cobra.Command, f *setupFlags) error {
	out := cmd.OutOrStdout()
	plans, err := gatherPlans(cmd, f)
	if err != nil {
		return err
	}
	pending := printPlan(out, plans)
	if !pending {
		outln(out, "All detected agents already configured. Nothing to do.")
		return nil
	}
	if f.check {
		return errors.New("changes pending; run `burndown-cli setup` to apply them")
	}
	if !f.yes {
		ok, err := confirm(cmd)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("aborted")
		}
	}
	return applyPlans(out, plans)
}

// targets records which agents to configure plus the detection facts used to
// report honestly on those left out.
type targets struct {
	claude, codex       bool
	detClaude, detCodex bool
	claudeDir, codexDir string
}

// gatherPlans resolves targets, reports detection honestly, and builds each
// selected agent's plan.
func gatherPlans(cmd *cobra.Command, f *setupFlags) ([]agentPlan, error) {
	t, err := resolveTargets(f)
	if err != nil {
		return nil, err
	}
	out := cmd.OutOrStdout()
	reportDetection(out, "Claude Code", t.claudeDir, t.detClaude, t.claude)
	reportDetection(out, "Codex", t.codexDir, t.detCodex, t.codex)
	return buildPlans(t, f.port)
}

// resolveTargets runs detection and applies the force flags to decide which
// agents to configure.
func resolveTargets(f *setupFlags) (targets, error) {
	detClaude, claudeDir, err := setup.DetectClaude()
	if err != nil {
		return targets{}, err
	}
	detCodex, codexDir, err := setup.DetectCodex()
	if err != nil {
		return targets{}, err
	}
	wantClaude, wantCodex := selectAgents(f, detClaude, detCodex)
	return targets{
		claude: wantClaude, codex: wantCodex,
		detClaude: detClaude, detCodex: detCodex,
		claudeDir: claudeDir, codexDir: codexDir,
	}, nil
}

// selectAgents targets detected agents by default, or exactly those named by
// the --all/--claude/--codex force flags when any is set.
func selectAgents(f *setupFlags, detClaude, detCodex bool) (claude, codex bool) {
	if !f.all && !f.claude && !f.codex {
		return detClaude, detCodex
	}
	return f.all || f.claude, f.all || f.codex
}

// buildPlans computes the pending change set for each selected agent.
func buildPlans(t targets, port int) ([]agentPlan, error) {
	var plans []agentPlan
	if t.claude {
		p, err := setup.PlanClaude(port)
		if err != nil {
			return nil, err
		}
		plans = append(plans, agentPlan{name: "Claude Code", plan: p})
	}
	if t.codex {
		p, err := setup.PlanCodex(port)
		if err != nil {
			return nil, err
		}
		plans = append(plans, agentPlan{name: "Codex", plan: p})
	}
	return plans, nil
}

func reportDetection(w io.Writer, name, dir string, detected, targeted bool) {
	switch {
	case detected && targeted:
		outf(w, "%s: detected (%s)\n", name, dir)
	case !detected && targeted:
		outf(w, "%s: not detected, configuring anyway (%s)\n", name, dir)
	case detected && !targeted:
		outf(w, "%s: detected but not selected\n", name)
	default:
		outf(w, "%s: not detected, skipping\n", name)
	}
}

// printPlan prints each targeted agent's pending edits and reports whether any
// change is pending.
func printPlan(w io.Writer, plans []agentPlan) bool {
	pending := false
	for _, ap := range plans {
		if ap.plan.Empty() {
			outf(w, "%s: up to date\n", ap.name)
			continue
		}
		pending = true
		outf(w, "%s: will add\n", ap.name)
		for _, line := range ap.plan.Descriptions() {
			outf(w, "  %s\n", line)
		}
	}
	return pending
}

func applyPlans(w io.Writer, plans []agentPlan) error {
	for _, ap := range plans {
		if ap.plan.Empty() {
			continue
		}
		backup, err := ap.plan.Apply()
		if err != nil {
			return fmt.Errorf("%s: %w", ap.name, err)
		}
		if backup != "" {
			outf(w, "%s: backed up to %s\n", ap.name, backup)
		}
		outf(w, "%s: updated\n", ap.name)
	}
	outln(w, "Restart Claude Code and Codex so the new OTEL settings take effect.")
	return nil
}

// confirm prompts for confirmation, defaulting to yes on an empty line.
func confirm(cmd *cobra.Command) (bool, error) {
	p := &prompter{cmd: cmd}
	ans, err := p.line("Apply? [Y/n] ")
	if err != nil {
		return false, err
	}
	switch ans {
	case "", "y", "Y", "yes", "Yes":
		return true, nil
	default:
		return false, nil
	}
}
