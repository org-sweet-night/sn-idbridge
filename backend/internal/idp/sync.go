// SPDX-License-Identifier: MIT

package idp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/audit"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/smices/open-idb/internal/id"
)

// auditWriter is the subset of audit.Service used by SyncService.
// Using an interface keeps the sync package decoupled from audit internals.
type auditWriter interface {
	Write(ctx context.Context, event audit.Event) error
}

type SyncService struct {
	queries         *generated.Queries
	provider        DirectoryProvider
	providerFactory func(ctx context.Context, entityID string, sourceID string, provider string) (DirectoryProvider, error)
	audit           auditWriter
	traceID         func() string
	txStarter       txStarter
}

type SyncServiceConfig struct {
	Queries         *generated.Queries
	Provider        DirectoryProvider
	ProviderFactory func(ctx context.Context, entityID string, sourceID string, provider string) (DirectoryProvider, error)
	Audit           auditWriter
	TraceID         func() string
	TxStarter       txStarter
}

type txStarter interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// ErrSyncAlreadyRunning prevents two reconciliation passes for the same
// entity/source boundary from observing different upstream snapshots.
var ErrSyncAlreadyRunning = errors.New("sync already running for this identity source")

// ErrSuspiciousFullSnapshot prevents a partial/truncated provider response from
// being interpreted as authoritative absence and destructively reconciled.
var ErrSuspiciousFullSnapshot = errors.New("suspicious destructive full-sync snapshot")

// SuspiciousFullSnapshotError carries the non-sensitive evidence an operator
// needs to review and explicitly approve a potentially truncated snapshot.
// It unwraps ErrSuspiciousFullSnapshot so callers can retain the stable error
// classification while exposing the fingerprint and row counts separately.
type SuspiciousFullSnapshotError struct {
	SnapshotFingerprint string
	CurrentUsers        int64
	CurrentDepartments  int64
	IncomingUsers       int64
	IncomingDepartments int64
	Reason              string
}

func (e *SuspiciousFullSnapshotError) Error() string {
	if e == nil {
		return ErrSuspiciousFullSnapshot.Error()
	}
	if e.Reason == "" {
		return ErrSuspiciousFullSnapshot.Error()
	}
	return fmt.Sprintf("%s: %s", ErrSuspiciousFullSnapshot, e.Reason)
}

func (e *SuspiciousFullSnapshotError) Unwrap() error { return ErrSuspiciousFullSnapshot }

const (
	webhookMaxAttempts      int32 = 5
	webhookClaimBatch             = 200
	webhookClaimLease             = 5 * time.Minute
	fullSyncSmallPopulation       = 20
)

type FullSyncInput struct {
	EntityID                string
	SourceID                string
	Provider                string
	SyncType                SyncMode
	RecoveryClaimToken      string
	DestructiveConfirmation *DestructiveSnapshotConfirmation
}

type FullSyncResult struct {
	JobID                 string `json:"job_id"`
	SnapshotFingerprint   string `json:"snapshot_fingerprint,omitempty"`
	DepartmentsUpserted   int    `json:"departments_upserted"`
	DepartmentsDeleted    int    `json:"departments_deleted"`
	UsersUpserted         int    `json:"users_upserted"`
	ManagedUsersCreated   int    `json:"managed_users_created"`
	ManagedUsersUpdated   int    `json:"managed_users_updated"`
	DirectoryUsersDeleted int    `json:"directory_users_deleted"`
	ManagedUsersDeleted   int    `json:"managed_users_deleted"`
	BindingsCreated       int    `json:"bindings_created"`
}

func NewSyncService(cfg SyncServiceConfig) (*SyncService, error) {
	if cfg.Queries == nil {
		return nil, fmt.Errorf("queries are required")
	}
	if cfg.Provider == nil && cfg.ProviderFactory == nil {
		return nil, fmt.Errorf("directory provider is required")
	}
	if cfg.TraceID == nil {
		cfg.TraceID = func() string { return id.NewULID() }
	}
	return &SyncService{
		queries:         cfg.Queries,
		provider:        cfg.Provider,
		providerFactory: cfg.ProviderFactory,
		audit:           cfg.Audit,
		traceID:         cfg.TraceID,
		txStarter:       cfg.TxStarter,
	}, nil
}

func (s *SyncService) SetTxStarter(starter txStarter) {
	s.txStarter = starter
}

func (s *SyncService) RunFullSync(ctx context.Context, input FullSyncInput) (FullSyncResult, error) {
	input.SyncType = SyncModeFull
	return s.runSync(ctx, input)
}

func (s *SyncService) RunIncrementalSync(ctx context.Context, input FullSyncInput) (FullSyncResult, error) {
	input.SyncType = SyncModeIncremental
	return s.runSync(ctx, input)
}

func (s *SyncService) SubmitWebhookEvent(ctx context.Context, entityID string, sourceID string, event DirectorySyncEvent) (string, error) {
	entity, err := ulidValue(entityID)
	if err != nil {
		return "", err
	}
	source, err := ulidValue(sourceID)
	if err != nil {
		return "", err
	}

	sourceRow, err := s.queries.GetIdentitySourceByID(ctx, generated.GetIdentitySourceByIDParams{
		EntityID: entity,
		ID:       source,
	})
	if err != nil {
		return "", err
	}
	event.EventID = strings.TrimSpace(event.EventID)
	if event.EventID == "" {
		event.EventID = id.NewULID()
	}
	trace, err := json.Marshal(event)
	if err != nil {
		return "", err
	}

	job, err := s.queries.CreateWebhookSyncJob(ctx, generated.CreateWebhookSyncJobParams{
		EntityID: entity,
		SourceID: source,
		Provider: sourceRow.Type,
		TraceID:  string(trace),
		EventID:  pgtype.Text{String: event.EventID, Valid: true},
	})
	if err != nil {
		return "", err
	}
	return ulidString(job.ID), nil
}

func (s *SyncService) ResolveDefaultFeishuWebhookTarget(ctx context.Context) (string, string, error) {
	entities, err := s.queries.ListEntities(ctx, generated.ListEntitiesParams{
		Limit:  1000,
		Offset: 0,
	})
	if err != nil {
		return "", "", err
	}

	var entityID string
	var sourceID string
	for _, entity := range entities {
		if entity.Status != "active" {
			continue
		}
		source, err := s.queries.GetFeishuSourceByEntity(ctx, entity.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return "", "", err
		}
		if entityID != "" {
			return "", "", fmt.Errorf("multiple active feishu webhook targets are configured")
		}
		entityID = entity.ID
		sourceID = source.ID
	}
	if entityID == "" || sourceID == "" {
		return "", "", fmt.Errorf("no active feishu identity source is configured")
	}
	return entityID, sourceID, nil
}

