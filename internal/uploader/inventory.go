package uploader

import (
	"context"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/config"
)

// scheduleInventoryLocked applies consent changes immediately and reports
// whether the independent inventory worker should run. u.mu must be held.
func (u *Uploader) scheduleInventoryLocked(enabled bool) bool {
	if !enabled {
		if u.inventoryCancel != nil {
			u.inventoryCancel()
		}
		u.inventoryCancel = nil
		u.inventoryRunning = false
		u.inventoryNextAttempt = time.Time{}
		return false
	}
	if u.inventoryRunning || u.now().Before(u.inventoryNextAttempt) {
		return false
	}
	u.inventoryRunning = true
	return true
}

func (u *Uploader) runInventory(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			u.cancelInventory()
			return
		case <-u.inventoryTrigger:
			u.syncInventory(ctx)
		}
	}
}

func (u *Uploader) syncInventory(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	u.mu.Lock()
	if !u.policy.InventoryEnabled {
		u.inventoryRunning = false
		u.mu.Unlock()
		cancel()
		return
	}
	u.inventoryCancel = cancel
	u.mu.Unlock()

	u.mutateConfig(func(cfg *config.Config) {
		if cfg.InventoryStatus != "current" {
			cfg.InventoryStatus = "pending"
		}
	})
	count, observedAt, err := u.buildAndSendInventory(ctx)
	if err == nil && u.inventoryEnabled() {
		u.finishInventory("current", observedAt, count, inventoryRefreshInterval)
		cancel()
		return
	}
	if ctx.Err() == nil && u.inventoryEnabled() {
		u.finishInventory("error", "", 0, inventoryRetryInterval)
	}
	cancel()
}

func (u *Uploader) buildAndSendInventory(ctx context.Context) (int, string, error) {
	items, err := u.discoverInventory(ctx)
	if err != nil {
		return 0, "", err
	}
	if !u.inventoryEnabled() {
		return 0, "", context.Canceled
	}
	observedAt := u.now().UTC().Format(time.RFC3339)
	_, err = u.client.SendInventory(ctx, u.collectorID, observedAt, items)
	return len(items), observedAt, err
}

func (u *Uploader) finishInventory(status, observedAt string, count int, delay time.Duration) {
	u.storeMu.Lock()
	defer u.storeMu.Unlock()
	u.mu.Lock()
	if !u.policy.InventoryEnabled {
		u.inventoryRunning = false
		u.inventoryCancel = nil
		u.mu.Unlock()
		return
	}
	u.inventoryRunning = false
	u.inventoryCancel = nil
	u.inventoryNextAttempt = u.now().Add(delay)
	u.mu.Unlock()
	u.mutateConfigLocked(func(cfg *config.Config) {
		cfg.InventoryStatus = status
		cfg.InventoryItemCount = count
		if observedAt != "" {
			cfg.InventoryLastUploadAt = observedAt
		}
	})
}

func (u *Uploader) inventoryEnabled() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.policy.InventoryEnabled
}

func (u *Uploader) cancelInventory() {
	u.mu.Lock()
	if u.inventoryCancel != nil {
		u.inventoryCancel()
	}
	u.mu.Unlock()
}
