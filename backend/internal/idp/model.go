// SPDX-License-Identifier: MIT

package idp

import (
	"context"
	"strings"
)

type DirectoryDepartment struct {
	ExternalDepartmentID       string
	ParentExternalDepartmentID string
	Name                       string
	RawProfile                 []byte
}

type DirectoryUser struct {
	ExternalUserID  string
	ExternalUnionID string
	ExternalOpenID  string
	Name            string
	EnglishName     string
	EmployeeNo      string
	JobTitle        string
	Email           string
	Phone           string
	AvatarURL       string
	Status          string
	RawProfile      []byte
}

// DirectoryObjectDeletion carries the provider identifier from a delete
// webhook without pretending that it is the provider's canonical ID. The
// sync layer resolves it against existing rows and never creates a tombstone
// for an object that is not already known locally.
type DirectoryObjectDeletion struct {
	ObjectID     string
	ObjectIDType string
}

type FullSyncData struct {
	Departments         []DirectoryDepartment
	Users               []DirectoryUser
	DepartmentDeletions []DirectoryObjectDeletion
	UserDeletions       []DirectoryObjectDeletion
}

// DestructiveSnapshotConfirmation is an operator approval for a full-sync
// snapshot that the safety guard classified as a potentially truncated
// response.  The fingerprint and row counts bind the approval to the exact
// snapshot observed by the failed run; ConfirmedBy is populated by the
// authenticated admin handler and is never accepted from request JSON.
type DestructiveSnapshotConfirmation struct {
	SnapshotFingerprint string `json:"snapshot_fingerprint"`
	CurrentUsers        int64  `json:"current_users"`
	CurrentDepartments  int64  `json:"current_departments"`
	ConfirmedBy         string `json:"-"`
}

type DirectorySyncEvent struct {
	EventType    string
	ObjectType   string
	ObjectID     string
	ObjectIDType string
	EventID      string
	Raw          map[string]interface{}
}

func (e DirectorySyncEvent) IsUserEvent() bool {
	return e.ObjectType == "user"
}

func (e DirectorySyncEvent) IsDepartmentEvent() bool {
	return e.ObjectType == "department"
}

func (e DirectorySyncEvent) IsDeleteEvent() bool {
	eventType := strings.ToLower(strings.TrimSpace(e.EventType))
	return strings.Contains(eventType, "delete")
}

func (e DirectorySyncEvent) IsKnown() bool {
	return e.IsUserEvent() || e.IsDepartmentEvent()
}

type SyncMode string

const (
	SyncModeFull        SyncMode = "full"
	SyncModeIncremental SyncMode = "incremental"
)

type DirectoryProvider interface {
	FullSync(ctx context.Context) (FullSyncData, error)
	IncrementalSync(ctx context.Context, events []DirectorySyncEvent) (FullSyncData, error)
}