func (s *SyncService) runSync(ctx context.Context, input FullSyncInput) (FullSyncResult, error) {
	entityID, err := ulidValue(input.EntityID)
	if err != nil {
		return FullSyncResult{}, err
	}
	sourceID, err := ulidValue(input.SourceID)
	if err != nil {
		return FullSyncResult{}, err
	}
	if input.SyncType == SyncModeIncremental && input.RecoveryClaimToken != "" {
		// The recovery poller owns a source lease before the request reaches the
		// runner. Register its release before trying the advisory sync lock so a
		// busy source becomes immediately retryable instead of waiting for expiry.
		defer s.releaseWebhookSyncLease(entityID, sourceID, input.RecoveryClaimToken)
	}
	releaseLock, err := s.acquireSourceLock(ctx, entityID, sourceID)
	if err != nil {
		return FullSyncResult{}, err
	}
	defer releaseLock()
	provider := input.Provider
	if provider == "" {
		source, err := s.queries.GetIdentitySourceByID(ctx, generated.GetIdentitySourceByIDParams{
			EntityID: entityID,
			ID:       sourceID,
		})
		if err != nil {
			return FullSyncResult{}, err
		}
		provider = source.Type
	}

	if input.SyncType != SyncModeFull && input.SyncType != SyncModeIncremental {
		input.SyncType = SyncModeFull
	}

	var webhookJobs []generated.SyncJob
	if input.SyncType == SyncModeIncremental {
		webhookJobs, err = s.claimPendingWebhookJobs(ctx, entityID, sourceID, input.RecoveryClaimToken)
		if err != nil {
			return FullSyncResult{}, err
		}
		// A recovery request can become stale while it waits in the in-memory
		// scheduler. Do not call the provider after another replica consumed it.
		if input.RecoveryClaimToken != "" && len(webhookJobs) == 0 {
			return FullSyncResult{}, nil
		}
	}

	syncProvider, err := s.resolveProvider(ctx, input.EntityID, input.SourceID, provider)
	if err != nil {
		_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, FullSyncResult{}, err)
		return FullSyncResult{}, err
	}

	job, err := s.queries.CreateSyncJob(ctx, generated.CreateSyncJobParams{
		EntityID: entityID,
		SourceID: sourceID,
		Type:     string(input.SyncType),
		Provider: provider,
		TraceID:  s.traceID(),
	})
	if err != nil {
		_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, FullSyncResult{}, err)
		return FullSyncResult{}, err
	}
	result := FullSyncResult{JobID: ulidString(job.ID)}
	traceID := job.TraceID

	// Emit sync started event
	s.writeAudit(ctx, audit.Event{
		EntityID:     input.EntityID,
		ActorType:    "sync_job",
		Action:       audit.ActionSyncStarted,
		ResourceType: "sync_job",
		ResourceID:   result.JobID,
		After:        map[string]string{"provider": provider, "trace_id": traceID},
		TraceID:      traceID,
	})

	data, processedWebhookJobs, err := s.loadDataByMode(ctx, syncProvider, entityID, input.SyncType, webhookJobs)
	webhookJobs = processedWebhookJobs
	if err != nil {
		_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, result, err)
		_ = s.failJob(ctx, entityID, job.ID, result, err)
		s.writeAudit(ctx, audit.Event{
			EntityID:     input.EntityID,
			ActorType:    "sync_job",
			Action:       audit.ActionSyncFailed,
			ResourceType: "sync_job",
			ResourceID:   result.JobID,
			After:        map[string]string{"error": err.Error(), "trace_id": traceID},
			TraceID:      traceID,
		})
		return result, err
	}
	users, err := uniqueDirectoryUsers(data.Users)
	if err != nil {
		_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, result, err)
		_ = s.failJob(ctx, entityID, job.ID, result, err)
		return result, err
	}
	departments, err := prepareDirectoryDepartments(data.Departments)
	if err != nil {
		_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, result, err)
		_ = s.failJob(ctx, entityID, job.ID, result, err)
		return result, err
	}
	if input.SyncType == SyncModeFull {
		result.SnapshotFingerprint = fullSyncSnapshotFingerprint(users, departments)
	}

	var auditEvents []audit.Event
	if input.SyncType == SyncModeFull {
		result, auditEvents, err = s.applyPreparedFullSync(ctx, input, entityID, sourceID, job.ID, traceID, departments, users, result)
	} else {
		result, auditEvents, err = s.applyPreparedSync(ctx, input, entityID, sourceID, job.ID, traceID, departments, users, data.DepartmentDeletions, data.UserDeletions, result, false)
	}
	if err != nil {
		if input.SyncType == SyncModeIncremental {
			for _, event := range auditEvents {
				s.writeAudit(ctx, event)
			}
		}
		_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, result, err)
		_ = s.failJob(ctx, entityID, job.ID, result, err)
		return result, err
	}
	for _, event := range auditEvents {
		s.writeAudit(ctx, event)
	}

	_ = s.finishWebhookJobs(ctx, entityID, webhookJobs, result, nil)

	// Emit sync finished event
	s.writeAudit(ctx, audit.Event{
		EntityID:     input.EntityID,
		ActorType:    "sync_job",
		Action:       audit.ActionSyncFinished,
		ResourceType: "sync_job",
		ResourceID:   result.JobID,
		After:        result,
		TraceID:      traceID,
	})

	return result, nil
}

func (s *SyncService) validateFullSnapshot(ctx context.Context, entityID, sourceID string, incomingUsers, incomingDepartments int, snapshotFingerprint string, confirmation *DestructiveSnapshotConfirmation) (bool, error) {
	currentUsers, err := s.queries.CountCurrentDirectoryUsersBySource(ctx, generated.CountCurrentDirectoryUsersBySourceParams{
		EntityID: entityID, SourceID: sourceID,
	})
	if err != nil {
		return false, fmt.Errorf("count current directory users: %w", err)
	}
	currentDepartments, err := s.queries.CountCurrentDirectoryDepartmentsBySource(ctx, generated.CountCurrentDirectoryDepartmentsBySourceParams{
		EntityID: entityID, SourceID: sourceID,
	})
	if err != nil {
		return false, fmt.Errorf("count current directory departments: %w", err)
	}
	reasons := make([]string, 0, 2)
	if reason := suspiciousSnapshotShrink("users", currentUsers, int64(incomingUsers)); reason != "" {
		reasons = append(reasons, reason)
	}
	if reason := suspiciousSnapshotShrink("departments", currentDepartments, int64(incomingDepartments)); reason != "" {
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return false, nil
	}
	snapshotError := func(reason string) error {
		return &SuspiciousFullSnapshotError{
			SnapshotFingerprint: snapshotFingerprint,
			CurrentUsers:        currentUsers,
			CurrentDepartments:  currentDepartments,
			IncomingUsers:       int64(incomingUsers),
			IncomingDepartments: int64(incomingDepartments),
			Reason:              reason,
		}
	}
	if confirmation == nil {
		return false, snapshotError(strings.Join(reasons, "; "))
	}
	if strings.TrimSpace(confirmation.ConfirmedBy) == "" {
		return false, snapshotError("destructive confirmation actor is required")
	}
	if confirmation.SnapshotFingerprint != snapshotFingerprint || !strings.HasPrefix(snapshotFingerprint, "sha256:") {
		return false, snapshotError("destructive confirmation does not match the fetched snapshot")
	}
	if confirmation.CurrentUsers != currentUsers || confirmation.CurrentDepartments != currentDepartments {
		return false, snapshotError("destructive confirmation counts do not match current source rows")
	}
	return true, nil
}

func suspiciousSnapshotShrink(kind string, current, incoming int64) string {
	if current <= 0 {
		return ""
	}
	if incoming <= 0 {
		return fmt.Sprintf("%s snapshot is empty while %d current rows exist", kind, current)
	}
	if incoming >= current {
		return ""
	}
	if current < fullSyncSmallPopulation {
		return fmt.Sprintf("%s small-population snapshot shrank from %d to %d (any shrink below %d current rows is destructive)", kind, current, incoming, fullSyncSmallPopulation)
	}
	if incoming <= current/2 {
		return fmt.Sprintf("%s snapshot shrank from %d to %d (at least 50%%)", kind, current, incoming)
	}
	return ""
}

// applyPreparedFullSync opens the mutation transaction only after the provider
// snapshot has been fetched. The service copy validates current row counts on
// the same transaction connection immediately before any mutation, so an
// operator confirmation cannot authorize a stale source population.
func (s *SyncService) applyPreparedFullSync(ctx context.Context, input FullSyncInput, entityID, sourceID, jobID, traceID string, departments []DirectoryDepartment, users []DirectoryUser, result FullSyncResult) (FullSyncResult, []audit.Event, error) {
	if s.txStarter == nil {
		return result, nil, fmt.Errorf("sync service transaction starter is not configured")
	}
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return result, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	txService := *s
	txService.queries = s.queries.WithTx(tx)
	txService.txStarter = nil
	confirmed, err := txService.validateFullSnapshot(ctx, entityID, sourceID, len(users), len(departments), result.SnapshotFingerprint, input.DestructiveConfirmation)
	if err != nil {
		return result, nil, err
	}
	var confirmationAudit audit.Event
	if confirmed {
		if s.audit == nil {
			return result, nil, fmt.Errorf("destructive full sync requires a durable audit writer")
		}
		confirmationAudit = audit.Event{
			EntityID:     input.EntityID,
			ActorUserID:  input.DestructiveConfirmation.ConfirmedBy,
			ActorType:    "admin",
			Action:       audit.ActionSyncDestructiveSnapshotConfirmed,
			ResourceType: "sync_job",
			ResourceID:   result.JobID,
			After: map[string]any{
				"snapshot_fingerprint": result.SnapshotFingerprint,
				"current_users":        input.DestructiveConfirmation.CurrentUsers,
				"current_departments":  input.DestructiveConfirmation.CurrentDepartments,
				"confirmed_by":         input.DestructiveConfirmation.ConfirmedBy,
			},
			TraceID: traceID,
		}
		// The production audit.Service can share the mutation transaction. A
		// confirmation row written before commit makes the approval evidence
		// atomic with the destructive reconciliation; a failed audit insert
		// therefore rolls the mutation back.
		if _, ok := s.audit.(*audit.Service); ok {
			if err := audit.NewService(txService.queries).Write(ctx, confirmationAudit); err != nil {
				return result, nil, fmt.Errorf("persist destructive sync confirmation: %w", err)
			}
		}
	}
	result, auditEvents, err := txService.applyPreparedSync(ctx, input, entityID, sourceID, jobID, traceID, departments, users, nil, nil, result, true)
	if err != nil {
		return result, auditEvents, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, auditEvents, err
	}
	committed = true
	if confirmed {
		// Test/dedicated writers cannot participate in pgx transactions. Keep
		// their observable event behavior, while the concrete production writer
		// was already persisted atomically above.
		if _, ok := s.audit.(*audit.Service); !ok {
			auditEvents = append(auditEvents, confirmationAudit)
		}
	}
	return result, auditEvents, nil
}

