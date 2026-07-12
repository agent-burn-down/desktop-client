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

const (
	// leaseDuration is how long a batch stays leased while its upload is in
	// flight. It must exceed the api client's worst-case call time (10s timeout
	// plus the 1+2+4s retry schedule) so a crash mid-upload re-leases cleanly.
	leaseDuration = 2 * time.Minute
	// maxBackoff caps the extra delay added between flush cycles while the
	// backend is unreachable, so an extended outage backs off without a storm.
	maxBackoff = 5 * time.Minute
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

	mu      sync.Mutex
	policy  api.Policy
	authOK  bool
	backoff time.Duration
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
		authOK:      true,
	}
}

// Run drives flush and heartbeat cycles until ctx is cancelled. Each cycle
// heartbeats (refreshing the live policy) then flushes; the next cycle is
// scheduled from the current policy's flush interval, so a policy change takes
// effect within one cycle without a restart.
func (u *Uploader) Run(ctx context.Context) {
	u.HeartbeatOnce(ctx)
	u.RotateCheckOnce(ctx)
	timer := time.NewTimer(u.flushDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			u.HeartbeatOnce(ctx)
			u.RotateCheckOnce(ctx)
			u.FlushOnce(ctx)
			timer.Reset(u.flushDelay())
		}
	}
}

// flushDelay is the wait before the next cycle: the policy flush interval,
// extended to the current backoff while the backend is unreachable.
func (u *Uploader) flushDelay() time.Duration {
	base := u.snapshotPolicy().FlushInterval()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.backoff > base {
		return u.backoff
	}
	return base
}

// FlushOnce drains the queue in policy-sized batches until it is empty or a
// send fails. On failure the batch is nacked (re-leasable next cycle) and the
// cycle stops, so nothing is lost and there is no tight retry loop.
func (u *Uploader) FlushOnce(ctx context.Context) {
	if !u.authorized() {
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
		u.handleSendError(err)
		return false
	}
	if err := u.queue.Ack(ids); err != nil {
		u.logger.Error("ack batch failed", "err", err)
	}
	u.onUploadSuccess(counts)
	return true
}

// handleSendError records a failed send. A 401 pauses flushing until the next
// successful heartbeat; other errors are logged once at reduced level and the
// backoff is widened so an outage does not spam.
func (u *Uploader) handleSendError(err error) {
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		u.onAuthFailure()
		u.logger.Warn("upload rejected: collector key unauthorized; pausing until heartbeat re-auth",
			"detail", authErr.Detail)
		return
	}
	u.counters.Add(counters.UploadErrors, 1)
	u.widenBackoff()
	u.logger.Info("upload deferred: backend unreachable, will retry next cycle", "err", err)
}

// HeartbeatOnce reports liveness and swaps in any refreshed policy. A 401
// leaves flushing paused; a success re-enables it.
func (u *Uploader) HeartbeatOnce(ctx context.Context) {
	tel := u.telemetry()
	out, err := u.client.Heartbeat(ctx, u.collectorID, &tel)
	if err != nil {
		var authErr *api.AuthError
		if errors.As(err, &authErr) {
			u.onAuthFailure()
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
	u.mu.Lock()
	u.policy = policy
	u.authOK = true
	u.backoff = 0
	u.mu.Unlock()
	u.counters.Set(counters.AuthFailed, 0)
	u.counters.Set(counters.LastHeartbeatAt, u.now().Unix())
	u.mutateConfig(func(cfg *config.Config) {
		cfg.Policy = policy
		if keyExpiresAt != nil {
			cfg.KeyExpiresAt = *keyExpiresAt
		}
	})
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

func (u *Uploader) onAuthFailure() {
	u.mu.Lock()
	u.authOK = false
	u.mu.Unlock()
	u.counters.Set(counters.AuthFailed, 1)
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

func (u *Uploader) authorized() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.authOK
}

func (u *Uploader) snapshotPolicy() api.Policy {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.policy
}

// split separates leased items into their row ids and events, preserving order.
func split(items []queue.Item) ([]int64, []api.NormalizedEvent) {
	ids := make([]int64, len(items))
	events := make([]api.NormalizedEvent, len(items))
	for i, it := range items {
		ids[i] = it.ID
		events[i] = it.Event
	}
	return ids, events
}
