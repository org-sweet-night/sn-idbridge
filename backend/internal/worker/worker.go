// SPDX-License-Identifier: MIT

// Package worker provides background job processing for sync scheduling
// and asynchronous audit log writes.
package worker

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Config controls Worker behaviour.
type Config struct {
	// PollInterval is how often the scheduler checks for pending work.
	// Defaults to 30 seconds when zero.
	PollInterval time.Duration

	// MaxConcurrentSyncs is the total number of sync jobs that may run
	// simultaneously across all entities.  Defaults to 3 when zero.
	MaxConcurrentSyncs int

	// MaxConcurrentPerEntity limits how many sync jobs a single entity
	// may execute at the same time.  Defaults to 1 when zero.
	MaxConcurrentPerEntity int

	// AuditBufferSize is the capacity of the async audit event channel.
	// Defaults to 1024 when zero.
	AuditBufferSize int

	// AcquireTimeout bounds waiting for a background database slot. Defaults to
	// 2 seconds when zero so a busy pool fails fast.
	AcquireTimeout time.Duration

	// OperationTimeout bounds an admitted background operation. Defaults to
	// 5 minutes when zero; this is deliberately separate from AcquireTimeout
	// because full directory syncs can legitimately run longer than a pool wait.
	OperationTimeout time.Duration

	// MaxConcurrentOperations reserves pool capacity for interactive traffic
	// by limiting all background database activity. Defaults to 2 when zero.
	MaxConcurrentOperations int
}

// applyDefaults fills in zero-valued fields with sensible defaults.
func (c *Config) applyDefaults() {
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.MaxConcurrentSyncs <= 0 {
		c.MaxConcurrentSyncs = 3
	}
	if c.MaxConcurrentPerEntity <= 0 {
		c.MaxConcurrentPerEntity = 1
	}
	if c.AuditBufferSize <= 0 {
		c.AuditBufferSize = 1024
	}
	if c.AcquireTimeout <= 0 {
		c.AcquireTimeout = 2 * time.Second
	}
	if c.OperationTimeout <= 0 {
		c.OperationTimeout = 5 * time.Minute
	}
	if c.MaxConcurrentOperations <= 0 {
		c.MaxConcurrentOperations = 2
	}
}

// Worker orchestrates background goroutines for sync scheduling and
// asynchronous audit processing.  Create one with New, then call Start
// to launch the background goroutines and Stop to shut them down.
type Worker struct {
	cfg       Config
	logger    *zap.Logger
	scheduler *Scheduler
	audit     *AuditProcessor
	cleanup   *CleanupRunner
	webhooks  *WebhookRecoveryPoller

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// New creates a Worker.  The scheduler and audit processor are wired
// internally; callers supply the SyncRunner and AuditWriter that connect
// to the database.
func New(cfg Config, logger *zap.Logger, runner *SyncRunner, auditSvc AuditWriter, cleanupRunners ...*CleanupRunner) *Worker {
	cfg.applyDefaults()

	limiter := newBackgroundLimiter(cfg.MaxConcurrentOperations, cfg.AcquireTimeout, cfg.OperationTimeout)
	audit := NewAuditProcessor(auditSvc, cfg.AuditBufferSize, logger)
	scheduler := NewScheduler(runner, audit, cfg.MaxConcurrentSyncs, cfg.MaxConcurrentPerEntity, logger)
	audit.limiter = limiter
	scheduler.limiter = limiter
	var cleanup *CleanupRunner
	if len(cleanupRunners) > 0 {
		cleanup = cleanupRunners[0]
		cleanup.limiter = limiter
	}

	return &Worker{
		cfg:       cfg,
		logger:    logger,
		scheduler: scheduler,
		audit:     audit,
		cleanup:   cleanup,
	}
}

// Scheduler returns the underlying Scheduler so callers can trigger jobs.
func (w *Worker) Scheduler() *Scheduler { return w.scheduler }

// AuditProcessor returns the underlying AuditProcessor so callers can
// enqueue fire-and-forget audit events.
func (w *Worker) AuditProcessor() *AuditProcessor { return w.audit }

// SetWebhookRecoveryStore enables durable webhook recovery. It must be called
// before Start so polling begins with the rest of the worker.
func (w *Worker) SetWebhookRecoveryStore(store webhookRecoveryStore) {
	w.webhooks = NewWebhookRecoveryPoller(store, w.scheduler, w.cfg.PollInterval, w.logger)
	w.webhooks.limiter = w.scheduler.limiter
}

// Start launches all background goroutines.  It is safe to call only
// once; subsequent calls are no-ops.
func (w *Worker) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)

	w.logger.Info("starting worker",
		zap.Duration("poll_interval", w.cfg.PollInterval),
		zap.Int("max_concurrent_syncs", w.cfg.MaxConcurrentSyncs),
		zap.Int("max_per_entity", w.cfg.MaxConcurrentPerEntity),
		zap.Int("audit_buffer", w.cfg.AuditBufferSize),
		zap.Duration("acquire_timeout", w.cfg.AcquireTimeout),
		zap.Duration("operation_timeout", w.cfg.OperationTimeout),
		zap.Int("max_concurrent_operations", w.cfg.MaxConcurrentOperations),
	)

	// Audit drain goroutine.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.audit.Run(ctx)
	}()

	// Scheduler dispatch goroutine.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.scheduler.Run(ctx)
	}()

	if w.webhooks != nil {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.webhooks.Run(ctx)
		}()
	}

	if w.cleanup != nil {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.cleanup.Run(ctx)
		}()
	}
}

// Stop cancels the worker context and blocks until all goroutines exit.
// It drains any remaining audit events before returning.
func (w *Worker) Stop() {
	if w.cancel == nil {
		return
	}
	w.logger.Info("stopping worker")
	w.cancel()
	w.wg.Wait()

	// Final drain of audit buffer after all goroutines have exited.
	w.audit.Drain()

	w.logger.Info("worker stopped")
}