func fullSyncSnapshotFingerprint(users []DirectoryUser, departments []DirectoryDepartment) string {
	canonicalUsers := append([]DirectoryUser(nil), users...)
	canonicalDepartments := append([]DirectoryDepartment(nil), departments...)
	sort.SliceStable(canonicalUsers, func(i, j int) bool {
		return canonicalUsers[i].ExternalUserID < canonicalUsers[j].ExternalUserID
	})
	sort.SliceStable(canonicalDepartments, func(i, j int) bool {
		return canonicalDepartments[i].ExternalDepartmentID < canonicalDepartments[j].ExternalDepartmentID
	})
	hasher := sha256.New()
	write := func(value []byte) {
		_, _ = hasher.Write(value)
	}
	write([]byte(`{"departments":[`))
	for index, department := range canonicalDepartments {
		if index > 0 {
			write([]byte(","))
		}
		payload, err := json.Marshal(department)
		if err != nil {
			panic("marshal full-sync department fingerprint: " + err.Error())
		}
		write(payload)
	}
	write([]byte(`],"users":[`))
	for index, user := range canonicalUsers {
		if index > 0 {
			write([]byte(","))
		}
		payload, err := json.Marshal(user)
		if err != nil {
			panic("marshal full-sync user fingerprint: " + err.Error())
		}
		write(payload)
	}
	write([]byte(`]}`))
	return fmt.Sprintf("sha256:%x", hasher.Sum(nil))
}

// applyPreparedSync writes an already fetched and validated snapshot through
// the service's current query set. Full sync callers provide transaction-bound
// queries and include the destructive reconciliation tail.
func (s *SyncService) applyPreparedSync(ctx context.Context, input FullSyncInput, entityID, sourceID, jobID, traceID string, departments []DirectoryDepartment, users []DirectoryUser, departmentDeletions, userDeletions []DirectoryObjectDeletion, result FullSyncResult, reconcile bool) (FullSyncResult, []audit.Event, error) {
	auditEvents := make([]audit.Event, 0, len(departments)+len(users))
	resolvedDepartments, err := s.resolveDirectoryDepartmentIdentities(ctx, entityID, sourceID, departments)
	if err != nil {
		return result, auditEvents, err
	}
	presentExternalDepartmentIDs := make([]string, 0, len(resolvedDepartments))
	for _, department := range resolvedDepartments {
		presentExternalDepartmentIDs = append(presentExternalDepartmentIDs, department.ExternalDepartmentID)
		if _, err := s.queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{
			EntityID:                   entityID,
			SourceID:                   sourceID,
			ExternalDepartmentID:       department.ExternalDepartmentID,
			ParentExternalDepartmentID: textValue(department.ParentExternalDepartmentID),
			Name:                       department.Name,
			RawProfile:                 department.RawProfile,
		}); err != nil {
			return result, auditEvents, err
		}
		result.DepartmentsUpserted++
		auditEvents = append(auditEvents, audit.Event{
			EntityID:     input.EntityID,
			ActorType:    "sync_job",
			Action:       audit.ActionSyncDepartmentUpdated,
			ResourceType: "directory_department",
			ResourceID:   department.ExternalDepartmentID,
			After:        department,
			TraceID:      traceID,
		})
	}

	if err := s.mapDepartments(ctx, input, entityID, sourceID, resolvedDepartments, traceID); err != nil {
		return result, auditEvents, err
	}

	presentExternalUserIDs := make([]string, 0, len(users))
	for _, user := range users {
		normalizedStatus := normalizeDirectoryStatus(user.Status)
		directoryUser, err := s.upsertMatchedDirectoryUser(ctx, entityID, sourceID, user, normalizedStatus)
		if err != nil {
			return result, auditEvents, err
		}
		presentExternalUserIDs = append(presentExternalUserIDs, directoryUser.ExternalUserID)
		result.UsersUpserted++
		if normalizedStatus == "deleted" {
			continue
		}

		binding, hasBinding, err := s.resolveAccountBinding(ctx, entityID, sourceID, directoryUser.ID, user)
		if err != nil {
			return result, auditEvents, err
		}
		if hasBinding {
			newLifecycle := lifecycleForDirectoryStatus(user.Status)
			if _, err := s.updateManagedUserFromDirectory(ctx, entityID, sourceID, binding.UserID, user, newLifecycle); err != nil {
				return result, auditEvents, err
			}
			result.ManagedUsersUpdated++
			if newLifecycle == "disabled" {
				auditEvents = append(auditEvents, audit.Event{
					EntityID:     input.EntityID,
					ActorType:    "sync_job",
					Action:       audit.ActionSyncUserDisabled,
					ResourceType: "user",
					ResourceID:   ulidString(binding.UserID),
					After:        map[string]string{"external_user_id": user.ExternalUserID, "lifecycle_status": newLifecycle},
					TraceID:      traceID,
				})
			}
			continue
		}

		mergedUser, err := s.queries.GetManagedUserByUsername(ctx, generated.GetManagedUserByUsernameParams{
			EntityID: entityID,
			Username: usernameForDirectoryUser(user),
		})
		if err == nil {
			newLifecycle := lifecycleForDirectoryStatus(user.Status)
			if _, err := s.updateManagedUserFromDirectory(ctx, entityID, sourceID, mergedUser.ID, user, newLifecycle); err != nil {
				return result, auditEvents, err
			}
			if err := s.queries.AssignRoleToUserByCode(ctx, generated.AssignRoleToUserByCodeParams{
				EntityID: entityID,
				UserID:   mergedUser.ID,
				Code:     "employee",
			}); err != nil {
				return result, auditEvents, err
			}
			result.ManagedUsersUpdated++
			if _, err := s.queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
				EntityID:        entityID,
				UserID:          mergedUser.ID,
				SourceID:        sourceID,
				DirectoryUserID: directoryUser.ID,
				ProviderUid:     user.ExternalUserID,
				ProviderUnionID: textValue(user.ExternalUnionID),
				IsPrimary:       true,
			}); err != nil {
				return result, auditEvents, err
			}
			result.BindingsCreated++
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return result, auditEvents, err
		}

		managedUser, err := s.queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
			EntityID:        entityID,
			Username:        usernameForDirectoryUser(user),
			DisplayName:     user.Name,
			EnglishName:     user.EnglishName,
			EmployeeNo:      user.EmployeeNo,
			JobTitle:        user.JobTitle,
			Email:           textValue(user.Email),
			Phone:           textValue(user.Phone),
			AvatarUrl:       textValue(user.AvatarURL),
			LifecycleStatus: lifecycleForDirectoryStatus(user.Status),
			UserType:        "employee",
			PrimarySourceID: textValue(sourceID),
			Locale:          pgtype.Text{String: "en-US", Valid: true},
		})
		if err != nil {
			return result, auditEvents, err
		}
		if err := s.queries.AssignRoleToUserByCode(ctx, generated.AssignRoleToUserByCodeParams{
			EntityID: entityID,
			UserID:   managedUser.ID,
			Code:     "employee",
		}); err != nil {
			return result, auditEvents, err
		}
		result.ManagedUsersCreated++

		if _, err := s.queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
			EntityID:        entityID,
			UserID:          managedUser.ID,
			SourceID:        sourceID,
			DirectoryUserID: directoryUser.ID,
			ProviderUid:     user.ExternalUserID,
			ProviderUnionID: textValue(user.ExternalUnionID),
			IsPrimary:       true,
		}); err != nil {
			return result, auditEvents, err
		}
		result.BindingsCreated++
		auditEvents = append(auditEvents, audit.Event{
			EntityID:     input.EntityID,
			ActorType:    "sync_job",
			Action:       audit.ActionSyncUserCreated,
			ResourceType: "user",
			ResourceID:   ulidString(managedUser.ID),
			After: map[string]string{
				"external_user_id": user.ExternalUserID,
				"username":         managedUser.Username,
				"lifecycle_status": managedUser.LifecycleStatus,
			},
			TraceID: traceID,
		})
	}

	if !reconcile && (len(departmentDeletions) > 0 || len(userDeletions) > 0) {
		updatedResult, deletionAuditEvents, err := s.applyIncrementalDeletions(ctx, input, entityID, sourceID, traceID, departmentDeletions, userDeletions, result)
		if err != nil {
			return result, auditEvents, err
		}
		result = updatedResult
		auditEvents = append(auditEvents, deletionAuditEvents...)
	}

	if reconcile {
		archivedUsers, deletedDirectoryUsers, deletedDepartments, err := reconcileFullSyncWithQueries(ctx, s.queries, entityID, sourceID, presentExternalUserIDs, presentExternalDepartmentIDs)
		if err != nil {
			return result, auditEvents, err
		}
		result.DirectoryUsersDeleted = deletedDirectoryUsers
		result.DepartmentsDeleted = deletedDepartments
		result.ManagedUsersDeleted = len(archivedUsers)
		for _, archivedUser := range archivedUsers {
			auditEvents = append(auditEvents, audit.Event{
				EntityID:     input.EntityID,
				ActorType:    "sync_job",
				Action:       audit.ActionSyncUserArchived,
				ResourceType: "archived_user",
				ResourceID:   ulidString(archivedUser.Archive.ID),
				After:        map[string]string{"username": archivedUser.Username, "archive_reason": "directory full sync removed user"},
				TraceID:      traceID,
			})
		}
	}

	if _, err := s.queries.FinishSyncJob(ctx, generated.FinishSyncJobParams{
		EntityID: entityID,
		ID:       jobID,
		Stats:    mustStatsJSON(result),
	}); err != nil {
		return result, auditEvents, err
	}
	return result, auditEvents, nil
}

