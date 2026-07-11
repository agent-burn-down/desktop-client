package daemon

import (
	"context"
	"sync"
)

// Run starts the uploader loop and blocks until ctx is cancelled (SIGTERM or
// SIGINT), then drains and closes the pipeline. It always returns the drain
// result so the caller can surface a dirty shutdown.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("collector started", "addr", d.receiver.Addr())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.uploader.Run(ctx)
	}()
	<-ctx.Done()
	d.logger.Info("shutdown signal received; draining")
	return d.drain(&wg)
}

// drain performs an ordered graceful shutdown: stop the receiver (letting
// in-flight requests finish), stop the uploader loop, flush the filter's
// remaining rollups, make one best-effort final upload, then close the queue.
// The result is a consistent queue with no stuck leases.
func (d *Daemon) drain(wg *sync.WaitGroup) error {
	shutCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := d.receiver.Shutdown(shutCtx); err != nil {
		d.logger.Warn("receiver shutdown incomplete", "err", err)
	}
	wg.Wait()
	if rollups := d.filter.Flush(); len(rollups) > 0 {
		if err := d.queue.Enqueue(rollups); err != nil {
			d.logger.Error("final rollup enqueue failed", "err", err)
		}
	}
	flushCtx, flushCancel := context.WithTimeout(context.Background(), finalFlushTimeout)
	defer flushCancel()
	d.uploader.FlushOnce(flushCtx)
	return d.close()
}

// close releases the queue and log file.
func (d *Daemon) close() error {
	err := d.queue.Close()
	if d.logFile != nil {
		_ = d.logFile.Close()
	}
	return err
}
