package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/platform"
	"github.com/agent-burn-down/desktop-client/internal/queue"
	"github.com/agent-burn-down/desktop-client/internal/version"
)

// backendServer serves /api/health and /ingest/v1/heartbeat with configurable
// status codes so backend and heartbeat checks can exercise each branch.
func backendServer(healthStatus, heartbeatStatus int) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(healthStatus)
		_, _ = w.Write([]byte(`{"ok":true,"uptime_seconds":1}`))
	})
	mux.HandleFunc("/ingest/v1/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(heartbeatStatus)
		if heartbeatStatus == http.StatusOK {
			_, _ = w.Write([]byte(`{"ok":true,"policy":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"detail":"collector key revoked"}`))
	})
	return httptest.NewServer(mux)
}

func TestCheckVersion(t *testing.T) {
	orig := version.Version
	defer func() { version.Version = orig }()

	tests := []struct {
		name    string
		cur     string
		status  int
		body    string
		want    Status
		hasHint bool
	}{
		{"up to date", "v1.0.0", 200, `{"tag_name":"v1.0.0"}`, Pass, false},
		{"stale", "v1.0.0", 200, `{"tag_name":"v2.0.0"}`, Warn, true},
		{"no releases", "v1.0.0", 404, ``, Pass, false},
		{"dev build", "dev", 200, `{"tag_name":"v1.0.0"}`, Pass, false},
		{"github down", "v1.0.0", 500, `boom`, Pass, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version.Version = tc.cur
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
			defer srv.Close()
			d := newDoctor(t, Config{GitHubURL: srv.URL, HTTPClient: srv.Client()})
			got := d.checkVersion(context.Background())
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", got.Status, tc.want, got.Detail)
			}
			if (got.Hint != "") != tc.hasHint {
				t.Errorf("hint = %q, hasHint want %v", got.Hint, tc.hasHint)
			}
		})
	}
}

func TestCheckConfig(t *testing.T) {
	good := configPerms{fileKnown: true, fileMode: 0o600, fileOK: true,
		dirKnown: true, dirMode: 0o700, dirOK: true}
	cfg := &config.Config{CollectorKey: "yaahc_key"}

	tests := []struct {
		name  string
		cfg   *config.Config
		err   error
		perms configPerms
		want  Status
	}{
		{"missing", nil, os.ErrNotExist, good, Fail},
		{"no key", &config.Config{}, nil, good, Fail},
		{"bad file perms", cfg, nil,
			configPerms{fileKnown: true, fileMode: 0o644, dirKnown: true, dirMode: 0o700, dirOK: true},
			Warn},
		{"bad dir perms", cfg, nil,
			configPerms{fileKnown: true, fileMode: 0o600, fileOK: true, dirKnown: true, dirMode: 0o755},
			Warn},
		{"all good", cfg, nil, good, Pass},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkConfig(tc.cfg, tc.err, tc.perms, "/tmp/config.json")
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", got.Status, tc.want, got.Detail)
			}
			if got.Status != Pass && got.Hint == "" {
				t.Error("non-pass config result missing hint")
			}
		})
	}
}

func TestCheckBackend(t *testing.T) {
	srv := backendServer(http.StatusOK, http.StatusOK)
	defer srv.Close()
	d := newDoctor(t, Config{HTTPClient: srv.Client()})

	ok := d.checkBackend(context.Background(), &config.Config{APIURL: srv.URL}, nil)
	if ok.Status != Pass {
		t.Errorf("healthy backend: status = %s (%s)", ok.Status, ok.Detail)
	}

	noURL := d.checkBackend(context.Background(), &config.Config{}, nil)
	if noURL.Status != Fail || noURL.Hint == "" {
		t.Errorf("missing api_url: %+v", noURL)
	}

	down := backendServer(http.StatusInternalServerError, http.StatusOK)
	down.Close()
	bad := d.checkBackend(context.Background(), &config.Config{APIURL: down.URL}, nil)
	if bad.Status != Fail || bad.Hint == "" {
		t.Errorf("down backend: %+v", bad)
	}
}

func TestCheckHeartbeat(t *testing.T) {
	cfg := func(url string) *config.Config {
		return &config.Config{APIURL: url, CollectorKey: "yaahc_key", CollectorID: 7}
	}

	okSrv := backendServer(http.StatusOK, http.StatusOK)
	defer okSrv.Close()
	d := newDoctor(t, Config{HTTPClient: okSrv.Client()})
	if r := d.checkHeartbeat(context.Background(), cfg(okSrv.URL), nil); r.Status != Pass {
		t.Errorf("ok heartbeat: %+v", r)
	}

	authSrv := backendServer(http.StatusOK, http.StatusUnauthorized)
	defer authSrv.Close()
	auth := d.checkHeartbeat(context.Background(), cfg(authSrv.URL), nil)
	if auth.Status != Fail || auth.Hint != loginHint {
		t.Errorf("auth failure should map to login hint: %+v", auth)
	}

	unreg := d.checkHeartbeat(context.Background(), &config.Config{APIURL: okSrv.URL}, nil)
	if unreg.Status != Fail || unreg.Hint != loginHint {
		t.Errorf("unregistered: %+v", unreg)
	}
}