func (s *SyncService) upsertMatchedDirectoryUser(ctx context.Context, entityID, sourceID string, user DirectoryUser, normalizedStatus string) (generated.DirectoryUser, error) {
	existing, found, err := findDirectoryUserByKnownIdentifiers(ctx, s.queries, entityID, sourceID, user)
	if err != nil {
		return generated.DirectoryUser{}, err
	}
	if found {
		// Preserve alternate identifiers when a partial incremental payload omits
		// them; losing one would make a later delete webhook impossible to match.
		if strings.TrimSpace(user.ExternalUnionID) == "" && existing.ExternalUnionID.Valid {
			user.ExternalUnionID = existing.ExternalUnionID.String
		}
		if strings.TrimSpace(user.ExternalOpenID) == "" && existing.ExternalOpenID.Valid {
			user.ExternalOpenID = existing.ExternalOpenID.String
		}
		return s.queries.UpdateDirectoryUserByID(ctx, generated.UpdateDirectoryUserByIDParams{
			EntityID:        entityID,
			SourceID:        sourceID,
			ID:              existing.ID,
			ExternalUserID:  user.ExternalUserID,
			ExternalUnionID: textValue(user.ExternalUnionID),
			ExternalOpenID:  textValue(user.ExternalOpenID),
			Name:            user.Name,
			EnglishName:     user.EnglishName,
			EmployeeNo:      user.EmployeeNo,
			JobTitle:        user.JobTitle,
			Email:           textValue(user.Email),
			Phone:           textValue(user.Phone),
			AvatarUrl:       textValue(user.AvatarURL),
			Status:          normalizedStatus,
			RawProfile:      user.RawProfile,
		})
	}
	return s.queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID:        entityID,
		SourceID:        sourceID,
		ExternalUserID:  user.ExternalUserID,
		ExternalUnionID: textValue(user.ExternalUnionID),
		ExternalOpenID:  textValue(user.ExternalOpenID),
		Name:            user.Name,
		EnglishName:     user.EnglishName,
		EmployeeNo:      user.EmployeeNo,
		JobTitle:        user.JobTitle,
		Email:           textValue(user.Email),
		Phone:           textValue(user.Phone),
		AvatarUrl:       textValue(user.AvatarURL),
		Status:          normalizedStatus,
		RawProfile:      user.RawProfile,
	})
}

func findDirectoryUserByKnownIdentifiers(ctx context.Context, queries *generated.Queries, entityID, sourceID string, user DirectoryUser) (generated.DirectoryUser, bool, error) {
	identifiers := []DirectoryObjectDeletion{
		{ObjectID: user.ExternalUserID, ObjectIDType: "user_id"},
		{ObjectID: user.ExternalOpenID, ObjectIDType: "open_id"},
		{ObjectID: user.ExternalUnionID, ObjectIDType: "union_id"},
	}
	seen := make(map[string]struct{}, len(identifiers))
	var matched generated.DirectoryUser
	found := false
	for _, identifier := range identifiers {
		identifier.ObjectID = strings.TrimSpace(identifier.ObjectID)
		if identifier.ObjectID == "" {
			continue
		}
		key := identifier.ObjectIDType + "\x00" + identifier.ObjectID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		rows, err := queries.ListDirectoryUsersByProviderIdentifier(ctx, generated.ListDirectoryUsersByProviderIdentifierParams{
			EntityID:       entityID,
			SourceID:       sourceID,
			Identifier:     identifier.ObjectID,
			IdentifierType: identifier.ObjectIDType,
		})
		if err != nil {
			return generated.DirectoryUser{}, false, err
		}
		if identifier.ObjectIDType == "user_id" {
			for _, row := range rows {
				if row.ExternalUserID == identifier.ObjectID {
					// A full Feishu snapshot uses user_id as its canonical identifier.
					// Prefer its exact row so reconciliation can retire stale rows that
					// only match an old open_id or union_id.
					return row, true, nil
				}
			}
		}
		for _, row := range rows {
			if !found {
				matched = row
				found = true
				continue
			}
			if row.ID != matched.ID {
				return generated.DirectoryUser{}, false, fmt.Errorf("directory user provider identifiers resolve to multiple existing rows")
			}
		}
	}
	return matched, found, nil
}

func (s *SyncService) resolveDirectoryDepartmentIdentities(ctx context.Context, entityID, sourceID string, departments []DirectoryDepartment) ([]DirectoryDepartment, error) {
	resolved := make([]DirectoryDepartment, len(departments))
	aliasToCanonical := make(map[string]string, len(departments)*3)
	for index, department := range departments {
		matched, err := s.preserveDirectoryDepartmentIdentity(ctx, entityID, sourceID, department)
		if err != nil {
			return nil, err
		}
		resolved[index] = matched
		for _, alias := range departmentLookupKeys(department) {
			aliasToCanonical[alias] = matched.ExternalDepartmentID
		}
		aliasToCanonical[matched.ExternalDepartmentID] = matched.ExternalDepartmentID
	}
	for index := range resolved {
		parentID := strings.TrimSpace(resolved[index].ParentExternalDepartmentID)
		if parentID == "" || parentID == "0" {
			continue
		}
		if canonical, ok := aliasToCanonical[parentID]; ok {
			resolved[index].ParentExternalDepartmentID = canonical
			continue
		}
		// Incremental batches commonly contain only the child. Resolve its parent
		// against the stored dual-ID raw profile before falling back to the alias.
		parents, err := s.queries.ListDirectoryDepartmentsByProviderIdentifier(ctx, generated.ListDirectoryDepartmentsByProviderIdentifierParams{
			EntityID: entityID, SourceID: sourceID, Identifier: parentID,
		})
		if err != nil {
			return nil, err
		}
		if len(parents) > 1 {
			return nil, fmt.Errorf("directory parent identifier resolves to multiple existing rows")
		}
		if len(parents) == 1 {
			resolved[index].ParentExternalDepartmentID = parents[0].ExternalDepartmentID
		}
	}
	return resolved, nil
}

