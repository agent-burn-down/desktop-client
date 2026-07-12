package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
)

// defaultAPIURL is the backend base URL used when --api-url is not given.
const defaultAPIURL = "https://app.agentburndown.com"

// keyPrefixLen is the number of leading characters of a collector key that are
// safe to display (yaahc_ + 8 chars). The full secret is never printed.
const keyPrefixLen = 14

// newLoginCmd builds the `login` command: validate a collector key by
// registering this machine, then persist the credentials.
func newLoginCmd() *cobra.Command {
	var key, machine, email, apiURL string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Register this machine with a collector key and save credentials",
		Long: "Validate a collector key (from your dashboard) by registering this\n" +
			"machine, then store it locally. With no --key, the key is read from\n" +
			"stdin (hidden when a terminal, a plain line when piped for CI).",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, loginInput{key: key, machine: machine, email: email, apiURL: apiURL})
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "collector key (yaahc_...); prompted if omitted")
	cmd.Flags().StringVar(&machine, "machine", "", "machine name (default: hostname)")
	cmd.Flags().StringVar(&email, "email", "", "reporting user email; prompted if omitted")
	cmd.Flags().StringVar(&apiURL, "api-url", defaultAPIURL, "backend base URL")
	return cmd
}

type loginInput struct {
	key, machine, email, apiURL string
}

// runLogin resolves inputs, validates the key against the backend, and persists
// the resulting credentials and policy.
func runLogin(cmd *cobra.Command, in loginInput) error {
	p := &prompter{cmd: cmd}
	email, err := resolveEmail(p, in.email)
	if err != nil {
		return err
	}
	machine, err := resolveMachine(in.machine)
	if err != nil {
		return err
	}
	key, err := resolveKey(p, in.key)
	if err != nil {
		return err
	}
	client := api.NewClient(in.apiURL, key)
	out, err := client.Register(cmd.Context(), machine, email)
	if err != nil {
		return loginError(err)
	}
	store, err := config.NewFileStore()
	if err != nil {
		return err
	}
	if err := persistLogin(store, in.apiURL, key, email, machine, out); err != nil {
		return err
	}
	outf(cmd.OutOrStdout(),
		"Logged in. key %s… collector_id %d machine %s\n",
		keyPrefix(key), out.CollectorID, machine)
	return nil
}

// newRegisterCmd builds the `register` command: re-register this machine using
// stored credentials and refresh the collector id and policy.
func newRegisterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Re-register this machine and refresh collector id and policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRegister(cmd)
		},
	}
	return cmd
}

func runRegister(cmd *cobra.Command) error {
	store, err := config.NewFileStore()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return fmt.Errorf("%w\nrun `burndown-cli login` first", err)
	}
	if err := requireRegisterable(cfg); err != nil {
		return err
	}
	client := api.NewClient(cfg.APIURL, cfg.CollectorKey)
	out, err := client.Register(cmd.Context(), cfg.Machine, cfg.UserEmail)
	if err != nil {
		return loginError(err)
	}
	cfg.CollectorID = out.CollectorID
	cfg.Policy = out.Policy
	if err := store.Save(cfg); err != nil {
		return err
	}
	outf(cmd.OutOrStdout(),
		"Registered. collector_id %d machine %s\n", out.CollectorID, cfg.Machine)
	return nil
}

// requireRegisterable verifies the stored config has the fields Register needs.
func requireRegisterable(cfg *config.Config) error {
	if cfg.APIURL == "" || cfg.CollectorKey == "" || cfg.UserEmail == "" || cfg.Machine == "" {
		return errors.New(
			"config is incomplete (need api_url, collector_key, user_email, machine); " +
				"run `burndown-cli login` first")
	}
	return nil
}

// persistLogin loads any existing config (to keep unknown fields), overwrites
// the known credential fields, and saves.
func persistLogin(
	store config.Store, apiURL, key, email, machine string, out *api.RegisterOut,
) error {
	cfg, err := store.Load()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		cfg = &config.Config{}
	}
	cfg.APIURL = apiURL
	cfg.CollectorKey = key
	cfg.UserEmail = email
	cfg.Machine = machine
	cfg.CollectorID = out.CollectorID
	cfg.Policy = out.Policy
	return store.Save(cfg)
}

// loginError maps an auth failure to an actionable message, passing other
// errors through unchanged.
func loginError(err error) error {
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		return fmt.Errorf(
			"collector key rejected: %s\ncheck the key from your dashboard and try again",
			authErr.Detail)
	}
	return err
}

func resolveEmail(p *prompter, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	email, err := p.line("Reporting user email: ")
	if err != nil {
		return "", err
	}
	if email == "" {
		return "", errors.New("a reporting user email is required")
	}
	return email, nil
}

func resolveMachine(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("determine hostname (pass --machine): %w", err)
	}
	return host, nil
}

func resolveKey(p *prompter, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	key, err := p.secret("Collector key (yaahc_...): ")
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", errors.New("a collector key is required")
	}
	return key, nil
}

// keyPrefix returns the displayable prefix (first keyPrefixLen chars) of a
// collector key, never the full secret.
func keyPrefix(key string) string {
	if len(key) <= keyPrefixLen {
		return key
	}
	return key[:keyPrefixLen]
}

// prompter reads interactive input from a command's stdin, using hidden input
// for secrets when stdin is a terminal and plain line reads otherwise (so CI
// can pipe values in).
type prompter struct {
	cmd *cobra.Command
	br  *bufio.Reader
}

func (p *prompter) reader() *bufio.Reader {
	if p.br == nil {
		p.br = bufio.NewReader(p.cmd.InOrStdin())
	}
	return p.br
}

func (p *prompter) line(label string) (string, error) {
	outf(p.cmd.ErrOrStderr(), "%s", label)
	line, err := p.reader().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (p *prompter) secret(label string) (string, error) {
	if f, ok := p.cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		outf(p.cmd.ErrOrStderr(), "%s", label)
		b, err := term.ReadPassword(int(f.Fd()))
		outln(p.cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return p.line(label)
}