func TestCheckKeyExpiry(t *testing.T) {
	future := func(d time.Duration) string { return time.Now().Add(d).Format(time.RFC3339) }
	tests := []struct {
		name string
		cfg  *config.Config
		err  error
		want Status
	}{
		{"no config", &config.Config{}, os.ErrNotExist, Pass},
		{"no key stored", &config.Config{}, nil, Pass},
		{"never expires", &config.Config{CollectorKey: "k"}, nil, Pass},
		{
			"far from expiry",
			&config.Config{CollectorKey: "k", KeyExpiresAt: future(60 * 24 * time.Hour)},
			nil, Pass,
		},
		{
			"within 14 days",
			&config.Config{CollectorKey: "k", KeyExpiresAt: future(5 * 24 * time.Hour)},
			nil, Warn,
		},
		{
			"already expired",
			&config.Config{CollectorKey: "k", KeyExpiresAt: future(-24 * time.Hour)},
			nil, Fail,
		},
		{
			"rotation failing",
			&config.Config{CollectorKey: "k", KeyExpiresAt: future(60 * 24 * time.Hour), RotationFailures: 2},
			nil, Warn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkKeyExpiry(tc.cfg, tc.err)
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s (%s)", got.Status, tc.want, got.Detail)
			}
			if got.Status != Pass && got.Hint == "" {
				t.Error("non-pass key_expiry result missing hint")
			}
		})
	}
}

func TestCheckDaemon(t *testing.T) {
	if r := checkDaemon(true, 8765); r.Status != Pass {
		t.Errorf("daemon up: %+v", r)
	}
	down := checkDaemon(false, 8765)
	if down.Status != Fail || down.Hint == "" {
		t.Errorf("daemon down: %+v", down)
	}
}

func TestCheckQueueDaemonUp(t *testing.T) {
	d := newDoctor(t, Config{})
	hz := &healthz{Counters: map[string]int64{counters.QueueDepth: 5}}
	r := d.checkQueue(true, hz)
	if r.Status != Pass {
		t.Errorf("daemon-up queue: %+v", r)
	}
}

func TestCheckQueueDaemonDown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "queue.db")
	q, err := queue.Open(dbPath, queue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue([]api.NormalizedEvent{{}, {}}); err != nil {
		t.Fatal(err)
	}
	_ = q.Close()

	d := newDoctor(t, Config{QueuePath: dbPath})
	r := d.checkQueue(false, nil)
	if r.Status != Pass {
		t.Errorf("healthy queue: %+v", r)
	}
}

func TestCheckQueueMissing(t *testing.T) {
	d := newDoctor(t, Config{QueuePath: filepath.Join(t.TempDir(), "absent.db")})
	if r := d.checkQueue(false, nil); r.Status != Pass {
		t.Errorf("missing queue db should pass: %+v", r)
	}
}

func TestCheckQueueCorrupt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "queue.db")
	if err := os.WriteFile(dbPath, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := newDoctor(t, Config{QueuePath: dbPath})
	r := d.checkQueue(false, nil)
	if r.Status != Fail || r.Hint == "" {
		t.Errorf("corrupt queue should fail with hint: %+v", r)
	}
}

func TestCheckService(t *testing.T) {
	tests := []struct {
		name    string
		status  platform.Status
		svcErr  error
		want    Status
		hasHint bool
	}{
		{"running", platform.Status{State: platform.StateRunning, PID: 10}, nil, Pass, false},
		{"not installed", platform.Status{State: platform.StateNotInstalled}, nil, Warn, true},
		{"stopped", platform.Status{State: platform.StateStopped}, nil, Warn, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := newDoctor(t, Config{Service: &fakeService{status: tc.status}})
			got := d.checkService()
			if got.Status != tc.want || (got.Hint != "") != tc.hasHint {
				t.Errorf("%+v, want status %s hint=%v", got, tc.want, tc.hasHint)
			}
		})
	}
}

func TestCheckServiceUnsupported(t *testing.T) {
	d := newDoctor(t, Config{ServiceErr: platform.ErrUnsupported})
	if r := d.checkService(); r.Status != Skip {
		t.Errorf("unsupported platform should skip: %+v", r)
	}
}

func TestCheckAgents(t *testing.T) {
	// Both agent dirs point at fresh temp dirs with a settings file that has no
	// OTEL keys, so both are detected but misconfigured -> fail.
	claudeDir := t.TempDir()
	codexDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BURNDOWN_CLAUDE_DIR", claudeDir)
	t.Setenv("BURNDOWN_CODEX_DIR", codexDir)
	r := checkAgents(8765)
	if r.Status != Fail || r.Hint == "" {
		t.Errorf("misconfigured agents should fail with hint: %+v", r)
	}
}

func TestCheckAgentsNoneDetected(t *testing.T) {
	// Point at a directory that does not exist and ensure the agent binaries are
	// not on PATH, so neither agent is detected -> warn.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("BURNDOWN_CLAUDE_DIR", missing)
	t.Setenv("BURNDOWN_CODEX_DIR", missing)
	t.Setenv("PATH", "")
	r := checkAgents(8765)
	if r.Status != Warn || r.Hint == "" {
		t.Errorf("no agents should warn with hint: %+v", r)
	}
}