func (s *SyncService) preserveDirectoryDepartmentIdentity(ctx context.Context, entityID, sourceID string, department DirectoryDepartment) (DirectoryDepartment, error) {
	departmentID, openDepartmentID := directoryDepartmentProviderIDs(department)
	identifiers := []DirectoryObjectDeletion{
		{ObjectID: openDepartmentID, ObjectIDType: "open_department_id"},
		{ObjectID: departmentID, ObjectIDType: "department_id"},
		{ObjectID: department.ExternalDepartmentID},
	}
	seen := make(map[string]struct{}, len(identifiers))
	var matched generated.DirectoryDepartment
	found := false
	for _, identifier := range identifiers {
		identifier.ObjectID = strings.TrimSpace(identifier.ObjectID)
		if identifier.ObjectID == "" {
			continue
		}
		key := identifier.ObjectIDType + "\x00" + identifier.ObjectID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		rows, err := s.queries.ListDirectoryDepartmentsByProviderIdentifier(ctx, generated.ListDirectoryDepartmentsByProviderIdentifierParams{
			EntityID:       entityID,
			SourceID:       sourceID,
			Identifier:     identifier.ObjectID,
			IdentifierType: identifier.ObjectIDType,
		})
		if err != nil {
			return DirectoryDepartment{}, err
		}
		for _, row := range rows {
			if !found {
				matched = row
				found = true
				continue
			}
			if row.ID != matched.ID {
				return DirectoryDepartment{}, fmt.Errorf("directory department provider identifiers resolve to multiple existing rows")
			}
		}
	}
	if found {
		// Keep the production canonical key (and therefore both directory and
		// organization ULIDs) even when the provider starts preferring another ID.
		department.ExternalDepartmentID = matched.ExternalDepartmentID
	}
	return department, nil
}

func directoryDepartmentProviderIDs(department DirectoryDepartment) (string, string) {
	var raw struct {
		DepartmentID     string `json:"department_id"`
		OpenDepartmentID string `json:"open_department_id"`
	}
	if len(department.RawProfile) > 0 {
		_ = json.Unmarshal(department.RawProfile, &raw)
	}
	return strings.TrimSpace(raw.DepartmentID), strings.TrimSpace(raw.OpenDepartmentID)
}

func validDirectoryDeletionIdentifier(objectType string, deletion DirectoryObjectDeletion) bool {
	if strings.TrimSpace(deletion.ObjectID) == "" {
		return false
	}
	identifierType := strings.ToLower(strings.TrimSpace(deletion.ObjectIDType))
	switch strings.ToLower(strings.TrimSpace(objectType)) {
	case "user":
		return identifierType == "user_id" || identifierType == "open_id" || identifierType == "union_id"
	case "department":
		return identifierType == "department_id" || identifierType == "open_department_id"
	default:
		return false
	}
}

