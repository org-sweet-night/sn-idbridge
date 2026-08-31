// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/smices/open-idb/internal/auth"
	"github.com/smices/open-idb/internal/idp"
	"go.uber.org/zap"
)

type SyncService interface {
	RunFullSync(ctx context.Context, input idp.FullSyncInput) (idp.FullSyncResult, error)
	RunIncrementalSync(ctx context.Context, input idp.FullSyncInput) (idp.FullSyncResult, error)
	SubmitWebhookEvent(ctx context.Context, entityID, sourceID string, event idp.DirectorySyncEvent) (string, error)
}

type DefaultFeishuWebhookTargetResolver interface {
	ResolveDefaultFeishuWebhookTarget(ctx context.Context) (entityID string, sourceID string, err error)
}

type OrganizationTreeCacheInvalidator interface {
	InvalidateOrganizationTree(ctx context.Context, entityID string) error
}

type ConsoleService interface {
	DashboardSummary(ctx context.Context, session auth.Session) (DashboardSummary, error)
	CurrentUser(ctx context.Context, session auth.Session) (CurrentUser, error)
	UpdateProfile(ctx context.Context, input UpdateProfileInput) (CurrentUser, error)
	UpdatePassword(ctx context.Context, input UpdatePasswordInput) error
}

type DashboardSummary struct {
	Users                int64  `json:"users"`
	ActiveUsers          int64  `json:"active_users"`
	NewUsers             int64  `json:"new_users"`
	AdminUsers           int64  `json:"admin_users"`
	ApplicationActivity  int64  `json:"application_activity"`
	PendingAuthorization int64  `json:"pending_authorization"`
	SyncHealth           string `json:"sync_health"`
}

type CurrentUser struct {
	ID                 string   `json:"id"`
	EntityID           string   `json:"entity_id"`
	Username           string   `json:"username"`
	DisplayName        string   `json:"display_name"`
	EnglishName        string   `json:"english_name,omitempty"`
	EmployeeNo         string   `json:"employee_no,omitempty"`
	JobTitle           string   `json:"job_title,omitempty"`
	Email              string   `json:"email,omitempty"`
	Phone              string   `json:"phone,omitempty"`
	AvatarURL          string   `json:"avatar_url,omitempty"`
	LifecycleStatus    string   `json:"lifecycle_status"`
	UserType           string   `json:"user_type"`
	PrimarySourceID    string   `json:"primary_source_id,omitempty"`
	PrimarySourceName  string   `json:"primary_source_name,omitempty"`
	Locale             string   `json:"locale"`
	MustChangePassword bool     `json:"must_change_password"`
	WeakPassword       bool     `json:"weak_password"`
	ConsoleScope       string   `json:"console_scope"`
	Capabilities       []string `json:"capabilities"`
}

type UpdatePasswordInput struct {
	EntityID        string
	UserID          string
	Username        string
	CurrentPassword string
	NewPassword     string
	WeakPassword    bool
}

type UpdateProfileInput struct {
	EntityID    string
	UserID      string
	DisplayName string
}

type Handler struct {
	syncService           SyncService
	consoleService        ConsoleService
	webhooks              *feishuWebhookRuntime
	organizationTreeCache OrganizationTreeCacheInvalidator
}

func (h *Handler) SetOrganizationTreeCacheInvalidator(invalidator OrganizationTreeCacheInvalidator) {
	h.organizationTreeCache = invalidator
}

func NewHandler(syncService SyncService, consoleService ConsoleService) Handler {
	return Handler{
		syncService:    syncService,
		consoleService: consoleService,
		webhooks:       newFeishuWebhookRuntime(),
	}
}

func (h Handler) RegisterRoutes(r chi.Router) {
	if h.syncService != nil {
		r.Post("/sapi/identity-sources/{source_id}/sync/full", h.triggerFullSync)
		r.Post("/sapi/identity-sources/{source_id}/sync/incremental", h.triggerIncrementalSync)
		r.Post("/api/webhooks/feishu", h.handleFeishuWebhook)
		r.Post("/api/webhooks/feishu/{entity_id}/{source_id}", h.handleFeishuWebhook)
	}
	if h.consoleService != nil {
		r.Get("/sapi/dashboard/summary", h.dashboardSummary)
		r.Get("/api/me", h.currentUser)
		r.Patch("/api/me", h.updateProfile)
		r.Post("/api/me/password", h.updatePassword)
	}
}

