package uploader

import (
	"context"
	"errors"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
)

const (
	// rotationLeadTime is how far ahead of KeyExpiresAt a rotation is started.
	rotationLeadTime = 7 * 24 * time.Hour
	// rotationRetryInterval bounds actual rotate attempts to roughly once/day,
	// matching the backend's one-rotation-per-day-per-key limit.
	rotationRetryInterval = 24 * time.Hour
)

// RotateCheckOnce advances the key-rotation state machine by one step: resume
// verifying a pending key from a prior rotation (including one from before a
// restart — PendingKey's presence in config is what "resume" means here), or
// start a new rotation if the current key is nearing expiry. A no-op without
// a config store (some tests run the uploader without one).
func (u *Uploader) RotateCheckOnce(ctx context.Context) {
	if u.store == nil {
		return
	}
	cfg, err := u.store.Load()
	if err != nil {
		u.logger.Debug("skip rotation check: config load failed", "err", err)
		return
	}
	if cfg.PendingKey != "" {
		u.verifyPendingKey(ctx, cfg)
		return
	}
	if u.rotationDue(cfg) {
		u.attemptRotate(ctx)
	}
}

// rotationDue reports whether the current key is within rotationLeadTime of
// expiry and no rotation was attempted in the last rotationRetryInterval. A
// missing/unparseable KeyExpiresAt (a legacy, never-expiring key) is never due.
func (u *Uploader) rotationDue(cfg *config.Config) bool {
	expires, err := time.Parse(time.RFC3339, cfg.KeyExpiresAt)
	if err != nil {
		return false
	}
	if u.now().Add(rotationLeadTime).Before(expires) {
		return false
	}
	if cfg.LastRotationAt == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, cfg.LastRotationAt)
	if err != nil {
		return true
	}
	return u.now().Sub(last) >= rotationRetryInterval
}

// attemptRotate calls RotateKey and, on success, persists the new key as
// Pending (not yet live) before verifying it — a crash right after this call
// cannot lose the new key, since the server already considers it issued.
func (u *Uploader) attemptRotate(ctx context.Context) {
	out, err := u.client.RotateKey(ctx)
	now := u.now().Format(time.RFC3339)
	if err != nil {
		u.recordFailedRotateAttempt(err, now)
		return
	}
	u.mutateConfig(func(c *config.Config) {
		c.PendingKey = out.CollectorKey
		c.PendingKeyID = out.KeyID
		c.PendingKeyExpires = out.KeyExpiresAt
		c.OldKeyValidUntil = out.OldKeyValidUntil
		c.LastRotationAt = now
	})
	u.logger.Info("key rotation started; verifying new key")
}

// recordFailedRotateAttempt persists the attempt timestamp (so the once/day
// gate backs off regardless of outcome) and, for a genuine failure, bumps the
// failure counter. A 409 conflict is not a failure of this instance — another
// rotation is already in flight — so it does not count toward the escalating
// warning; the eventual key_rotated 401 hands off to the daemon's swap logic.
func (u *Uploader) recordFailedRotateAttempt(err error, at string) {
	var conflict *api.ConflictError
	isConflict := errors.As(err, &conflict)
	u.mutateConfig(func(c *config.Config) {
		c.LastRotationAt = at
		if !isConflict {
			c.RotationFailures++
		}
	})
	if isConflict {
		u.logger.Info("key rotation already in progress elsewhere; waiting for key_rotated to resync",
			"err", err)
		return
	}
	u.logger.Warn("key rotation failed; will retry tomorrow", "err", err)
}

// verifyPendingKey heartbeats with the pending key before committing it. The
// live client's key is swapped only for the duration of this call —
// RotateCheckOnce runs sequentially with FlushOnce/HeartbeatOnce within a
// single Run cycle, so nothing else uses the client concurrently.
func (u *Uploader) verifyPendingKey(ctx context.Context, cfg *config.Config) {
	previous := u.client.Key()
	u.client.SetKey(cfg.PendingKey)
	_, err := u.client.Heartbeat(ctx, u.collectorID, nil)
	if err == nil {
		u.commitPendingKey(cfg)
		return
	}
	u.client.SetKey(previous) // keep serving on the still-valid old key
	var authErr *api.AuthError
	if errors.As(err, &authErr) {
		// The pending key itself was rejected (unexpected, but possible if the
		// server-side overlap window already lapsed): discard it so the next
		// rotationDue check starts a fresh rotation instead of retrying a dead key.
		u.mutateConfig(func(c *config.Config) {
			c.PendingKey, c.PendingKeyExpires, c.OldKeyValidUntil = "", "", ""
			c.PendingKeyID = 0
			c.RotationFailures++
		})
		u.logger.Warn("pending rotated key rejected; discarding and will re-rotate", "err", err)
		return
	}
	// Transient network/server error: keep the pending key for another try
	// next cycle, still serving on the old key in the meantime.
	u.logger.Info("could not verify pending rotated key yet; will retry", "err", err)
}

// commitPendingKey promotes a verified pending key to the active key. The
// live client is already on this key (set by verifyPendingKey before the
// successful heartbeat), so only config needs to catch up.
func (u *Uploader) commitPendingKey(cfg *config.Config) {
	u.mutateConfig(func(c *config.Config) {
		c.CollectorKey = cfg.PendingKey
		c.KeyID = cfg.PendingKeyID
		c.KeyExpiresAt = cfg.PendingKeyExpires
		c.PendingKey, c.PendingKeyExpires, c.OldKeyValidUntil = "", "", ""
		c.PendingKeyID = 0
		c.RotationFailures = 0
	})
	u.logger.Info("key rotation committed")
}