func (s *SyncService) applyIncrementalDeletions(ctx context.Context, input FullSyncInput, entityID, sourceID, traceID string, departmentDeletions, userDeletions []DirectoryObjectDeletion, result FullSyncResult) (FullSyncResult, []audit.Event, error) {
	if s.txStarter == nil {
		return result, nil, fmt.Errorf("sync service transaction starter is not configured")
	}
	baseResult := result
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return baseResult, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	queries := s.queries.WithTx(tx)
	auditEvents := make([]audit.Event, 0, len(userDeletions))

	for _, deletion := range departmentDeletions {
		if !validDirectoryDeletionIdentifier("department", deletion) {
			continue
		}
		identifier := strings.TrimSpace(deletion.ObjectID)
		departments, err := queries.ListDirectoryDepartmentsByProviderIdentifier(ctx, generated.ListDirectoryDepartmentsByProviderIdentifierParams{
			EntityID:       entityID,
			SourceID:       sourceID,
			Identifier:     identifier,
			IdentifierType: strings.ToLower(strings.TrimSpace(deletion.ObjectIDType)),
		})
		if err != nil {
			return baseResult, nil, err
		}
		if len(departments) == 0 {
			continue
		}
		if len(departments) > 1 {
			return baseResult, nil, fmt.Errorf("department deletion identifier resolves to multiple existing rows")
		}
		department := departments[0]
		if _, err := queries.DeleteSourceDepartmentByExternalID(ctx, generated.DeleteSourceDepartmentByExternalIDParams{
			EntityID:             entityID,
			SourceID:             textValue(sourceID),
			ExternalDepartmentID: textValue(department.ExternalDepartmentID),
		}); err != nil {
			return baseResult, nil, err
		}
		if _, err := queries.DeleteDirectoryDepartmentByID(ctx, generated.DeleteDirectoryDepartmentByIDParams{
			EntityID: entityID,
			SourceID: sourceID,
			ID:       department.ID,
		}); err != nil {
			return baseResult, nil, err
		}
		result.DepartmentsDeleted++
	}

	for _, deletion := range userDeletions {
		if !validDirectoryDeletionIdentifier("user", deletion) {
			continue
		}
		identifier := strings.TrimSpace(deletion.ObjectID)
		directoryUsers, err := queries.ListDirectoryUsersByProviderIdentifier(ctx, generated.ListDirectoryUsersByProviderIdentifierParams{
			EntityID:       entityID,
			SourceID:       sourceID,
			Identifier:     identifier,
			IdentifierType: strings.ToLower(strings.TrimSpace(deletion.ObjectIDType)),
		})
		if err != nil {
			return baseResult, nil, err
		}
		if len(directoryUsers) == 0 {
			continue
		}
		if len(directoryUsers) > 1 {
			return baseResult, nil, fmt.Errorf("user deletion identifier resolves to multiple existing rows")
		}
		directoryUser := directoryUsers[0]
		wasDeleted := directoryUser.Status == "deleted"
		if _, err := queries.MarkDirectoryUserDeletedByID(ctx, generated.MarkDirectoryUserDeletedByIDParams{
			EntityID: entityID,
			SourceID: sourceID,
			ID:       directoryUser.ID,
		}); err != nil {
			return baseResult, nil, err
		}
		if !wasDeleted {
			result.DirectoryUsersDeleted++
		}

		if _, err := queries.GetAccountBindingByDirectoryUserID(ctx, generated.GetAccountBindingByDirectoryUserIDParams{
			EntityID: entityID, SourceID: sourceID, DirectoryUserID: directoryUser.ID,
		}); errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return baseResult, nil, err
		}
		managedUser, err := queries.GetManagedUserForDeletedDirectoryUser(ctx, generated.GetManagedUserForDeletedDirectoryUserParams{
			EntityID: entityID, SourceID: sourceID, DirectoryUserID: directoryUser.ID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return baseResult, nil, err
		}
		archive, err := queries.ArchiveUser(ctx, generated.ArchiveUserParams{
			EntityID: entityID, UserID: managedUser.ID, ArchivedByUserID: pgtype.Text{}, ArchiveReason: "directory incremental sync deleted user",
		})
		if err != nil {
			return baseResult, nil, err
		}
		if err := queries.DeleteUserActiveDependents(ctx, generated.DeleteUserActiveDependentsParams{EntityID: entityID, UserID: managedUser.ID}); err != nil {
			return baseResult, nil, err
		}
		if err := queries.DeleteUserActiveRow(ctx, generated.DeleteUserActiveRowParams{EntityID: entityID, UserID: managedUser.ID}); err != nil {
			return baseResult, nil, err
		}
		result.ManagedUsersDeleted++
		auditEvents = append(auditEvents, audit.Event{
			EntityID: input.EntityID, ActorType: "sync_job", Action: audit.ActionSyncUserArchived,
			ResourceType: "archived_user", ResourceID: ulidString(archive.ID),
			After: map[string]string{"username": managedUser.Username, "archive_reason": "directory incremental sync deleted user"}, TraceID: traceID,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return baseResult, nil, err
	}
	committed = true
	return result, auditEvents, nil
}

func (s *SyncService) resolveAccountBinding(ctx context.Context, entityID, sourceID, directoryUserID string, user DirectoryUser) (generated.AccountBinding, bool, error) {
	// The directory row is the stable bridge when a provider upgrades its
	// canonical identifier (for example open_id -> user_id).
	binding, err := s.queries.GetAccountBindingByDirectoryUserID(ctx, generated.GetAccountBindingByDirectoryUserIDParams{
		EntityID:        entityID,
		SourceID:        sourceID,
		DirectoryUserID: directoryUserID,
	})
	if err == nil {
		return s.ensureAccountBindingMatchesDirectory(ctx, entityID, sourceID, directoryUserID, binding, user)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return generated.AccountBinding{}, false, err
	}

	binding, err = s.queries.GetAccountBindingByProviderUID(ctx, generated.GetAccountBindingByProviderUIDParams{
		EntityID:    entityID,
		SourceID:    sourceID,
		ProviderUid: user.ExternalUserID,
	})
	if err == nil {
		return s.ensureAccountBindingMatchesDirectory(ctx, entityID, sourceID, directoryUserID, binding, user)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return generated.AccountBinding{}, false, err
	}

	if strings.TrimSpace(user.ExternalUnionID) == "" {
		return generated.AccountBinding{}, false, nil
	}
	bindings, err := s.queries.ListAccountBindingsByProviderUnionID(ctx, generated.ListAccountBindingsByProviderUnionIDParams{
		EntityID:        entityID,
		SourceID:        sourceID,
		ProviderUnionID: textValue(user.ExternalUnionID),
	})
	if err != nil {
		return generated.AccountBinding{}, false, err
	}
	if len(bindings) == 0 {
		return generated.AccountBinding{}, false, nil
	}
	if len(bindings) > 1 {
		return generated.AccountBinding{}, false, fmt.Errorf("provider union_id resolves to multiple account bindings")
	}
	return s.ensureAccountBindingMatchesDirectory(ctx, entityID, sourceID, directoryUserID, bindings[0], user)
}

func (s *SyncService) ensureAccountBindingMatchesDirectory(ctx context.Context, entityID, sourceID, directoryUserID string, binding generated.AccountBinding, user DirectoryUser) (generated.AccountBinding, bool, error) {
	if binding.DirectoryUserID == directoryUserID &&
		binding.ProviderUid == user.ExternalUserID &&
		textMatches(binding.ProviderUnionID, user.ExternalUnionID) {
		return binding, true, nil
	}
	updated, err := s.queries.UpdateAccountBindingFromDirectory(ctx, generated.UpdateAccountBindingFromDirectoryParams{
		EntityID:        entityID,
		SourceID:        sourceID,
		ID:              binding.ID,
		DirectoryUserID: directoryUserID,
		ProviderUid:     user.ExternalUserID,
		ProviderUnionID: textValue(user.ExternalUnionID),
	})
	if err != nil {
		return generated.AccountBinding{}, false, err
	}
	return updated, true, nil
}

func (s *SyncService) updateManagedUserFromDirectory(ctx context.Context, entityID, sourceID, userID string, user DirectoryUser, lifecycle string) (generated.User, error) {
	return s.queries.UpdateManagedUserFromDirectory(ctx, generated.UpdateManagedUserFromDirectoryParams{
		EntityID:        entityID,
		ID:              userID,
		PrimarySourceID: textValue(sourceID),
		Username:        textValue(usernameForDirectoryUser(user)),
		DisplayName:     user.Name,
		EnglishName:     user.EnglishName,
		EmployeeNo:      user.EmployeeNo,
		JobTitle:        user.JobTitle,
		Email:           textValue(user.Email),
		Phone:           textValue(user.Phone),
		AvatarUrl:       textValue(user.AvatarURL),
		LifecycleStatus: lifecycle,
	})
}

func (s *SyncService) archiveManagedUser(ctx context.Context, entityID, userID, reason string) (generated.ArchivedUser, error) {
	if s.txStarter == nil {
		return generated.ArchivedUser{}, fmt.Errorf("sync service transaction starter is not configured")
	}
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return generated.ArchivedUser{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	txQueries := s.queries.WithTx(tx)
	archive, err := txQueries.ArchiveUser(ctx, generated.ArchiveUserParams{
		EntityID:         entityID,
		UserID:           userID,
		ArchivedByUserID: pgtype.Text{},
		ArchiveReason:    reason,
	})
	if err != nil {
		return generated.ArchivedUser{}, err
	}
	if err := txQueries.DeleteUserActiveDependents(ctx, generated.DeleteUserActiveDependentsParams{
		EntityID: entityID,
		UserID:   userID,
	}); err != nil {
		return generated.ArchivedUser{}, err
	}
	if err := txQueries.DeleteUserActiveRow(ctx, generated.DeleteUserActiveRowParams{
		EntityID: entityID,
		UserID:   userID,
	}); err != nil {
		return generated.ArchivedUser{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return generated.ArchivedUser{}, err
	}
	committed = true
	return archive, nil
}

type archivedSyncUser struct {
	Username string
	Archive  generated.ArchivedUser
}

// reconcileFullSyncWithQueries is transaction-agnostic so full snapshot
// application can include reconciliation in its outer transaction.
func reconcileFullSyncWithQueries(ctx context.Context, queries *generated.Queries, entityID, sourceID string, presentExternalUserIDs, presentExternalDepartmentIDs []string) ([]archivedSyncUser, int, int, error) {
	deletedDepartments, err := queries.DeleteMissingSourceDepartments(ctx, generated.DeleteMissingSourceDepartmentsParams{
		EntityID: entityID, SourceID: pgtype.Text{String: sourceID, Valid: true}, PresentExternalDepartmentIds: presentExternalDepartmentIDs,
	})
	if err != nil {
		return nil, 0, 0, err
	}
	if _, err := queries.DeleteMissingDirectoryDepartments(ctx, generated.DeleteMissingDirectoryDepartmentsParams{
		EntityID: entityID, SourceID: sourceID, PresentExternalDepartmentIds: presentExternalDepartmentIDs,
	}); err != nil {
		return nil, 0, 0, err
	}
	deletedDirectoryUsers, err := queries.MarkMissingDirectoryUsersDeleted(ctx, generated.MarkMissingDirectoryUsersDeletedParams{
		EntityID:               entityID,
		SourceID:               sourceID,
		PresentExternalUserIds: presentExternalUserIDs,
	})
	if err != nil {
		return nil, 0, 0, err
	}
	deletedManagedUsers, err := queries.ListManagedUsersForDeletedDirectoryUsers(ctx, generated.ListManagedUsersForDeletedDirectoryUsersParams{EntityID: entityID, SourceID: sourceID})
	if err != nil {
		return nil, 0, 0, err
	}
	archivedUsers := make([]archivedSyncUser, 0, len(deletedManagedUsers))
	for _, user := range deletedManagedUsers {
		archive, err := queries.ArchiveUser(ctx, generated.ArchiveUserParams{
			EntityID: entityID, UserID: user.ID, ArchivedByUserID: pgtype.Text{}, ArchiveReason: "directory full sync removed user",
		})
		if err != nil {
			return nil, 0, 0, err
		}
		if err := queries.DeleteUserActiveDependents(ctx, generated.DeleteUserActiveDependentsParams{EntityID: entityID, UserID: user.ID}); err != nil {
			return nil, 0, 0, err
		}
		if err := queries.DeleteUserActiveRow(ctx, generated.DeleteUserActiveRowParams{EntityID: entityID, UserID: user.ID}); err != nil {
			return nil, 0, 0, err
		}
		archivedUsers = append(archivedUsers, archivedSyncUser{Username: user.Username, Archive: archive})
	}
	return archivedUsers, len(deletedDirectoryUsers), int(deletedDepartments), nil
}

// acquireSourceLock serializes sync runs for one identity source across all
// backend replicas. The lock is tied to a short, otherwise idle transaction;
// PostgreSQL releases it automatically if the process crashes.
func (s *SyncService) acquireSourceLock(ctx context.Context, entityID, sourceID string) (func(), error) {
	if s.txStarter == nil {
		// Preserve lightweight unit callers. The production app always sets the
		// PostgreSQL transaction starter before accepting sync requests.
		return func() {}, nil
	}
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	var acquired bool
	err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1::text || chr(31) || $2::text, 0))", entityID, sourceID).Scan(&acquired)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	if !acquired {
		_ = tx.Rollback(context.Background())
		return nil, ErrSyncAlreadyRunning
	}
	return func() {
		_ = tx.Commit(context.Background())
	}, nil
}

func uniqueDirectoryUsers(users []DirectoryUser) ([]DirectoryUser, error) {
	result := make([]DirectoryUser, 0, len(users))
	seen := make(map[string]int, len(users))
	identifierOwners := make(map[string]string, len(users)*3)
	for _, user := range users {
		key := strings.TrimSpace(user.ExternalUserID)
		if key == "" {
			return nil, fmt.Errorf("directory user external id is required")
		}
		user.ExternalUserID = key
		user.ExternalOpenID = strings.TrimSpace(user.ExternalOpenID)
		user.ExternalUnionID = strings.TrimSpace(user.ExternalUnionID)
		for _, identifier := range []struct {
			name  string
			value string
		}{{"user_id", key}, {"open_id", user.ExternalOpenID}, {"union_id", user.ExternalUnionID}} {
			if identifier.value == "" {
				continue
			}
			if owner := identifierOwners[identifier.value]; owner != "" && owner != key {
				return nil, fmt.Errorf("directory user %s %q belongs to multiple users", identifier.name, identifier.value)
			}
		}
		if index, ok := seen[key]; ok {
			existing := result[index]
			if existing.ExternalOpenID != "" && user.ExternalOpenID != "" && existing.ExternalOpenID != user.ExternalOpenID {
				return nil, fmt.Errorf("duplicate directory user %q has conflicting open_id values", key)
			}
			if existing.ExternalUnionID != "" && user.ExternalUnionID != "" && existing.ExternalUnionID != user.ExternalUnionID {
				return nil, fmt.Errorf("duplicate directory user %q has conflicting union_id values", key)
			}
			result[index] = mergeDirectoryUser(result[index], user)
			identifierOwners[key] = key
			if user.ExternalOpenID != "" {
				identifierOwners[user.ExternalOpenID] = key
			}
			if user.ExternalUnionID != "" {
				identifierOwners[user.ExternalUnionID] = key
			}
			continue
		}
		identifierOwners[key] = key
		if user.ExternalOpenID != "" {
			identifierOwners[user.ExternalOpenID] = key
		}
		if user.ExternalUnionID != "" {
			identifierOwners[user.ExternalUnionID] = key
		}
		seen[key] = len(result)
		result = append(result, user)
	}
	return result, nil
}

func prepareDirectoryDepartments(departments []DirectoryDepartment) ([]DirectoryDepartment, error) {
	prepared := make([]DirectoryDepartment, len(departments))
	type providerIDs struct {
		departmentID     string
		openDepartmentID string
	}
	idsByExternal := make(map[string]providerIDs, len(departments))
	identifierOwners := make(map[string]string, len(departments)*3)
	for index, department := range departments {
		department.ExternalDepartmentID = strings.TrimSpace(department.ExternalDepartmentID)
		department.ParentExternalDepartmentID = strings.TrimSpace(department.ParentExternalDepartmentID)
		if department.ExternalDepartmentID == "" {
			return nil, fmt.Errorf("directory department external id is required")
		}
		departmentID, openDepartmentID := directoryDepartmentProviderIDs(department)
		if existing, ok := idsByExternal[department.ExternalDepartmentID]; ok {
			if existing.departmentID != "" && departmentID != "" && existing.departmentID != departmentID {
				return nil, fmt.Errorf("duplicate directory department %q has conflicting department_id values", department.ExternalDepartmentID)
			}
			if existing.openDepartmentID != "" && openDepartmentID != "" && existing.openDepartmentID != openDepartmentID {
				return nil, fmt.Errorf("duplicate directory department %q has conflicting open_department_id values", department.ExternalDepartmentID)
			}
			if existing.departmentID == "" {
				existing.departmentID = departmentID
			}
			if existing.openDepartmentID == "" {
				existing.openDepartmentID = openDepartmentID
			}
			idsByExternal[department.ExternalDepartmentID] = existing
		} else {
			idsByExternal[department.ExternalDepartmentID] = providerIDs{departmentID: departmentID, openDepartmentID: openDepartmentID}
		}
		for _, identifier := range []struct {
			name  string
			value string
		}{{"external_department_id", department.ExternalDepartmentID}, {"department_id", departmentID}, {"open_department_id", openDepartmentID}} {
			if identifier.value == "" {
				continue
			}
			if owner := identifierOwners[identifier.value]; owner != "" && owner != department.ExternalDepartmentID {
				return nil, fmt.Errorf("directory department %s %q belongs to multiple departments", identifier.name, identifier.value)
			}
			identifierOwners[identifier.value] = department.ExternalDepartmentID
		}
		prepared[index] = department
	}
	return prepared, nil
}

func mergeDirectoryUser(existing, next DirectoryUser) DirectoryUser {
	if existing.ExternalUnionID == "" {
		existing.ExternalUnionID = next.ExternalUnionID
	}
	if existing.ExternalOpenID == "" {
		existing.ExternalOpenID = next.ExternalOpenID
	}
	if existing.Name == "" {
		existing.Name = next.Name
	}
	if existing.EnglishName == "" {
		existing.EnglishName = next.EnglishName
	}
	if existing.EmployeeNo == "" {
		existing.EmployeeNo = next.EmployeeNo
	}
	if existing.JobTitle == "" {
		existing.JobTitle = next.JobTitle
	}
	if existing.Email == "" {
		existing.Email = next.Email
	}
	if existing.Phone == "" {
		existing.Phone = next.Phone
	}
	if existing.AvatarURL == "" {
		existing.AvatarURL = next.AvatarURL
	}
	if existing.Status == "" || existing.Status == "unknown" {
		existing.Status = next.Status
	}
	if len(existing.RawProfile) == 0 || string(existing.RawProfile) == "{}" {
		existing.RawProfile = next.RawProfile
	}
	return existing
}

func (s *SyncService) loadDataByMode(ctx context.Context, provider DirectoryProvider, entityID string, mode SyncMode, webhookJobs []generated.SyncJob) (FullSyncData, []generated.SyncJob, error) {
	if mode == SyncModeIncremental {
		type incremental interface {
			IncrementalSync(context.Context, []DirectorySyncEvent) (FullSyncData, error)
		}
		if p, ok := provider.(incremental); ok {
			events := make([]DirectorySyncEvent, 0, len(webhookJobs))
			remainingJobs := make([]generated.SyncJob, 0, len(webhookJobs))
			for _, job := range webhookJobs {
				var event DirectorySyncEvent
				if err := json.Unmarshal([]byte(job.TraceID), &event); err != nil {
					_ = s.failWebhookJob(ctx, entityID, job.ID, FullSyncResult{}, err)
					continue
				}
				events = append(events, event)
				remainingJobs = append(remainingJobs, job)
			}
			if len(events) == 0 {
				return FullSyncData{}, remainingJobs, nil
			}
			data, err := p.IncrementalSync(ctx, events)
			return data, remainingJobs, err
		}
	}
	data, err := provider.FullSync(ctx)
	return data, webhookJobs, err
}

func (s *SyncService) claimPendingWebhookJobs(ctx context.Context, entityID, sourceID, claimToken string) ([]generated.SyncJob, error) {
	return s.queries.ClaimDueWebhookJobsBySource(ctx, generated.ClaimDueWebhookJobsBySourceParams{
		LeaseSeconds: int32(webhookClaimLease / time.Second),
		EntityID:     entityID,
		SourceID:     sourceID,
		ClaimToken:   claimToken,
		BatchSize:    webhookClaimBatch,
	})
}

func (s *SyncService) releaseWebhookSyncLease(entityID, sourceID, claimToken string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.queries.ReleaseWebhookSyncLease(ctx, generated.ReleaseWebhookSyncLeaseParams{
		EntityID: entityID, SourceID: sourceID, ClaimToken: claimToken,
	})
}

const syncCleanupTimeout = 5 * time.Second

// cleanupContext keeps terminal job state writable even when the sync's
// operation context was canceled by its deadline or by a provider failure.
// Values are preserved for tracing, but cancellation is detached and bounded
// so cleanup cannot keep a database connection indefinitely.
func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		return context.WithTimeout(context.Background(), syncCleanupTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(parent), syncCleanupTimeout)
}

