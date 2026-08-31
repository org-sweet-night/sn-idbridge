// SPDX-License-Identifier: MIT

package worker

import (
	"context"
	"sync"

	"github.com/smices/open-idb/internal/audit"
	"go.uber.org/zap"
)

// syncRequest is an internal message enqueued by TriggerFullSync and
// consumed by the scheduler dispatch loop.
type syncRequest struct {
	EntityID           string
	SourceID           string
	Provider           string
	SyncType           string
	RecoveryClaimToken string
}

// Scheduler manages an in-memory job queue for sync operations.  It
// enforces a global concurrency limit and a per-entity concurrency
// limit, dispatching work to the SyncRunner.
type Scheduler struct {
	runner *SyncRunner
	audit  *AuditProcessor
	logger *zap.Logger

	queue chan syncRequest

	// Global semaphore: buffered channel whose capacity equals the
	// maximum number of concurrent sync jobs across all entities.
	globalSem chan struct{}

	// Per-entity semaphores.  Each entity gets a semaphore channel
	// with capacity = maxConcurrentPerEntity, created on first use.
	mu               sync.Mutex
	entitySemaphores map[string]chan struct{}
	maxPerEntity     int
	limiter          *backgroundLimiter
}

// NewScheduler creates a Scheduler.  Call Run to start the dispatch
// loop.
func NewScheduler(
	runner *SyncRunner,
	audit *AuditProcessor,
	maxConcurrent int,
	maxPerEntity int,
	logger *zap.Logger,
) *Scheduler {
	return &Scheduler{
		runner:           runner,
		audit:            audit,
		logger:           logger,
		queue:            make(chan syncRequest, maxConcurrent*4),
		globalSem:        make(chan struct{}, maxConcurrent),
		entitySemaphores: make(map[string]chan struct{}),
		maxPerEntity:     maxPerEntity,
	}
}

// TriggerFullSync enqueues a full sync job for the given entity and
// source.  It returns immediately.  If the queue is full the request
// is dropped and a warning is logged.
func (s *Scheduler) TriggerFullSync(entityID, sourceID string) {
	s.TriggerFullSyncWithProvider(entityID, sourceID, "")
}

// TriggerFullSyncWithProvider is like TriggerFullSync but allows
// specifying the provider name.
func (s *Scheduler) TriggerFullSyncWithProvider(entityID, sourceID, provider string) {
	req := syncRequest{
		EntityID: entityID,
		SourceID: sourceID,
		Provider: provider,
		SyncType: "full",
	}
	select {
	case s.queue <- req:
		s.logger.Info("sync job enqueued",
			zap.String("entity_id", entityID),
			zap.String("source_id", sourceID),
		)
	default:
		s.logger.Warn("sync queue full, dropping request",
			zap.String("entity_id", entityID),
			zap.String("source_id", sourceID),
		)
	}
}

// TriggerIncrementalSync enqueues an incremental sync job for the given entity and
// source.  It returns immediately.  If the queue is full the request is dropped
// and a warning is logged.
func (s *Scheduler) TriggerIncrementalSync(entityID, sourceID string) {
	s.TriggerIncrementalSyncWithProvider(entityID, sourceID, "")
}

// TriggerIncrementalSyncWithProvider is like TriggerIncrementalSync but allows
// specifying the provider name.
func (s *Scheduler) TriggerIncrementalSyncWithProvider(entityID, sourceID, provider string) {
	req := syncRequest{
		EntityID: entityID,
		SourceID: sourceID,
		Provider: provider,
		SyncType: "incremental",
	}
	select {
	case s.queue <- req:
		s.logger.Info("sync job enqueued",
			zap.String("entity_id", entityID),
			zap.String("source_id", sourceID),
		)
	default:
		s.logger.Warn("sync queue full, dropping request",
			zap.String("entity_id", entityID),
			zap.String("source_id", sourceID),
		)
	}
}

// TriggerRecoveredWebhookSync enqueues an incremental sync that owns a
// database source lease. The boolean reports whether it entered the queue so
// the poller can release a lease immediately when the queue is full.
func (s *Scheduler) TriggerRecoveredWebhookSync(entityID, sourceID, provider, claimToken string) bool {
	req := syncRequest{
		EntityID:           entityID,
		SourceID:           sourceID,
		Provider:           provider,
		SyncType:           "incremental",
		RecoveryClaimToken: claimToken,
	}
	select {
	case s.queue <- req:
		s.logger.Info("persisted webhook sync enqueued",
			zap.String("entity_id", entityID),
			zap.String("source_id", sourceID),
		)
		return true
	default:
		return false
	}
}

// Run starts the dispatch loop.  It reads requests from the queue and
// launches each in its own goroutine, gated by the global and per-entity
// semaphores.  It blocks until ctx is cancelled and all in-flight jobs
// complete.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("scheduler started")
	var wg sync.WaitGroup

loop:
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopping (context cancelled)")
			break loop
		case req := <-s.queue:
			// Acquire global slot.
			select {
			case s.globalSem <- struct{}{}:
			case <-ctx.Done():
				// Put the request back if we can, otherwise drop it.
				select {
				case s.queue <- req:
				default:
				}
				s.logger.Info("scheduler stopping (context cancelled during acquire)")
				break loop
			}

			wg.Add(1)
			go func(r syncRequest) {
				defer wg.Done()
				defer func() { <-s.globalSem }()

				entitySem := s.entitySemaphore(r.EntityID)

				// Acquire per-entity slot.
				select {
				case entitySem <- struct{}{}:
				case <-ctx.Done():
					s.logger.Info("sync job skipped (context cancelled waiting for entity slot)",
						zap.String("entity_id", r.EntityID),
						zap.String("source_id", r.SourceID),
					)
					return
				}
				defer func() { <-entitySem }()

				s.executeJob(ctx, r)
			}(req)
		}
	}

	// Wait for all in-flight jobs to finish before returning.
	wg.Wait()
	s.logger.Info("scheduler stopped")
}

// executeJob runs a single sync job through the SyncRunner and sends
// the resulting audit event to the AuditProcessor.
func (s *Scheduler) executeJob(ctx context.Context, req syncRequest) {
	log := s.logger.With(
		zap.String("entity_id", req.EntityID),
		zap.String("source_id", req.SourceID),
		zap.String("provider", req.Provider),
	)

	var auditEvents []audit.Event
	run := func(operationCtx context.Context) error {
		var err error
		auditEvents, err = s.runner.Run(operationCtx, SyncJobRequest{
			EntityID:           req.EntityID,
			SourceID:           req.SourceID,
			Provider:           req.Provider,
			SyncType:           req.SyncType,
			RecoveryClaimToken: req.RecoveryClaimToken,
		})
		return err
	}
	var (
		err error
	)
	if s.limiter != nil {
		err = s.limiter.do(ctx, run)
	} else {
		err = run(ctx)
	}

	// Always emit audit events (start + finish/fail), even on error.
	for _, ev := range auditEvents {
		s.audit.WriteAsync(ev)
	}

	if err != nil {
		log.Error("sync job execution failed", zap.Error(err))
	}
}

// entitySemaphore returns (or creates) the per-entity semaphore channel.
func (s *Scheduler) entitySemaphore(entityID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	sem, ok := s.entitySemaphores[entityID]
	if !ok {
		sem = make(chan struct{}, s.maxPerEntity)
		s.entitySemaphores[entityID] = sem
	}
	return sem
}
