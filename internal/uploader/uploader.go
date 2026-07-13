package uploader

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/queue"
	"github.com/agent-burn-down/desktop-client/internal/version"
)

// errNoStore marks an uploader running without a config store (some tests);
// pending-key/degraded-state reads are simply skipped in that case.
var errNoStore = errors.New("uploader: no config store configured")

const (
	// leaseDuration is how long a batch stays leased while its upload is in
	// flight. It must exceed the api client's worst-case call time (10s timeout
	// plus the 1+2+4s retry schedule) so a crash mid-upload re-leases cleanly.
	leaseDuration = 2 * time.Minute
	// maxBackoff caps the extra delay added between flush cycles while the
	// backend is unreachable, so an extended outage backs off without a storm.
	maxBackoff = 5 * time.Minute
	// degradedProbeInterval is the heartbeat-only cadence while Degraded: slow
	// enough that probing a revoked/invalid key is not itself a retry storm,
	// fast enough to notice a re-login within a few minutes.
	degradedProbeInterval = 5 * time.Minute
)

// uploadState is Active (normal operation) or Degraded (a 401 the daemon
// cannot self-heal from without a fresh login: key_revoked, key_invalid, or a
// key_expired/key_rotated recovery attempt that itself failed).
type uploadState int

const (
	stateActive uploadState = iota
	stateDegraded
)

// Config constructs an Uploader.
type Config struct {
	Client      *api.Client
	Queue       *queue.Queue
	Store       config.Store
	Counters    *counters.Registry
	Logger      *slog.Logger
	CollectorID int64
	Policy      api.Policy
}

// Uploader drains the durable queue to the backend on the policy flush cadence
// and refreshes the live policy via heartbeats. A single instance is safe for
// concurrent method calls; Run owns the periodic loop.
type Uploader struct {
	client      *api.Client
	queue       *queue.Queue
	store       config.Store
	counters    *counters.Registry
	logger      *slog.Logger
	collectorID int64

	now func() time.Time

	mu         sync.Mutex
	policy     api.Policy
	mode       uploadState
	authReason string
	backoff    time.Duration
}

// New returns an Uploader. Flushing is enabled until a 401 pauses it.
func New(cfg Config) *Uploader {
	return &Uploader{
		client:      cfg.Client,
		queue:       cfg.Queue,
		store:       cfg.Store,
		counters:    cfg.Counters,
		logger:      cfg.Logger,
		collectorID: cfg.CollectorID,
		now:         time.Now,
		policy:      cfg.Policy,
		mode:        stateActive,
	}
}

// Run drives flush and heartbeat cycles until ctx is cancelled. Each Active
// cycle heartbeats (refreshing the live policy), checks rotation, then
// flushes; a Degraded cycle only probes (see runCycle), on a slower cadence.
// The next cycle's delay is recomputed every time, so a policy change or a
// state transition takes effect within one cycle without a restart.
func (u *Uploader) Run(ctx context.Context) {
	u.HeartbeatOnce(ctx)
	if u.state() == stateActive {
		u.RotateCheckOnce(ctx)
	}
	timer := time.NewTimer(u.cycleDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			u.runCycle(ctx)
			timer.Reset(u.cycleDelay())
		}
	}
}

// runCycle executes one heartbeat/rotate/flush cycle. While Degraded it only
// probes: it first reloads the key from config (so an out-of-band
// `burndown-cli login` run while the daemon is up is picked up without a
// restart), then heartbeats; rotation and flush are skipped, since there is
// nothing productive to rotate or upload against a dead key.
func (u *Uploader) runCycle(ctx context.Context) {
	if u.state() == stateDegraded {
		u.reloadKeyIfChanged()
	}
	u.HeartbeatOnce(ctx)
	if u.state() != stateActive {
		return
	}
	u.RotateCheckOnce(ctx)
	u.FlushOnce(ctx)
}

// cycleDelay is the wait before the next cycle: the degraded probe interval
// while Degraded, otherwise the policy flush interval extended to the current
// backoff while the backend is unreachable.
func (u *Uploader) cycleDelay() time.Duration {
	if u.state() == stateDegraded {
		return degradedProbeInterval
	}
	return u.flushDelay()
}

func (u *Uploader) flushDelay() time.Duration {
	base := u.snapshotPolicy().FlushInterval()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.backoff > base {
		return u.backoff
	}
	return base
}

// reloadKeyIfChanged re-reads the collector key from config and swaps it into
// the live client if it changed. This is what lets a fresh `burndown-cli
// login` resume a running daemon's uploads without a restart.
func (u *Uploader) reloadKeyIfChanged() {
	cfg, err := u.loadConfig()
	if err != nil {
		return
	}
	if cfg.CollectorKey != "" && cfg.CollectorKey != u.client.Key() {
		u.client.SetKey(cfg.CollectorKey)
	}
}