func (s *SyncService) finishWebhookJobs(ctx context.Context, entityID string, webhookJobs []generated.SyncJob, result FullSyncResult, cause error) error {
	if len(webhookJobs) == 0 {
		return nil
	}
	for _, job := range webhookJobs {
		var finishErr error
		if cause == nil {
			finishErr = s.finishWebhookJob(ctx, entityID, job.ID, result)
		} else if job.AttemptCount < webhookMaxAttempts {
			finishErr = s.retryWebhookJob(ctx, entityID, job, result, cause)
		} else {
			finishErr = s.failWebhookJob(ctx, entityID, job.ID, result, cause)
		}
		if finishErr != nil {
			// Best-effort for all webhook jobs.
			continue
		}
	}
	return nil
}

func (s *SyncService) retryWebhookJob(ctx context.Context, entityID string, job generated.SyncJob, result FullSyncResult, cause error) error {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	delay := webhookRetryDelay(job.AttemptCount)
	_, err := s.queries.RescheduleWebhookSyncJob(cleanupCtx, generated.RescheduleWebhookSyncJobParams{
		ErrorMessage: pgtype.Text{String: cause.Error(), Valid: true},
		Stats:        mustStatsJSON(result),
		DelaySeconds: int32(delay / time.Second),
		EntityID:     entityID,
		ID:           job.ID,
	})
	return err
}

