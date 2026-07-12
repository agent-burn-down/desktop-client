package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/platform"
)

// defaultAPIURL is the backend base URL used when --api-url is not given.
const defaultAPIURL = "https://app.agentburndown.com"

// keyPrefixLen is the number of leading characters of a collector key that are
// safe to display (abd_ + a few chars, or the longer legacy yaahc_ prefix).
// The full secret is never printed.
const keyPrefixLen = 14

// devicePollFloor is the minimum wait between device-token polls, applied if
// the server ever returns a non-positive interval.
const devicePollFloor = time.Second

// openURL is platform.OpenURL by a package var so tests can stub it — running
// the real opener during `go test` would pop a browser on the developer's
// machine.
var openURL = platform.OpenURL

// devicePollSleep overrides pollDeviceToken's wait between polls; nil in
// production (time.Sleep), a no-op in tests so polling doesn't take real time.
var devicePollSleep func(time.Duration)

// newLoginCmd builds the `login` command: pair via the device-code flow by
// default, falling back to a pasted/piped collector key for headless/CI use.
func newLoginCmd() *cobra.Command {
	var key, machine, email, apiURL string
	var device bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Pair this machine with your Agent Burndown account",
		Long: "Pair this machine with your Agent Burndown account. On an interactive\n" +
			"terminal, opens a browser to approve a short device code by default.\n" +
			"For headless/CI use, pass --key (a collector key from your dashboard)\n" +
			"or pipe it via stdin.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLogin(cmd, loginInput{
				key: key, machine: machine, email: email, apiURL: apiURL, device: device,
			})
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "collector key (abd_...); use for headless/CI login")
	cmd.Flags().StringVar(&machine, "machine", "", "machine name (default: hostname)")
	cmd.Flags().StringVar(&email, "email", "", "reporting user email; prompted if omitted")
	cmd.Flags().StringVar(&apiURL, "api-url", defaultAPIURL, "backend base URL")
	cmd.Flags().BoolVar(&device, "device", false,
		"force the device-code flow even if stdin is not a terminal")
	return cmd
}

type loginInput struct {
	key, machine, email, apiURL string
	device                      bool
}

// runLogin dispatches to the device-code flow (the default on an interactive
// terminal) or the paste/pipe-key flow (--key, --device forcing the other way
// is an error, or a non-terminal stdin for headless/CI).
func runLogin(cmd *cobra.Command, in loginInput) error {
	if in.key != "" && in.device {
		return errors.New("--key and --device are mutually exclusive")
	}
	if in.device || (in.key == "" && isTerminalStdin(cmd)) {
		return runDeviceLogin(cmd, in)
	}
	return runPasteKeyLogin(cmd, in)
}

// runPasteKeyLogin resolves inputs, validates the key against the backend, and
// persists the resulting credentials and policy. This is the pre-device-flow
// behavior, unchanged: --key explicitly, or a key piped via stdin for CI.
func runPasteKeyLogin(cmd *cobra.Command, in loginInput) error {
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

// runDeviceLogin runs the RFC-8628-style device-authorization flow: request a
// grant, print/open the approval URL, poll for approval, then register with
// the issued key. Ctrl-C is safe at any point — nothing is persisted until
// every step succeeds.
func runDeviceLogin(cmd *cobra.Command, in loginInput) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	p := &prompter{cmd: cmd}
	email, err := resolveEmail(p, in.email)
	if err != nil {
		return err
	}
	machine, err := resolveMachine(in.machine)
	if err != nil {
		return err
	}

	client := api.NewClient(in.apiURL, "")
	da, err := client.DeviceAuthorize(ctx, machine)
	if err != nil {
		return fmt.Errorf("start device login: %w", err)
	}
	announceDeviceGrant(cmd, da)

	dt, err := pollDeviceToken(ctx, client, da, devicePollSleep)
	if err != nil {
		return err
	}
	return finishDeviceLogin(cmd, ctx, in, dt, email, machine)
}

// announceDeviceGrant prints the verification URL/code (always visible,
// regardless of whether a browser can be opened) and best-effort opens it.
func announceDeviceGrant(cmd *cobra.Command, da *api.DeviceAuth) {
	outf(cmd.ErrOrStderr(), "To finish logging in, visit:\n\n    %s\n\nAnd enter code: %s\n\n",
		da.VerificationURI, da.UserCode)
	if err := openURL(da.VerificationURIComplete); err != nil {
		outf(cmd.ErrOrStderr(), "(could not open a browser automatically: %v)\n", err)
	}
	outf(cmd.ErrOrStderr(), "Waiting for approval...\n")
}