// FlushOnce drains the queue in policy-sized batches until it is empty or a
// send fails. On failure the batch is nacked (re-leasable next cycle) and the
// cycle stops, so nothing is lost and there is no tight retry loop.
func (u *Uploader) FlushOnce(ctx context.Context) {
	if u.state() != stateActive {
		return
	}
	batchSize := u.snapshotPolicy().BatchSize()
	for {
		items, err := u.queue.LeaseBatch(batchSize, leaseDuration)
		if err != nil {
			u.counters.Add(counters.UploadErrors, 1)
			u.logger.Error("lease batch failed", "err", err)
			return
		}
		if len(items) == 0 {
			u.onFlushSuccess()
			return
		}
		if !u.sendBatch(ctx, items) {
			return
		}
	}
}

// sendBatch uploads one leased batch, acking on success and nacking on failure.
// It reports whether the flush cycle may continue.
func (u *Uploader) sendBatch(ctx context.Context, items []queue.Item) bool {
	ids, events := split(items)
	counts, err := u.client.SendEvents(ctx, u.collectorID, events)
	if err != nil {
		_ = u.queue.Nack(ids)
		u.handleSendError(ctx, err)
		return false
	}
	if err := u.queue.Ack(ids); err != nil {
		u.logger.Error("ack batch failed", "err", err)
	}
	u.onUploadSuccess(counts)
	return true
}

// handleSendError records a failed send. A 401 is dispatched by standardized
// code (see handleAuthError); other errors are logged once at reduced level
// and the backoff is widened so an outage does not spam.
func (u *Uploader) handleSendError(ctx context.Context, err error) {
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		u.handleAuthError(ctx, authErr)
		return
	}
	u.counters.Add(counters.UploadErrors, 1)
	u.widenBackoff()
	u.logger.Info("upload deferred: backend unreachable, will retry next cycle", "err", err)
}

// HeartbeatOnce reports liveness and swaps in any refreshed policy. A 401 is
// dispatched by standardized code (see handleAuthError); a success re-enables
// Active mode.
func (u *Uploader) HeartbeatOnce(ctx context.Context) {
	tel := u.telemetry()
	out, err := u.client.Heartbeat(ctx, u.collectorID, &tel)
	if err != nil {
		var authErr *api.AuthError
		if errors.As(err, &authErr) {
			u.handleAuthError(ctx, authErr)
			return
		}
		u.counters.Add(counters.HeartbeatErrors, 1)
		u.logger.Info("heartbeat deferred: backend unreachable", "err", err)
		return
	}
	u.onHeartbeatOK(out.Policy, out.KeyExpiresAt)
}

// telemetry builds the self-telemetry snapshot sent with each heartbeat. It
// merges the shared counters registry with the live queue depth and derives the
// reported shape via counters.Report, the same path `status --json` uses over
// /healthz, so the two always agree.
func (u *Uploader) telemetry() counters.Telemetry {
	snap := u.counters.Snapshot()
	if depth, err := u.queue.Depth(); err == nil {
		snap[counters.QueueDepth] = depth
	}
	return counters.Report(snap, version.Version)
}

// onHeartbeatOK swaps in the refreshed policy and, when present, the current
// key's expiry (nil for a legacy server or a never-expiring key — left
// untouched rather than cleared, since a legacy client that never sends
// key_expires_at must not be misread as "just started expiring").
func (u *Uploader) onHeartbeatOK(policy api.Policy, keyExpiresAt *string) {
	u.enterActive()
	u.mu.Lock()
	u.policy = policy
	u.mu.Unlock()
	u.counters.Set(counters.LastHeartbeatAt, u.now().Unix())
	u.mutateConfig(func(cfg *config.Config) {
		cfg.Policy = policy
		if keyExpiresAt != nil {
			cfg.KeyExpiresAt = *keyExpiresAt
		}
	})
}

// handleAuthError dispatches a 401 by its standardized code (#21's contract):
//   - key_rotated: adopt a cached pending key (from a rotation this instance
//     or another one started) if one exists; otherwise there is no way to
//     recover the new key, so this degrades like key_invalid.
//   - key_expired: attempt one immediate rotate+verify (bypassing the T-7d/
//     once-a-day gate — the key is already dead right now); success recovers,
//     failure degrades. #17's proactive rotation is what normally prevents
//     this path from firing at all.
//   - key_revoked / key_invalid / anything unrecognized: degrade immediately.
//     No retry storm: FlushOnce stops entirely and the heartbeat probe cadence
//     slows to degradedProbeInterval (see runCycle/cycleDelay).
func (u *Uploader) handleAuthError(ctx context.Context, authErr *api.AuthError) {
	switch authErr.Code {
	case api.CodeKeyRotated:
		u.recoverViaPendingKeyOrDegrade(ctx)
	case api.CodeKeyExpired:
		u.attemptRotate(ctx)
		u.recoverViaPendingKeyOrDegrade(ctx)
	default:
		u.enterDegraded(authErr.Code)
	}
}