func webhookRetryDelay(attempt int32) time.Duration {
	switch attempt {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 30 * time.Minute
	default:
		return 2 * time.Hour
	}
}

func (s *SyncService) finishWebhookJob(ctx context.Context, entityID string, jobID string, result FullSyncResult) error {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	_, err := s.queries.FinishSyncJob(cleanupCtx, generated.FinishSyncJobParams{
		EntityID: entityID,
		ID:       jobID,
		Stats:    mustStatsJSON(result),
	})
	return err
}

func (s *SyncService) failWebhookJob(ctx context.Context, entityID string, jobID string, result FullSyncResult, cause error) error {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	_, err := s.queries.FailSyncJob(cleanupCtx, generated.FailSyncJobParams{
		EntityID:     entityID,
		ID:           jobID,
		ErrorMessage: pgtype.Text{String: cause.Error(), Valid: true},
		Stats:        mustStatsJSON(result),
	})
	return err
}

func (s *SyncService) resolveProvider(ctx context.Context, entityID string, sourceID string, provider string) (DirectoryProvider, error) {
	if provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	switch strings.TrimSpace(provider) {
	case "":
		return nil, fmt.Errorf("provider is required")
	case "feishu", "dingtalk", "wecom", "ldap", "local":
		if s.providerFactory != nil {
			return s.providerFactory(ctx, entityID, sourceID, provider)
		}
		if s.provider == nil {
			return nil, fmt.Errorf("directory provider is not configured")
		}
		return s.provider, nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}
}

// writeAudit emits an audit event if the audit writer is configured.
// Audit write failures are logged but do not fail the sync operation
// (sync runs are long-running and partial progress should be preserved).
func (s *SyncService) writeAudit(ctx context.Context, event audit.Event) {
	if s.audit == nil {
		return
	}
	// Best-effort: sync audit writes should not block sync progress
	_ = s.audit.Write(ctx, event)
}

func (s *SyncService) failJob(ctx context.Context, entityID string, jobID string, result FullSyncResult, cause error) error {
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	_, err := s.queries.FailSyncJob(cleanupCtx, generated.FailSyncJobParams{
		EntityID:     entityID,
		ID:           jobID,
		ErrorMessage: pgtype.Text{String: cause.Error(), Valid: true},
		Stats:        mustStatsJSON(result),
	})
	return err
}

// mapDepartments maps directory_departments to organizational departments.
// It finds or creates a default organization for the entity, then upserts
// department records linked to that organization. Parent relationships are
// resolved in two passes: first upsert all departments, then update parent_id.
// Errors fail the sync job so a successful full sync always exposes the same
// source-backed department set through both directory and organization views.
func (s *SyncService) mapDepartments(ctx context.Context, input FullSyncInput, entityID, sourceID string, departments []DirectoryDepartment, traceID string) error {
	// Find or create a company root for this entity.
	org, err := s.queries.GetFirstOrganization(ctx, entityID)
	if err != nil {
		name := "Company"
		if entity, entityErr := s.queries.GetEntityByID(ctx, entityID); entityErr == nil {
			name = strings.TrimSpace(entity.BrandName)
			if name == "" {
				name = strings.TrimSpace(entity.Name)
			}
		}
		if name == "" {
			name = "Company"
		}
		org, err = s.queries.CreateOrganization(ctx, generated.CreateOrganizationParams{
			EntityID: entityID,
			Name:     name,
		})
		if err != nil {
			return err
		}
	}

	// First pass: upsert all departments and index every known provider ID.
	deptIDs := make(map[string]string) // provider department/open_department ID → department ULID
	for _, dept := range departments {
		parentExternalID := strings.TrimSpace(dept.ParentExternalDepartmentID)
		preserveParent := input.SyncType == SyncModeIncremental && parentExternalID != "" && parentExternalID != "0"
		row, err := s.queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{
			EntityID:             entityID,
			OrganizationID:       org.ID,
			Name:                 dept.Name,
			SourceID:             pgtypeULID(sourceID),
			ExternalDepartmentID: pgtypeText(dept.ExternalDepartmentID),
			PreserveParent:       preserveParent,
		})
		if err != nil {
			return err
		}
		for _, key := range departmentLookupKeys(dept) {
			deptIDs[key] = row.ID
		}
	}

	// Second pass: resolve parent relationships
	for _, dept := range departments {
		parentExternalID := strings.TrimSpace(dept.ParentExternalDepartmentID)
		if parentExternalID == "" || parentExternalID == "0" {
			continue
		}
		childID, ok := deptIDs[dept.ExternalDepartmentID]
		if !ok {
			continue
		}
		parentID, ok := deptIDs[parentExternalID]
		if !ok && input.SyncType == SyncModeIncremental {
			parent, err := s.queries.GetDepartmentBySourceExternalID(ctx, generated.GetDepartmentBySourceExternalIDParams{
				EntityID: entityID, SourceID: pgtypeULID(sourceID), ExternalDepartmentID: pgtypeText(parentExternalID),
			})
			if err == nil {
				parentID = parent.ID
				ok = true
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if !ok {
			continue
		}
		if _, err := s.queries.UpdateDepartment(ctx, generated.UpdateDepartmentParams{
			EntityID: entityID,
			ID:       childID,
			Name:     pgtype.Text{String: dept.Name, Valid: true},
			ParentID: pgtypeULID(parentID),
		}); err != nil {
			return err
		}
	}
	return nil
}

func departmentLookupKeys(dept DirectoryDepartment) []string {
	keys := make([]string, 0, 3)
	appendKey := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "0" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	appendKey(dept.ExternalDepartmentID)
	var raw struct {
		DepartmentID     string `json:"department_id"`
		OpenDepartmentID string `json:"open_department_id"`
	}
	if len(dept.RawProfile) > 0 && json.Unmarshal(dept.RawProfile, &raw) == nil {
		appendKey(raw.DepartmentID)
		appendKey(raw.OpenDepartmentID)
	}
	return keys
}

func pgtypeULID(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: v != ""}
}

func pgtypeText(v string) pgtype.Text {
	return pgtype.Text{String: v, Valid: v != ""}
}

func normalizeDirectoryStatus(status string) string {
	switch status {
	case "active", "disabled", "deleted", "unknown":
		return status
	default:
		return "unknown"
	}
}

func lifecycleForDirectoryStatus(status string) string {
	switch normalizeDirectoryStatus(status) {
	case "active":
		return "active"
	case "disabled", "deleted":
		return "disabled"
	default:
		return "locked"
	}
}

func usernameForDirectoryUser(user DirectoryUser) string {
	if user.Email != "" {
		return user.Email
	}
	return user.ExternalUserID
}

func mustStatsJSON(result FullSyncResult) []byte {
	payload, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	return payload
}

func ulidValue(value string) (string, error) {
	if err := id.ValidateULID(value); err != nil {
		return "", err
	}
	return value, nil
}

func ulidString(value string) string {
	return value
}

func textValue(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func textMatches(value pgtype.Text, expected string) bool {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return !value.Valid || strings.TrimSpace(value.String) == ""
	}
	return value.Valid && strings.TrimSpace(value.String) == expected
}
