package doctor

import (
	"context"
	"net/http"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/platform"
	"github.com/agent-burn-down/desktop-client/internal/queue"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
	"github.com/agent-burn-down/desktop-client/internal/version"
)

// Status is the outcome of a single check.
type Status int

const (
	// Pass means the check succeeded.
	Pass Status = iota
	// Warn means a non-fatal problem the user should address.
	Warn
	// Fail means a broken component that needs fixing.
	Fail
	// Skip means the check does not apply here (for example, service
	// management on a non-darwin platform).
	Skip
)

// String renders the status as the lowercase token used in output and JSON.
func (s Status) String() string {
	switch s {
	case Pass:
		return "pass"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	case Skip:
		return "skip"
	default:
		return "unknown"
	}
}

// exitCode maps a status to its contribution to the process exit code.
func (s Status) exitCode() int {
	switch s {
	case Warn:
		return 1
	case Fail:
		return 2
	default:
		return 0
	}
}

// Result is one check's outcome. Hint is a one-line remediation command and is
// always set for Warn and Fail results.
type Result struct {
	Name   string
	Status Status
	Detail string
	Hint   string
}

func pass(name, detail string) Result {
	return Result{Name: name, Status: Pass, Detail: detail}
}

func warn(name, detail, hint string) Result {
	return Result{Name: name, Status: Warn, Detail: detail, Hint: hint}
}

func fail(name, detail, hint string) Result {
	return Result{Name: name, Status: Fail, Detail: detail, Hint: hint}
}

// Config configures a Doctor. Zero-value fields fall back to production
// defaults; tests override the injectable URLs, HTTP client, service, and queue
// path to isolate from the real network, launchd, and daemon.
type Config struct {
	// Port is the loopback receiver port probed for daemon liveness.
	Port int
	// HealthzURL overrides the daemon health URL (default derives from Port).
	HealthzURL string
	// GitHubURL overrides the latest-release endpoint (default is the public
	// GitHub API); tolerant of failure so it is safe to point anywhere.
	GitHubURL string
	// HTTPClient is used for all outbound probes (default: 3s timeout).
	HTTPClient *http.Client
	// Service and ServiceErr are the resolved platform service; when nil they
	// are resolved via platform.New (ServiceErr carries ErrUnsupported).
	Service    platform.Service
	ServiceErr error
	// QueuePath overrides the queue database path (default: queue.DefaultPath).
	QueuePath string
	// Store overrides the config store (default: config.NewFileStore).
	Store config.Store
	// ConfigPath and ConfigDir are the paths whose permissions are inspected
	// (defaults derive from the config package).
	ConfigPath string
	ConfigDir  string
}

// probeTimeout bounds each outbound HTTP probe so doctor stays fast when a
// backend or the daemon is unreachable.
const probeTimeout = 3 * time.Second

// Doctor holds resolved dependencies and runs the check suite.
type Doctor struct {
	port       int
	healthzURL string
	githubURL  string
	httpClient *http.Client
	service    platform.Service
	serviceErr error
	queuePath  string
	store      config.Store
	cfgPath    string
	cfgDir     string
}

// New resolves defaults for any unset Config field and returns a Doctor.
func New(c Config) (*Doctor, error) {
	d := &Doctor{
		port:       orInt(c.Port, receiver.DefaultPort),
		githubURL:  orStr(c.GitHubURL, version.LatestReleaseURL),
		httpClient: c.HTTPClient,
		service:    c.Service,
		serviceErr: c.ServiceErr,
		queuePath:  c.QueuePath,
		store:      c.Store,
	}
	if d.httpClient == nil {
		d.httpClient = &http.Client{Timeout: probeTimeout}
	}
	d.healthzURL = orStr(c.HealthzURL, healthzURLForPort(d.port))
	if err := d.resolvePaths(c); err != nil {
		return nil, err
	}
	d.resolveService()
	return d, nil
}

// resolvePaths fills the config store, config paths, and queue path defaults.
func (d *Doctor) resolvePaths(c Config) error {
	if d.store == nil {
		store, err := config.NewFileStore()
		if err != nil {
			return err
		}
		d.store = store
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	d.cfgDir = orStr(c.ConfigDir, dir)
	d.cfgPath = orStr(c.ConfigPath, configPath(d.store, dir))
	if d.queuePath == "" {
		qp, err := queue.DefaultPath()
		if err != nil {
			return err
		}
		d.queuePath = qp
	}
	return nil
}

// resolveService resolves the platform service when the caller did not inject
// one, capturing platform.ErrUnsupported (on non-darwin) as a non-fatal state
// the service check reports as a skip rather than an error.
func (d *Doctor) resolveService() {
	if d.service != nil || d.serviceErr != nil {
		return
	}
	d.service, d.serviceErr = platform.New()
}

// Run executes every check and returns the results in a stable order. It is
// safe to call with the daemon down, no config, and no network.
func (d *Doctor) Run(ctx context.Context) []Result {
	perms := statConfigPerms(d.cfgPath, d.cfgDir)
	cfg, cfgErr := d.store.Load()
	hz, hzErr := probeHealthz(ctx, d.httpClient, d.healthzURL)
	daemonUp := hzErr == nil
	return []Result{
		d.checkVersion(ctx),
		checkConfig(cfg, cfgErr, perms, d.cfgPath),
		d.checkBackend(ctx, cfg, cfgErr),
		d.checkHeartbeat(ctx, cfg, cfgErr),
		checkKeyExpiry(cfg, cfgErr),
		checkDaemon(daemonUp, d.port),
		checkAgents(d.port),
		d.checkQueue(daemonUp, hz),
		d.checkService(),
	}
}

// Aggregate returns the worst status across results (Fail > Warn > Pass), with
// Skip treated as Pass.
func Aggregate(results []Result) Status {
	worst := Pass
	for _, r := range results {
		if r.Status.exitCode() > worst.exitCode() {
			worst = r.Status
		}
	}
	return worst
}

// ExitCode returns the process exit code for a result set: 0 pass/skip, 1 warn,
// 2 fail.
func ExitCode(results []Result) int {
	code := 0
	for _, r := range results {
		if c := r.Status.exitCode(); c > code {
			code = c
		}
	}
	return code
}

func orInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

func orStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// configPath returns the config file path, preferring a FileStore's own path.
func configPath(store config.Store, dir string) string {
	if fs, ok := store.(*config.FileStore); ok {
		return fs.Path()
	}
	return dir + "/config.json"
}