// recoverViaPendingKeyOrDegrade tries to verify+adopt whatever pending key is
// currently persisted; degrades if there is none or adopting it fails.
func (u *Uploader) recoverViaPendingKeyOrDegrade(ctx context.Context) {
	cfg, err := u.loadConfig()
	if err != nil || cfg.PendingKey == "" {
		u.enterDegraded(api.CodeKeyInvalid)
		return
	}
	if u.verifyPendingKey(ctx, cfg) {
		u.enterActive()
		return
	}
	u.enterDegraded(api.CodeKeyInvalid)
}

// enterDegraded transitions to Degraded and persists the reason (so status/
// doctor are accurate even while the daemon is down). Logs only on the actual
// Active -> Degraded transition, never once per cycle.
func (u *Uploader) enterDegraded(reason string) {
	u.mu.Lock()
	transitioning := u.mode != stateDegraded
	u.mode = stateDegraded
	u.authReason = reason
	u.mu.Unlock()
	u.counters.Set(counters.AuthFailed, 1)
	u.mutateConfig(func(cfg *config.Config) { cfg.AuthReason = reason })
	if transitioning {
		u.logger.Warn("collector key unauthorized; uploads paused, still queueing locally",
			"reason", reason, "hint", "run `burndown-cli login`")
	}
}

// enterActive transitions to Active (a no-op if already there) and clears any
// degraded reason.
func (u *Uploader) enterActive() {
	u.mu.Lock()
	wasDegraded := u.mode == stateDegraded
	u.mode = stateActive
	u.authReason = ""
	u.backoff = 0
	u.mu.Unlock()
	u.counters.Set(counters.AuthFailed, 0)
	u.mutateConfig(func(cfg *config.Config) { cfg.AuthReason = "" })
	if wasDegraded {
		u.logger.Info("collector key recovered; resuming uploads")
	}
}

// loadConfig is a read-only counterpart to mutateConfig for callers that only
// need to inspect current state (e.g. whether a pending key exists).
func (u *Uploader) loadConfig() (*config.Config, error) {
	if u.store == nil {
		return nil, errNoStore
	}
	return u.store.Load()
}

// mutateConfig best-effort loads the config, applies fn, and saves it back so
// changes survive a restart. Failures are logged, not surfaced — the uploader
// keeps running on in-memory state either way. Shared by policy persistence,
// key-rotation state, and auth-reason persistence.
func (u *Uploader) mutateConfig(fn func(*config.Config)) {
	if u.store == nil {
		return
	}
	cfg, err := u.store.Load()
	if err != nil {
		u.logger.Debug("skip config persist: load failed", "err", err)
		return
	}
	fn(cfg)
	if err := u.store.Save(cfg); err != nil {
		u.logger.Debug("skip config persist: save failed", "err", err)
	}
}

func (u *Uploader) onUploadSuccess(counts *api.Counts) {
	if counts != nil {
		u.counters.Add(counters.Uploaded, int64(counts.Accepted))
		u.counters.Add(counters.UploadDropped, int64(counts.Dropped))
	}
	u.counters.Set(counters.LastUploadAt, u.now().Unix())
}

func (u *Uploader) onFlushSuccess() {
	u.mu.Lock()
	u.backoff = 0
	u.mu.Unlock()
}

func (u *Uploader) widenBackoff() {
	u.mu.Lock()
	defer u.mu.Unlock()
	base := u.policy.FlushInterval()
	if u.backoff == 0 {
		u.backoff = base
	}
	u.backoff *= 2
	if u.backoff > maxBackoff {
		u.backoff = maxBackoff
	}
}

func (u *Uploader) state() uploadState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.mode
}

func (u *Uploader) snapshotPolicy() api.Policy {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.policy
}

// split separates leased items into their row ids and events, preserving
// order, and attaches each item's queue-minted EventID as the wire event_id
// so retries after a crash-after-send-before-ack reuse the same idempotency
// key and the backend dedupes them.
func split(items []queue.Item) ([]int64, []api.NormalizedEvent) {
	ids := make([]int64, len(items))
	events := make([]api.NormalizedEvent, len(items))
	for i, it := range items {
		ids[i] = it.ID
		events[i] = it.Event
		events[i].EventID = &items[i].EventID
	}
	return ids, events
}