// finishDeviceLogin registers this machine with the issued key and persists
// credentials. Nothing is written before this succeeds in full.
func finishDeviceLogin(
	cmd *cobra.Command, ctx context.Context, in loginInput,
	dt *api.DeviceToken, email, machine string,
) error {
	authed := api.NewClient(in.apiURL, dt.CollectorKey)
	out, err := authed.Register(ctx, machine, email)
	if err != nil {
		return loginError(err)
	}
	store, err := config.NewFileStore()
	if err != nil {
		return err
	}
	if err := persistLogin(store, in.apiURL, dt.CollectorKey, email, machine, out); err != nil {
		return err
	}
	outf(cmd.OutOrStdout(),
		"Logged in. key %s… collector_id %d machine %s\n",
		keyPrefix(dt.CollectorKey), out.CollectorID, machine)
	return nil
}

// errDeviceCodeExpired is returned both when the local deadline (expires_in)
// passes and when the server reports expired_token, since they mean the same
// thing to the user: restart `login`.
var errDeviceCodeExpired = errors.New("device code expired; run `burndown-cli login` again")

// pollDeviceToken polls POST /api/device/token honoring the server's
// interval/slow_down guidance until the grant resolves or expires. sleep is
// injectable so tests don't wait in real time; production uses time.Sleep.
func pollDeviceToken(
	ctx context.Context, client *api.Client, da *api.DeviceAuth, sleep func(time.Duration),
) (*api.DeviceToken, error) {
	if sleep == nil {
		sleep = time.Sleep
	}
	interval := clampInterval(da.Interval)
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)

	for {
		sleep(interval)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, errDeviceCodeExpired
		}
		dt, err := client.DeviceToken(ctx, da.DeviceCode)
		if err == nil {
			return dt, nil
		}
		next, terminal, recognized := deviceTokenOutcome(err, interval)
		if !recognized {
			continue // transient network/server error; deadline still bounds the loop
		}
		if terminal != nil {
			return nil, terminal
		}
		interval = next
	}
}

// clampInterval floors a server-provided poll interval so a misbehaving
// value can never spin the poll loop.
func clampInterval(seconds int) time.Duration {
	interval := time.Duration(seconds) * time.Second
	if interval < devicePollFloor {
		return devicePollFloor
	}
	return interval
}

// deviceTokenOutcome maps a DeviceToken poll error to the next poll interval
// or a terminal error. recognized is false for anything other than a
// *DeviceTokenError (a transient network/server failure); callers should keep
// polling in that case, bounded by the overall deadline.
func deviceTokenOutcome(
	err error, interval time.Duration,
) (next time.Duration, terminal error, recognized bool) {
	var tokErr *api.DeviceTokenError
	if !errors.As(err, &tokErr) {
		return 0, nil, false
	}
	switch tokErr.Code {
	case api.DeviceCodeSlowDown:
		return interval + time.Second, nil, true
	case api.DeviceCodeAccessDenied:
		return 0, errors.New("device login was denied"), true
	case api.DeviceCodeExpiredToken:
		return 0, errDeviceCodeExpired, true
	default: // authorization_pending, or an unrecognized future code
		return interval, nil, true
	}
}

// isTerminalStdin reports whether cmd's stdin is an interactive terminal
// (vs. piped/redirected input, the headless/CI case).
func isTerminalStdin(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
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
	cfg.KeyExpiresAt = stringOrEmpty(out.KeyExpiresAt)
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
	cfg.KeyExpiresAt = stringOrEmpty(out.KeyExpiresAt)
	return store.Save(cfg)
}

// stringOrEmpty dereferences a nullable API string field, returning "" for
// nil (the config.Config convention for "no expiry").
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// loginError maps an auth failure to an actionable message, passing other
// errors through unchanged.
func loginError(err error) error {
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		detail := authErr.Detail
		if detail == "" {
			detail = authErr.Code
		}
		return fmt.Errorf(
			"collector key rejected: %s\ncheck the key from your dashboard and try again",
			detail)
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
	key, err := p.secret("Collector key (abd_...): ")
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
	if isTerminalStdin(p.cmd) {
		f, _ := p.cmd.InOrStdin().(*os.File)
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
