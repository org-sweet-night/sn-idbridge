// SPDX-License-Identifier: MIT

package audit

import "github.com/smices/open-idb/internal/audit/model"

// Re-export action constants from the model sub-package so that existing
// callers (adminapi, worker) can continue to reference them as
// audit.ActionLoginSuccess, etc.
const (
	ActionLoginSuccess                     = model.ActionLoginSuccess
	ActionLoginFailed                      = model.ActionLoginFailed
	ActionLogout                           = model.ActionLogout
	ActionTokenRevoke                      = model.ActionTokenRevoke
	ActionAuthorizeSuccess                 = model.ActionAuthorizeSuccess
	ActionAuthorizeDenied                  = model.ActionAuthorizeDenied
	ActionSyncStarted                      = model.ActionSyncStarted
	ActionSyncFinished                     = model.ActionSyncFinished
	ActionSyncFailed                       = model.ActionSyncFailed
	ActionSyncDestructiveSnapshotConfirmed = model.ActionSyncDestructiveSnapshotConfirmed
	ActionSyncUserCreated                  = model.ActionSyncUserCreated
	ActionSyncUserDisabled                 = model.ActionSyncUserDisabled
	ActionSyncUserArchived                 = model.ActionSyncUserArchived
	ActionSyncDepartmentUpdated            = model.ActionSyncDepartmentUpdated
	ActionUserUpdated                      = model.ActionUserUpdated
	ActionUserDisabled                     = model.ActionUserDisabled
	ActionUserArchived                     = model.ActionUserArchived
	ActionUserBoundIdentity                = model.ActionUserBoundIdentity
	ActionUserUnboundIdentity              = model.ActionUserUnboundIdentity
	ActionRoleCreated                      = model.ActionRoleCreated
	ActionRoleUpdated                      = model.ActionRoleUpdated
	ActionRolePermChanged                  = model.ActionRolePermChanged
	ActionAppAssignChanged                 = model.ActionAppAssignChanged
	ActionAppCreated                       = model.ActionAppCreated
	ActionOIDCClientUpdated                = model.ActionOIDCClientUpdated
	ActionSecretRotated                    = model.ActionSecretRotated
)

// Event is an alias for model.Event. Defining it here keeps backward
// compatibility for packages that already import audit and use
// audit.Event in their interface signatures. New code that would
// otherwise create an import cycle should import audit/model directly.
type Event = model.Event
