package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/platform"
	"github.com/agent-burn-down/desktop-client/internal/queue"
	"github.com/agent-burn-down/desktop-client/internal/setup"
	"github.com/agent-burn-down/desktop-client/internal/version"
)

// loginHint is the remediation shared by every check that needs valid stored
// credentials.
const loginHint = "run `burndown-cli login`"

// checkVersion compares the running version against the latest GitHub release.
// It never fails: no releases or an unreachable GitHub are a pass with a note.
func (d *Doctor) checkVersion(ctx context.Context) Result {
	latest, err := version.LatestRelease(ctx, d.httpClient, d.githubURL)
	cur := version.Version
	switch {
	case err != nil:
		return pass("version", fmt.Sprintf("running %s; could not check latest (%v)", cur, err))
	case latest == "":
		return pass("version", fmt.Sprintf("running %s; no published releases yet", cur))
	case cur == "dev":
		return pass("version", fmt.Sprintf("development build; latest release is %s", latest))
	case normalizeTag(cur) == normalizeTag(latest):
		return pass("version", fmt.Sprintf("up to date (%s)", cur))
	default:
		return warn("version",
			fmt.Sprintf("running %s, latest is %s", cur, latest),
			"download the latest release from GitHub")
	}
}

func normalizeTag(s string) string { return strings.TrimPrefix(s, "v") }

// checkConfig verifies the config exists, holds a key, and has safe file/dir
// permissions.
func checkConfig(cfg *config.Config, cfgErr error, perms configPerms, path string) Result {
	switch {
	case cfgErr != nil:
		return fail("config", "config missing or unreadable: "+cfgErr.Error(), loginHint)
	case cfg.CollectorKey == "":
		return fail("config", "no collector key stored", loginHint)
	case perms.fileKnown && !perms.fileOK:
		return warn("config",
			fmt.Sprintf("config file mode is %04o, want 0600", perms.fileMode),
			fmt.Sprintf("run `chmod 600 %s`", path))
	case perms.dirKnown && !perms.dirOK:
		return warn("config",
			fmt.Sprintf("config dir mode is %04o, want 0700", perms.dirMode),
			fmt.Sprintf("run `chmod 700 %s`", perms.dir))
	default:
		return pass("config", "present, key set, permissions 0600/0700")
	}
}

// checkBackend verifies the backend answers GET /api/health.
func (d *Doctor) checkBackend(ctx context.Context, cfg *config.Config, cfgErr error) Result {
	if cfgErr != nil || cfg.APIURL == "" {
		return fail("backend", "no api_url configured", loginHint)
	}
	if err := d.apiClient(cfg).Health(ctx); err != nil {
		return fail("backend", "GET /api/health failed: "+err.Error(),
			"check api_url and network connectivity")
	}
	return pass("backend", "reachable at "+cfg.APIURL)
}

// checkHeartbeat verifies a heartbeat succeeds with the stored key and
// collector id, distinguishing an auth rejection from a transport failure.
func (d *Doctor) checkHeartbeat(ctx context.Context, cfg *config.Config, cfgErr error) Result {
	if cfgErr != nil || cfg.CollectorKey == "" || cfg.CollectorID == 0 {
		return fail("heartbeat", "collector not registered", loginHint)
	}
	_, err := d.apiClient(cfg).Heartbeat(ctx, cfg.CollectorID, nil)
	if err == nil {
		return pass("heartbeat", fmt.Sprintf("ok (collector_id %d)", cfg.CollectorID))
	}
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		return fail("heartbeat", "authentication rejected: "+authErr.Detail, loginHint)
	}
	return fail("heartbeat", "heartbeat failed: "+err.Error(),
		"check api_url and network connectivity")
}

// checkKeyExpiry warns when the stored key is nearing expiry or when
// automatic rotation (#17) has been failing, so the user can re-login before
// either becomes an outage.
func checkKeyExpiry(cfg *config.Config, cfgErr error) Result {
	if cfgErr != nil || cfg.CollectorKey == "" {
		return pass("key_expiry", "n/a (no key stored yet)")
	}
	if cfg.RotationFailures > 0 {
		return warn("key_expiry",
			fmt.Sprintf("automatic rotation has failed %d time(s) in a row", cfg.RotationFailures),
			loginHint)
	}
	if cfg.KeyExpiresAt == "" {
		return pass("key_expiry", "key does not expire")
	}
	expires, err := time.Parse(time.RFC3339, cfg.KeyExpiresAt)
	if err != nil {
		return pass("key_expiry", "key expiry unparseable: "+cfg.KeyExpiresAt)
	}
	days := int(time.Until(expires).Hours() / 24)
	switch {
	case days < 0:
		return fail("key_expiry", "key has expired", loginHint)
	case days < 14:
		return warn("key_expiry", fmt.Sprintf("key expires in %dd", days), loginHint)
	default:
		return pass("key_expiry", fmt.Sprintf("expires in %dd", days))
	}
}

