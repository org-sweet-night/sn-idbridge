// SPDX-License-Identifier: MIT

// Package model defines the audit event types shared across the
// application. It is intentionally kept free of dependencies on other
// internal packages so that any package (auth, sso, adminapi, worker)
// can import it without creating import cycles.
package model

// Action codes for audit events (stable, English-coded).
const (
	ActionLoginSuccess                     = "auth.login.success"
	ActionLoginFailed                      = "auth.login.failed"
	ActionLogout                           = "auth.logout"
	ActionTokenRevoke                      = "auth.token.revoke"
	ActionAuthorizeSuccess                 = "sso.authorize.success"
	ActionAuthorizeDenied                  = "sso.authorize.denied"
	ActionSyncStarted                      = "sync.feishu.started"
	ActionSyncFinished                     = "sync.feishu.finished"
	ActionSyncFailed                       = "sync.feishu.failed"
	ActionSyncDestructiveSnapshotConfirmed = "sync.feishu.destructive_snapshot_confirmed"
	ActionSyncUserCreated                  = "sync.user.created"
	ActionSyncUserDisabled                 = "sync.user.disabled"
	ActionSyncUserArchived                 = "sync.user.archived"
	ActionSyncDepartmentUpdated            = "sync.department.updated"
	ActionUserUpdated                      = "user.updated"
	ActionUserDisabled                     = "user.disabled"
	ActionUserArchived                     = "user.archived"
	ActionUserBoundIdentity                = "user.bound_identity"
	ActionUserUnboundIdentity              = "user.unbound_identity"
	ActionRoleCreated                      = "role.created"
	ActionRoleUpdated                      = "role.updated"
	ActionRolePermChanged                  = "role.permission_changed"
	ActionAppAssignChanged                 = "application.assignment_changed"
	ActionAppCreated                       = "application.created"
	ActionOIDCClientUpdated                = "oidc_client.updated"
	ActionSecretRotated                    = "secret.rotated"
)

// Event represents an audit event to be recorded.
type Event struct {
	EntityID     string
	ActorUserID  string // empty for system/sync_job actors
	ActorType    string // user, system, sync_job, api_client
	Action       string
	ResourceType string
	ResourceID   string
	Before       interface{} // marshaled to JSON
	After        interface{} // marshaled to JSON
	IP           string
	UserAgent    string
	TraceID      string
}