func (h Handler) triggerFullSync(w http.ResponseWriter, r *http.Request) {
	h.triggerSync(w, r, idp.SyncModeFull)
}

func (h Handler) triggerIncrementalSync(w http.ResponseWriter, r *http.Request) {
	h.triggerSync(w, r, idp.SyncModeIncremental)
}

func (h Handler) triggerSync(w http.ResponseWriter, r *http.Request, syncType idp.SyncMode) {
	entityID, ok := entityIDForRequest(w, r)
	if !ok {
		return
	}

	var (
		result idp.FullSyncResult
		err    error
	)
	input := idp.FullSyncInput{
		EntityID: entityID,
		SourceID: chi.URLParam(r, "source_id"),
		Provider: "",
		SyncType: syncType,
	}
	if syncType == idp.SyncModeFull {
		confirmation, err := parseDestructiveSnapshotConfirmation(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_sync_confirmation", err.Error())
			return
		}
		if confirmation != nil {
			admin, ok := auth.AdminSessionFromContext(r.Context())
			if !ok {
				cookie, cookieErr := r.Cookie("idb_admin_session")
				if cookieErr != nil || strings.TrimSpace(cookie.Value) == "" {
					writeError(w, http.StatusUnauthorized, "admin_session_required", "idb_admin_session cookie is required")
					return
				}
				admin, cookieErr = auth.ResolveAdminSession(r.Context(), cookie.Value)
				if cookieErr != nil {
					writeError(w, http.StatusUnauthorized, "invalid_admin_session", "idb_admin_session cookie is invalid")
					return
				}
			}
			// The target entity may be supplied by an internal caller header,
			// but destructive approval is always scoped to the authenticated
			// admin's tenant. Only a platform admin may cross that boundary.
			if strings.TrimSpace(admin.Role) != "platform_admin" && strings.TrimSpace(admin.EntityID) != strings.TrimSpace(entityID) {
				writeError(w, http.StatusForbidden, "invalid_sync_confirmation", "admin session is not authorized for this entity")
				return
			}
			confirmation.ConfirmedBy = strings.TrimSpace(admin.AdminID)
			if confirmation.ConfirmedBy == "" {
				writeError(w, http.StatusForbidden, "invalid_sync_confirmation", "admin identity is required for destructive sync confirmation")
				return
			}
			input.DestructiveConfirmation = confirmation
		}
	}

	switch syncType {
	case idp.SyncModeIncremental:
		result, err = h.syncService.RunIncrementalSync(r.Context(), input)
	default:
		result, err = h.syncService.RunFullSync(r.Context(), input)
	}
	if err != nil {
		if errors.Is(err, idp.ErrSyncAlreadyRunning) {
			writeError(w, http.StatusConflict, "sync_in_progress", "a sync is already running for this identity source")
			return
		}
		var snapshotErr *idp.SuspiciousFullSnapshotError
		if errors.As(err, &snapshotErr) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":                "destructive_snapshot_confirmation_required",
				"error_description":    "full sync rejected a potentially truncated snapshot; review the snapshot and retry with an authenticated destructive_snapshot_confirmation",
				"snapshot_fingerprint": snapshotErr.SnapshotFingerprint,
				"current_users":        snapshotErr.CurrentUsers,
				"current_departments":  snapshotErr.CurrentDepartments,
				"incoming_users":       snapshotErr.IncomingUsers,
				"incoming_departments": snapshotErr.IncomingDepartments,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "sync_failed", err.Error())
		return
	}
	if h.organizationTreeCache != nil {
		if cacheErr := h.organizationTreeCache.InvalidateOrganizationTree(r.Context(), entityID); cacheErr != nil {
			if h.webhooks != nil && h.webhooks.logger != nil {
				h.webhooks.logger.Warn("organization tree cache invalidation failed after manual sync", zap.Error(cacheErr))
			}
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func parseDestructiveSnapshotConfirmation(r *http.Request) (*idp.DestructiveSnapshotConfirmation, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return nil, nil
	}
	var payload struct {
		DestructiveSnapshotConfirmation *idp.DestructiveSnapshotConfirmation `json:"destructive_snapshot_confirmation"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	if err := decoder.Decode(&payload); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("invalid JSON request body")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("request body must contain one JSON object")
	}
	return payload.DestructiveSnapshotConfirmation, nil
}

func (h Handler) handleFeishuWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFeishuWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "webhook_body_too_large", "webhook body exceeds the size limit")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_webhook_body", "invalid request body")
		return
	}
	if h.syncService == nil {
		writeError(w, http.StatusServiceUnavailable, "sync_service_unavailable", "sync service is not configured")
		return
	}

	entityID, sourceID, ok := h.feishuWebhookTarget(w, r)
	if !ok {
		return
	}
	runtime := h.webhooks
	if runtime == nil {
		runtime = newFeishuWebhookRuntime()
	}
	securityConfig, err := runtime.securityConfig(r.Context(), entityID, sourceID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "webhook_security_unavailable", "webhook security configuration is unavailable")
		return
	}
	payload, err := prepareFeishuWebhookPayload(r, body, securityConfig)
	if err != nil {
		writeWebhookSecurityError(w, err)
		return
	}

	var envelope struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook_body", "invalid json body")
		return
	}
	if strings.EqualFold(envelope.Type, "url_verification") {
		if strings.TrimSpace(envelope.Challenge) == "" {
			writeError(w, http.StatusBadRequest, "invalid_webhook_challenge", "webhook challenge is required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"challenge": envelope.Challenge,
		})
		return
	}
	event, err := parseFeishuWebhookEvent(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_webhook_event", err.Error())
		return
	}
	if !event.IsKnown() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if event.IsDeleteEvent() && !isVerifiedTypedFeishuDelete(securityConfig, event) {
		if runtime.logger != nil {
			runtime.logger.Warn("ignored unverified or untyped feishu delete webhook",
				zap.String("entity_id", entityID),
				zap.String("source_id", sourceID),
				zap.String("event_id", event.EventID),
				zap.String("object_type", event.ObjectType),
				zap.String("object_id_type", event.ObjectIDType),
			)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	dedupeKey := feishuWebhookDedupeKey(entityID, sourceID, event.EventID, payload)
	if !runtime.deduper.reserve(dedupeKey, runtime.now()) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	if !runtime.runner.tryReserve() {
		runtime.deduper.remove(dedupeKey)
		writeError(w, http.StatusServiceUnavailable, "webhook_processing_busy", "webhook processing capacity is full")
		return
	}
	jobID, err := h.syncService.SubmitWebhookEvent(r.Context(), entityID, sourceID, event)
	if err != nil {
		runtime.runner.release()
		runtime.deduper.remove(dedupeKey)
		writeError(w, http.StatusBadRequest, "submit_webhook_failed", err.Error())
		return
	}

	// Run at most a fixed number of incremental syncs concurrently. Overload is
	// returned to Feishu so its delivery retry policy can apply.
	runtime.runner.runReserved(func(ctx context.Context) {
		_, runErr := h.syncService.RunIncrementalSync(ctx, idp.FullSyncInput{
			EntityID: entityID,
			SourceID: sourceID,
		})
		if runErr != nil {
			runtime.deduper.remove(dedupeKey)
			if runtime.logger != nil {
				runtime.logger.Warn("feishu webhook incremental sync failed",
					zap.String("entity_id", entityID),
					zap.String("source_id", sourceID),
					zap.String("event_id", event.EventID),
					zap.String("job_id", jobID),
					zap.Error(runErr),
				)
			}
			return
		}
		if h.organizationTreeCache != nil {
			if cacheErr := h.organizationTreeCache.InvalidateOrganizationTree(ctx, entityID); cacheErr != nil && runtime.logger != nil {
				runtime.logger.Warn("organization tree cache invalidation failed after webhook sync",
					zap.String("entity_id", entityID),
					zap.String("source_id", sourceID),
					zap.String("job_id", jobID),
					zap.Error(cacheErr),
				)
			}
		}
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "accepted",
		"job_id": jobID,
	})
}

func isVerifiedTypedFeishuDelete(cfg FeishuWebhookSecurityConfig, event idp.DirectorySyncEvent) bool {
	if strings.TrimSpace(cfg.VerificationToken) == "" && strings.TrimSpace(cfg.EncryptKey) == "" {
		return false
	}
	identifierType := strings.ToLower(strings.TrimSpace(event.ObjectIDType))
	switch strings.ToLower(strings.TrimSpace(event.ObjectType)) {
	case "user":
		return identifierType == "user_id" || identifierType == "open_id" || identifierType == "union_id"
	case "department":
		return identifierType == "department_id" || identifierType == "open_department_id"
	default:
		return false
	}
}

func writeWebhookSecurityError(w http.ResponseWriter, err error) {
	var securityErr *webhookSecurityError
	if errors.As(err, &securityErr) {
		writeError(w, securityErr.status, securityErr.code, securityErr.message)
		return
	}
	writeError(w, http.StatusUnauthorized, "invalid_webhook", "webhook verification failed")
}

func (h Handler) feishuWebhookTarget(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	entityID := chi.URLParam(r, "entity_id")
	sourceID := chi.URLParam(r, "source_id")
	if entityID != "" && sourceID != "" {
		return entityID, sourceID, true
	}
	resolver, ok := h.syncService.(DefaultFeishuWebhookTargetResolver)
	if !ok {
		writeError(w, http.StatusBadRequest, "default_feishu_webhook_unavailable", "default feishu webhook target is not configured")
		return "", "", false
	}
	entityID, sourceID, err := resolver.ResolveDefaultFeishuWebhookTarget(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, "default_feishu_webhook_unavailable", err.Error())
		return "", "", false
	}
	return entityID, sourceID, true
}

func entityIDForRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	headerEntityID := strings.TrimSpace(r.Header.Get("X-IDB-Entity-ID"))
	if headerEntityID != "" {
		// Internal callers may supply the target header, but an authenticated
		// admin session still binds that target to its entity. The outer HTTP
		// middleware rejects missing/invalid admin cookies; keeping this lookup
		// optional preserves direct internal-handler tests and service callers.
		if admin, ok := adminSessionIfPresent(r); ok && strings.TrimSpace(admin.Role) != "platform_admin" && strings.TrimSpace(admin.EntityID) != headerEntityID {
			writeError(w, http.StatusForbidden, "entity_scope_forbidden", "admin session is not authorized for this entity")
			return "", false
		}
		return headerEntityID, true
	}
	session, ok := readSession(w, r)
	if !ok {
		return "", false
	}
	return session.EntityID, true
}

func adminSessionIfPresent(r *http.Request) (auth.AdminSession, bool) {
	if session, ok := auth.AdminSessionFromContext(r.Context()); ok {
		return session, true
	}
	cookie, err := r.Cookie("idb_admin_session")
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return auth.AdminSession{}, false
	}
	session, err := auth.ResolveAdminSession(r.Context(), cookie.Value)
	if err != nil {
		return auth.AdminSession{}, false
	}
	return session, true
}

func (h Handler) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	summary, err := h.consoleService.DashboardSummary(r.Context(), session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "dashboard_summary_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (h Handler) currentUser(w http.ResponseWriter, r *http.Request) {
	session, ok := readUserSession(w, r)
	if !ok {
		return
	}
	user, err := h.consoleService.CurrentUser(r.Context(), session)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "current_user_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h Handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	session, ok := readUserSession(w, r)
	if !ok {
		return
	}
	var request struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile_request", "invalid json body")
		return
	}
	displayName := strings.TrimSpace(request.DisplayName)
	if displayName == "" {
		writeError(w, http.StatusBadRequest, "invalid_display_name", "display_name is required")
		return
	}
	user, err := h.consoleService.UpdateProfile(r.Context(), UpdateProfileInput{
		EntityID:    session.EntityID,
		UserID:      session.UserID,
		DisplayName: displayName,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "profile_update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h Handler) updatePassword(w http.ResponseWriter, r *http.Request) {
	session, ok := readUserSession(w, r)
	if !ok {
		return
	}
	var request struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_password_request", "invalid json body")
		return
	}
	weakPassword := isWeakPassword(request.NewPassword)
	if weakPassword {
		writeError(w, http.StatusBadRequest, "weak_password", "new password does not meet minimum strength requirements")
		return
	}
	err := h.consoleService.UpdatePassword(r.Context(), UpdatePasswordInput{
		EntityID:        session.EntityID,
		UserID:          session.UserID,
		Username:        session.Username,
		CurrentPassword: request.CurrentPassword,
		NewPassword:     request.NewPassword,
		WeakPassword:    weakPassword,
	})
	if err != nil {
		writeError(w, http.StatusUnauthorized, "password_update_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readSession(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	cookie, err := r.Cookie("idb_admin_session")
	if err != nil {
		writeError(w, http.StatusUnauthorized, "admin_session_required", "idb_admin_session cookie is required")
		return auth.Session{}, false
	}
	adminSession, err := auth.ResolveAdminSession(r.Context(), cookie.Value)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_admin_session", "idb_admin_session cookie is invalid")
		return auth.Session{}, false
	}
	return auth.Session{
		ID:          adminSession.ID,
		UserID:      adminSession.AdminID,
		EntityID:    adminSession.EntityID,
		Username:    adminSession.Username,
		DisplayName: adminSession.DisplayName,
		ExpiresAt:   adminSession.ExpiresAt,
	}, true
}

func readUserSession(w http.ResponseWriter, r *http.Request) (auth.Session, bool) {
	cookie, err := r.Cookie("idb_session")
	if err != nil {
		writeError(w, http.StatusUnauthorized, "session_required", "idb_session cookie is required")
		return auth.Session{}, false
	}
	session, err := auth.ResolveSession(r.Context(), cookie.Value)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_session", "idb_session cookie is invalid")
		return auth.Session{}, false
	}
	return session, true
}

func isWeakPassword(password string) bool {
	if len(password) < 6 {
		return true
	}
	hasLetter := strings.ContainsAny(password, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	hasDigit := strings.ContainsAny(password, "0123456789")
	return !(hasLetter && hasDigit)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": message,
	})
}

func parseFeishuWebhookEvent(payload []byte) (idp.DirectorySyncEvent, error) {
	var envelope struct {
		Type         string          `json:"type"`
		EventType    string          `json:"event_type"`
		EventID      string          `json:"event_id"`
		ObjectType   string          `json:"object_type"`
		ObjectID     string          `json:"object_id"`
		ObjectIDType string          `json:"object_id_type"`
		EventPayload json.RawMessage `json:"event"`
		Header       struct {
			EventType string `json:"event_type"`
			EventID   string `json:"event_id"`
		} `json:"header"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return idp.DirectorySyncEvent{}, err
	}

	var nested struct {
		EventType    string `json:"event_type"`
		Type         string `json:"type"`
		ObjectType   string `json:"object_type"`
		ObjectID     string `json:"object_id"`
		ObjectIDType string `json:"object_id_type"`
		UserID       string `json:"user_id"`
		OpenID       string `json:"open_id"`
		UnionID      string `json:"union_id"`
		DepartmentID string `json:"department_id"`
		OpenDeptID   string `json:"open_department_id"`
		EventID      string `json:"event_id"`
	}

	if len(envelope.EventPayload) > 0 {
		if err := json.Unmarshal(envelope.EventPayload, &nested); err != nil {
			return idp.DirectorySyncEvent{}, err
		}
	}

	eventType := firstNonEmptyString(envelope.EventType, envelope.Header.EventType, nested.EventType, nested.Type)
	eventPayload := map[string]interface{}{}
	if len(envelope.EventPayload) > 0 {
		_ = json.Unmarshal(envelope.EventPayload, &eventPayload)
	}
	eventObjectType := strings.ToLower(strings.TrimSpace(firstNonEmptyString(
		envelope.ObjectType,
		nested.ObjectType,
		stringValue(eventPayload, "object_type"),
	)))
	objectIDType := firstNonEmptyString(
		nested.ObjectIDType,
		envelope.ObjectIDType,
	)
	objectID := firstNonEmptyString(
		envelope.ObjectID,
		nested.ObjectID,
		nested.UserID,
		nested.OpenID,
		nested.UnionID,
		nested.DepartmentID,
		nested.OpenDeptID,
		stringValue(eventPayload, "user_id"),
		stringValue(eventPayload, "open_id"),
		stringValue(eventPayload, "union_id"),
		stringValue(eventPayload, "department_id"),
		stringValue(eventPayload, "open_department_id"),
	)

	if eventObjectType == "" {
		if nested.UserID != "" || nested.OpenID != "" || nested.UnionID != "" {
			eventObjectType = "user"
		}
		if nested.DepartmentID != "" || nested.OpenDeptID != "" {
			eventObjectType = "department"
		}
	}
	if eventObjectType == "" {
		if stringValue(eventPayload, "user_id") != "" ||
			stringValue(eventPayload, "open_id") != "" ||
			stringValue(eventPayload, "union_id") != "" {
			eventObjectType = "user"
		}
		if stringValue(eventPayload, "department_id") != "" ||
			stringValue(eventPayload, "open_department_id") != "" ||
			stringValue(eventPayload, "department_type") != "" {
			eventObjectType = "department"
		}
	}
	if eventObjectType == "" {
		normalizedEventType := strings.ToLower(strings.TrimSpace(eventType))
		switch {
		case strings.Contains(normalizedEventType, "user") || strings.Contains(normalizedEventType, "employee"):
			eventObjectType = "user"
		case strings.Contains(normalizedEventType, "department") || strings.Contains(normalizedEventType, "dept"):
			eventObjectType = "department"
		}
	}

	if eventObjectType == "" {
		return idp.DirectorySyncEvent{}, jsonError("unsupported webhook event")
	}
	if objectID == "" {
		objectFromPayload := firstNonEmptyString(
			extractObjectID(eventPayload, eventObjectType),
			stringValue(eventPayload, eventObjectType+"_id"),
		)
		objectID = objectFromPayload
	}
	if objectID == "" {
		return idp.DirectorySyncEvent{}, jsonError("webhook event missing object id")
	}

	if objectIDType == "" && eventObjectType == "user" && nested.UserID != "" {
		objectIDType = "user_id"
	}
	if objectIDType == "" && eventObjectType == "department" && nested.DepartmentID != "" {
		objectIDType = "department_id"
	}
	if objectIDType == "" {
		objectIDType = extractObjectIDType(eventPayload, eventObjectType)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return idp.DirectorySyncEvent{}, err
	}

	return idp.DirectorySyncEvent{
		EventType:    strings.ToLower(strings.TrimSpace(eventType)),
		ObjectType:   eventObjectType,
		ObjectID:     objectID,
		ObjectIDType: objectIDType,
		EventID:      firstNonEmptyString(envelope.EventID, envelope.Header.EventID, nested.EventID),
		Raw:          raw,
	}, nil
}

func stringValue(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if raw, ok := values[key]; ok {
			if value, ok := raw.(string); ok {
				value = strings.TrimSpace(value)
				if value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func extractObjectID(payload map[string]interface{}, objectType string) string {
	objectNode := feishuWebhookObjectNode(payload, objectType)
	if objectNode == nil {
		return ""
	}
	switch objectType {
	case "user":
		return firstNonEmptyString(
			stringValue(objectNode, "user_id"),
			stringValue(objectNode, "open_id"),
			stringValue(objectNode, "union_id"),
			stringValue(objectNode, "id"),
		)
	case "department":
		return firstNonEmptyString(
			stringValue(objectNode, "department_id"),
			stringValue(objectNode, "open_department_id"),
			stringValue(objectNode, "id"),
		)
	default:
		return ""
	}
}

func extractObjectIDType(payload map[string]interface{}, objectType string) string {
	objectNode := feishuWebhookObjectNode(payload, objectType)
	if objectNode == nil {
		return ""
	}
	switch objectType {
	case "user":
		for _, key := range []string{"user_id", "open_id", "union_id"} {
			if stringValue(objectNode, key) != "" {
				return key
			}
		}
	case "department":
		for _, key := range []string{"department_id", "open_department_id"} {
			if stringValue(objectNode, key) != "" {
				return key
			}
		}
	}
	return ""
}

func feishuWebhookObjectNode(payload map[string]interface{}, objectType string) map[string]interface{} {
	if objectNode, ok := payload["object"].(map[string]interface{}); ok {
		return objectNode
	}
	if objectNode, ok := payload[objectType].(map[string]interface{}); ok {
		return objectNode
	}
	return nil
}

func jsonError(message string) error {
	return &adminJSONError{msg: message}
}

type adminJSONError struct {
	msg string
}

func (e *adminJSONError) Error() string {
	return e.msg
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