// apiClient builds a client with a no-op retry sleep so probes fail fast.
func (d *Doctor) apiClient(cfg *config.Config) *api.Client {
	return api.NewClient(cfg.APIURL, cfg.CollectorKey,
		api.WithHTTPClient(d.httpClient),
		api.WithSleep(func(time.Duration) {}))
}

// checkDaemon verifies the collector daemon answers on /healthz.
func checkDaemon(daemonUp bool, port int) Result {
	if daemonUp {
		return pass("daemon", fmt.Sprintf("listening on 127.0.0.1:%d", port))
	}
	return fail("daemon", "daemon not responding on /healthz",
		"run `burndown-cli service install` or `burndown-cli serve`")
}

// checkQueue reports queue depth and integrity. When the daemon is up it reads
// the depth from /healthz (the daemon holds the DB); when down it opens the DB
// directly and runs an integrity check.
func (d *Doctor) checkQueue(daemonUp bool, hz *healthz) Result {
	if daemonUp {
		return pass("queue",
			fmt.Sprintf("depth %d (reported by daemon)", hz.Counters[counters.QueueDepth]))
	}
	if _, err := os.Stat(d.queuePath); errors.Is(err, os.ErrNotExist) {
		return pass("queue", "no queue database yet")
	}
	q, err := queue.Open(d.queuePath, queue.Options{})
	if err != nil {
		return fail("queue", "cannot open queue db: "+err.Error(),
			fmt.Sprintf("file a bug or move %s aside and restart", d.queuePath))
	}
	defer func() { _ = q.Close() }()
	if err := q.Check(); err != nil {
		return fail("queue", "integrity check failed: "+err.Error(),
			fmt.Sprintf("move %s aside and restart the collector", d.queuePath))
	}
	depth, err := q.Depth()
	if err != nil {
		return fail("queue", "cannot read queue depth: "+err.Error(),
			fmt.Sprintf("move %s aside and restart the collector", d.queuePath))
	}
	return pass("queue", fmt.Sprintf("intact, depth %d", depth))
}

// checkService reports the launchd job state; it is skipped where service
// management is unsupported.
func (d *Doctor) checkService() Result {
	if errors.Is(d.serviceErr, platform.ErrUnsupported) {
		return Result{Name: "service", Status: Skip,
			Detail: "service management not supported on this platform"}
	}
	if d.serviceErr != nil {
		return fail("service", "cannot resolve service: "+d.serviceErr.Error(),
			"reinstall burndown-cli")
	}
	status, err := d.service.Status()
	if err != nil {
		return fail("service", "cannot query service: "+err.Error(),
			"run `burndown-cli service install`")
	}
	switch status.State {
	case platform.StateRunning:
		return pass("service", "running ("+status.String()+")")
	case platform.StateNotInstalled:
		return warn("service", "launchd job not installed",
			"run `burndown-cli service install`")
	default:
		return warn("service", "installed but not running",
			"run `burndown-cli service start`")
	}
}

// checkAgents inspects Claude Code and Codex OTEL configuration, reusing the
// setup planner. A detected-but-misconfigured agent fails; no agent detected
// warns; correctly configured agents pass.
func checkAgents(port int) Result {
	inspected := []agentCheck{
		inspectAgent("Claude Code", setup.DetectClaude, planClaude(port)),
		inspectAgent("Codex", setup.DetectCodex, planCodex(port)),
	}
	var misconfig, configured []string
	anyDetected := false
	for _, c := range inspected {
		if !c.detected {
			continue
		}
		anyDetected = true
		if c.ok {
			configured = append(configured, c.name)
		} else {
			misconfig = append(misconfig, c.name+": "+c.problem)
		}
	}
	switch {
	case len(misconfig) > 0:
		return fail("agents", strings.Join(misconfig, "; "), "run `burndown-cli setup`")
	case !anyDetected:
		return warn("agents", "no supported agent detected (Claude Code, Codex)",
			"install Claude Code or Codex, then run `burndown-cli setup`")
	default:
		return pass("agents", "OTEL configured: "+strings.Join(configured, ", "))
	}
}

// emptier is the plan surface checkAgents needs from setup's planners.
type emptier interface{ Empty() bool }

// agentCheck is one agent's inspection outcome.
type agentCheck struct {
	name     string
	detected bool
	ok       bool
	problem  string
}

func planClaude(port int) func() (emptier, error) {
	return func() (emptier, error) { return setup.PlanClaude(port) }
}

func planCodex(port int) func() (emptier, error) {
	return func() (emptier, error) { return setup.PlanCodex(port) }
}

// inspectAgent detects an agent and, when present, computes whether its OTEL
// settings are already complete.
func inspectAgent(
	name string, detect func() (bool, string, error), plan func() (emptier, error),
) agentCheck {
	detected, _, err := detect()
	if err != nil {
		return agentCheck{name: name, detected: true, problem: err.Error()}
	}
	if !detected {
		return agentCheck{name: name}
	}
	p, err := plan()
	if err != nil {
		return agentCheck{name: name, detected: true, problem: err.Error()}
	}
	if !p.Empty() {
		return agentCheck{name: name, detected: true, problem: "OTEL settings missing or incomplete"}
	}
	return agentCheck{name: name, detected: true, ok: true}
}
