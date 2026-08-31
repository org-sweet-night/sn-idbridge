// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/db/generated"
)

func TestExactDirectorySubjectReadsCurrentRolesAndLifecycleWithoutTreeCache(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAdminServiceTestPool(ctx, t)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Exact Subject", Slug: "exact-subject", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "local", Name: "Fixtures"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	directoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "owner", Name: "Owner", Status: "active", RawProfile: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create directory user: %v", err)
	}
	managed, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID: entity.ID, Username: "owner", DisplayName: "Owner", LifecycleStatus: "active", UserType: "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true}, Locale: pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create managed user: %v", err)
	}
	if _, err := queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
		EntityID: entity.ID, UserID: managed.ID, SourceID: source.ID, DirectoryUserID: directoryUser.ID, ProviderUid: "owner", IsPrimary: true,
	}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	requester, err := queries.CreateRole(ctx, generated.CreateRoleParams{EntityID: entity.ID, Name: "Requester", Code: "isa:pam:requester"})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := queries.AssignRoleToUser(ctx, generated.AssignRoleToUserParams{EntityID: entity.ID, UserID: managed.ID, RoleID: requester.ID}); err != nil {
		t.Fatalf("assign role: %v", err)
	}
	service, err := NewAdminService(queries)
	if err != nil {
		t.Fatalf("new admin service: %v", err)
	}
	node, err := service.GetDirectorySubject(ctx, entity.ID, managed.ID)
	if err != nil {
		t.Fatalf("first exact lookup: %v", err)
	}
	if !reflect.DeepEqual(node.Roles, []string{"isa:pam:requester"}) {
		t.Fatalf("roles = %#v", node.Roles)
	}
	if node.Version == "" {
		t.Fatal("exact subject version is empty")
	}
	versionWithRole := node.Version
	resyncedDirectoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "owner", Name: "Owner", Status: "active", RawProfile: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("repeat no-op directory sync: %v", err)
	}
	if resyncedDirectoryUser.UpdatedAt.Time.Equal(directoryUser.UpdatedAt.Time) {
		t.Fatal("no-op sync did not exercise volatile updated_at mutation")
	}
	node, err = service.GetDirectorySubject(ctx, entity.ID, managed.ID)
	if err != nil {
		t.Fatalf("exact lookup after no-op sync: %v", err)
	}
	if node.Version != versionWithRole {
		t.Fatalf("version after no-op sync = %q, want stable %q", node.Version, versionWithRole)
	}
	if err := queries.RemoveRoleFromUser(ctx, generated.RemoveRoleFromUserParams{EntityID: entity.ID, UserID: managed.ID, RoleID: requester.ID}); err != nil {
		t.Fatalf("remove role: %v", err)
	}
	node, err = service.GetDirectorySubject(ctx, entity.ID, managed.ID)
	if err != nil {
		t.Fatalf("lookup after role removal: %v", err)
	}
	if node.Roles != nil {
		t.Fatalf("roles after removal = %#v, want nil", node.Roles)
	}
	if node.Version == "" || node.Version == versionWithRole {
		t.Fatalf("version after role removal = %q, want a new nonempty version", node.Version)
	}
	if _, err := queries.UpdateUserLifecycle(ctx, generated.UpdateUserLifecycleParams{EntityID: entity.ID, ID: managed.ID, LifecycleStatus: "disabled"}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	_, err = service.GetDirectorySubject(ctx, entity.ID, managed.ID)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("lookup after disable error = %v, want pgx.ErrNoRows", err)
	}
}
