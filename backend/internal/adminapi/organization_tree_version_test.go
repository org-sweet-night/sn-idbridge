// SPDX-License-Identifier: MIT

package adminapi

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/db/generated"
)

func TestDirectoryAuthorizationVersionUsesOnlyCanonicalAuthorizationState(t *testing.T) {
	row := generated.DirectoryUser{
		ID: "01HZZZZZZZ0000000000000011", SourceID: "01HZZZZZZZ0000000000000012", Status: "active",
		Name: "Owner", UpdatedAt: pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true},
	}
	base := directoryAuthorizationVersion(testEntityID(), testUserID(), "active", row, []string{"isa:pam:requester", "employee"})
	if base == "" {
		t.Fatal("version is empty")
	}
	resynced := row
	resynced.Name = "Updated display name"
	resynced.UpdatedAt = pgtype.Timestamptz{Time: time.Unix(2, 0), Valid: true}
	if got := directoryAuthorizationVersion(testEntityID(), testUserID(), "active", resynced, []string{"employee", "isa:pam:requester"}); got != base {
		t.Fatalf("display/timestamp/order-only change produced version %q, want %q", got, base)
	}

	changes := []struct {
		name      string
		lifecycle string
		row       generated.DirectoryUser
		roles     []string
	}{
		{name: "managed lifecycle", lifecycle: "disabled", row: row, roles: []string{"employee", "isa:pam:requester"}},
		{name: "role set", lifecycle: "active", row: row, roles: []string{"employee"}},
		{name: "directory status", lifecycle: "active", row: withDirectoryStatus(row, "disabled"), roles: []string{"employee", "isa:pam:requester"}},
		{name: "binding source", lifecycle: "active", row: withDirectorySource(row, "01HZZZZZZZ0000000000000013"), roles: []string{"employee", "isa:pam:requester"}},
		{name: "binding target", lifecycle: "active", row: withDirectoryID(row, "01HZZZZZZZ0000000000000014"), roles: []string{"employee", "isa:pam:requester"}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			if got := directoryAuthorizationVersion(testEntityID(), testUserID(), change.lifecycle, change.row, change.roles); got == base {
				t.Fatalf("authorization-relevant change retained version %q", got)
			}
		})
	}
}

func withDirectoryStatus(row generated.DirectoryUser, status string) generated.DirectoryUser {
	row.Status = status
	return row
}

func withDirectorySource(row generated.DirectoryUser, sourceID string) generated.DirectoryUser {
	row.SourceID = sourceID
	return row
}

func withDirectoryID(row generated.DirectoryUser, id string) generated.DirectoryUser {
	row.ID = id
	return row
}
