// Package daemon composes the collector pipeline into a single long-running
// process: the loopback OTLP receiver feeds normalize → filter → durable queue,
// and the uploader drains the queue to the backend on the policy cadence. It
// owns structured logging, a shared counters registry served on /healthz, and
// graceful drain on SIGTERM/SIGINT.
package daemon

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/agent-burn-down/desktop-client/internal/api"
	"github.com/agent-burn-down/desktop-client/internal/config"
	"github.com/agent-burn-down/desktop-client/internal/counters"
	"github.com/agent-burn-down/desktop-client/internal/filter"
	"github.com/agent-burn-down/desktop-client/internal/normalize"
	"github.com/agent-burn-down/desktop-client/internal/queue"
	"github.com/agent-burn-down/desktop-client/internal/receiver"
	"github.com/agent-burn-down/desktop-client/internal/uploader"
)

const (
	// rollupInterval bounds how often the filter's accumulated token/cost
	// rollups are flushed into the queue while traffic flows.
	rollupInterval = time.Minute
	// drainTimeout bounds the receiver's graceful shutdown.
	drainTimeout = 5 * time.Second
	// finalFlushTimeout bounds the best-effort final upload on shutdown.
	finalFlushTimeout = 10 * time.Second
	// retentionInterval is how often acked rows older than the retention window
	// are pruned while the daemon runs; a prune also happens once at startup.
	retentionInterval = time.Hour
)

// Options configures a Daemon. Config and Store are required.
type Options struct {
	Config  *config.Config
	Store   config.Store
	Port    int
	Verbose bool
	Repo    string
}

// Daemon is the composed collector process.
type Daemon struct {
	queue    *queue.Queue
	filter   *filter.Filter
	receiver *receiver.Server
	uploader *uploader.Uploader
	counters *counters.Registry
	logger   *slog.Logger
	logFile  io.Closer
	repo     string

	retentionDays int

	rollupMu   sync.Mutex
	lastRollup time.Time
}

// New builds the daemon: logger, counters, queue, filter, api client, uploader,
// and receiver wired to the pipeline handler. The caller owns Run and Close.
func New(opts Options) (*Daemon, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	logger, logFile, err := newLogger(dir, opts.Verbose)
	if err != nil {
		return nil, err
	}
	d, err := assemble(opts, dir, logger, logFile)
	if err != nil {
		_ = logFile.Close()
		return nil, err
	}
	return d, nil
}

// assemble constructs the queue-backed pipeline and receiver.
func assemble(opts Options, dir string, logger *slog.Logger, logFile io.Closer) (*Daemon, error) {
	q, err := queue.Open(dir+"/queue.db", queue.Options{})
	if err != nil {
		return nil, err
	}
	reg := counters.New()
	cfg := opts.Config
	client := api.NewClient(cfg.APIURL, cfg.CollectorKey, api.WithLogger(logger))
	d := &Daemon{
		queue:         q,
		filter:        filter.New(nil, nil),
		counters:      reg,
		logger:        logger,
		logFile:       logFile,
		repo:          opts.Repo,
		retentionDays: cfg.Retention(),
		lastRollup:    time.Now(),
	}
	d.uploader = uploader.New(uploader.Config{
		Client: client, Queue: q, Store: opts.Store, Counters: reg,
		Logger: logger, CollectorID: cfg.CollectorID, Policy: cfg.Policy,
	})
	if err := d.startReceiver(opts.Port); err != nil {
		_ = q.Close()
		return nil, err
	}
	return d, nil
}

// startReceiver builds and binds the loopback receiver.
func (d *Daemon) startReceiver(port int) error {
	srv, err := receiver.New(receiver.Config{
		Port:     port,
		Handler:  d.handleLogs,
		Counters: d.countersSnapshot,
	})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	d.receiver = srv
	return nil
}

// handleLogs is the receiver's logs handler: normalize → filter → enqueue kept
// events plus any due rollups. It returns (accepted, dropped) counts for the
// OTLP response; the receiver always answers 200 regardless.
func (d *Daemon) handleLogs(payload map[string]any) (accepted, dropped int) {
	events, normDropped := normalize.NormalizeLogBatch(payload, d.repo)
	d.counters.Add(counters.Received, int64(len(events)+normDropped))
	d.counters.Add(counters.Normalized, int64(len(events)))
	kept := d.filter.Apply(events)
	d.counters.Add(counters.Filtered, int64(len(events)-len(kept)))
	toEnqueue := append(kept, d.dueRollups()...)
	if err := d.queue.Enqueue(toEnqueue); err != nil {
		d.counters.Add(counters.Errors, 1)
		d.logger.Error("enqueue failed", "err", err)
		return 0, len(events) + normDropped
	}
	d.counters.Add(counters.Queued, int64(len(toEnqueue)))
	return len(kept), normDropped + (len(events) - len(kept))
}

// dueRollups flushes the filter's accumulated rollups when rollupInterval has
// elapsed since the last flush, at most one flush per interval.
func (d *Daemon) dueRollups() []api.NormalizedEvent {
	d.rollupMu.Lock()
	if time.Since(d.lastRollup) < rollupInterval {
		d.rollupMu.Unlock()
		return nil
	}
	d.lastRollup = time.Now()
	d.rollupMu.Unlock()
	rollups := d.filter.Flush()
	d.counters.Add(counters.Rollups, int64(len(rollups)))
	return rollups
}

// countersSnapshot merges the shared registry with the live queue depth for the
// receiver's /healthz response.
func (d *Daemon) countersSnapshot() map[string]int64 {
	snap := d.counters.Snapshot()
	if depth, err := d.queue.Depth(); err == nil {
		snap[counters.QueueDepth] = depth
	}
	return snap
}

// runRetention prunes acked rows older than the retention window once at
// startup and then every retentionInterval until ctx is cancelled.
func (d *Daemon) runRetention(ctx context.Context) {
	d.pruneOnce()
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.pruneOnce()
		}
	}
}

// pruneOnce deletes acked rows older than the retention window, keeping the
// local stats database bounded. Failures are logged, not fatal.
func (d *Daemon) pruneOnce() {
	cutoff := time.Now().Add(-time.Duration(d.retentionDays) * 24 * time.Hour)
	deleted, err := d.queue.PruneAcked(cutoff)
	if err != nil {
		d.logger.Error("retention prune failed", "err", err)
		return
	}
	if deleted > 0 {
		d.logger.Info("retention pruned acked rows",
			"deleted", deleted, "retention_days", d.retentionDays)
	}
}

// ReceiverAddr returns the receiver's bound address (useful when Port is 0).
func (d *Daemon) ReceiverAddr() string { return d.receiver.Addr() }
