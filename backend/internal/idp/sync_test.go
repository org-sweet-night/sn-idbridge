// SPDX-License-Identifier: MIT

package idp

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/smices/open-idb/internal/audit"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestLifecycleForDirectoryStatus(t *testing.T) {
	tests := map[string]string{
		"active":   "active",
		"disabled": "disabled",
		"deleted":  "disabled",
		"unknown":  "locked",
		"other":    "locked",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			if got := lifecycleForDirectoryStatus(input); got != want {
				t.Fatalf("lifecycleForDirectoryStatus(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestUsernameForDirectoryUser(t *testing.T) {
	if got := usernameForDirectoryUser(DirectoryUser{Email: "ada@example.test", ExternalUserID: "ou_1"}); got != "ada@example.test" {
		t.Fatalf("username = %q", got)
	}
	if got := usernameForDirectoryUser(DirectoryUser{ExternalUserID: "ou_1"}); got != "ou_1" {
		t.Fatalf("username = %q", got)
	}
}

func TestUniqueDirectoryUsersRejectsMissingExternalUserID(t *testing.T) {
	_, err := uniqueDirectoryUsers([]DirectoryUser{{
		Name:           "Missing Provider ID",
		ExternalUserID: "   ",
	}})
	if err == nil {
		t.Fatal("uniqueDirectoryUsers error = nil, want missing external user id error")
	}
}

func TestUniqueDirectoryUsersRejectsConflictingProviderIdentifiers(t *testing.T) {
	_, err := uniqueDirectoryUsers([]DirectoryUser{
		{ExternalUserID: "user_a", ExternalOpenID: "shared_open_id", ExternalUnionID: "union_a"},
		{ExternalUserID: "user_b", ExternalOpenID: "shared_open_id", ExternalUnionID: "union_b"},
	})
	if err == nil || !strings.Contains(err.Error(), "open_id") {
		t.Fatalf("uniqueDirectoryUsers error = %v, want open_id collision", err)
	}

	_, err = uniqueDirectoryUsers([]DirectoryUser{
		{ExternalUserID: "same_user", ExternalOpenID: "open_a"},
		{ExternalUserID: "same_user", ExternalOpenID: "open_b"},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting open_id") {
		t.Fatalf("uniqueDirectoryUsers duplicate error = %v, want conflicting open_id", err)
	}

	_, err = uniqueDirectoryUsers([]DirectoryUser{
		{ExternalUserID: "shared_cross_namespace"},
		{ExternalUserID: "user_b", ExternalOpenID: "shared_cross_namespace"},
	})
	if err == nil || !strings.Contains(err.Error(), "open_id") {
		t.Fatalf("uniqueDirectoryUsers cross-namespace error = %v, want identifier collision", err)
	}
}

func TestPrepareDirectoryDepartmentsRejectsConflictingProviderIdentifiers(t *testing.T) {
	_, err := prepareDirectoryDepartments([]DirectoryDepartment{
		{ExternalDepartmentID: "department_a", RawProfile: []byte(`{"department_id":"department_a","open_department_id":"shared_open_department"}`)},
		{ExternalDepartmentID: "department_b", RawProfile: []byte(`{"department_id":"department_b","open_department_id":"shared_open_department"}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "open_department_id") {
		t.Fatalf("prepareDirectoryDepartments error = %v, want open_department_id collision", err)
	}

	_, err = prepareDirectoryDepartments([]DirectoryDepartment{
		{ExternalDepartmentID: "same_department", RawProfile: []byte(`{"department_id":"same_department","open_department_id":"open_a"}`)},
		{ExternalDepartmentID: "same_department", RawProfile: []byte(`{"department_id":"same_department","open_department_id":"open_b"}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting open_department_id") {
		t.Fatalf("prepareDirectoryDepartments duplicate error = %v, want conflicting open_department_id", err)
	}

	_, err = prepareDirectoryDepartments([]DirectoryDepartment{
		{ExternalDepartmentID: "shared_cross_namespace", RawProfile: []byte(`{"department_id":"shared_cross_namespace"}`)},
		{ExternalDepartmentID: "department_b", RawProfile: []byte(`{"department_id":"department_b","open_department_id":"shared_cross_namespace"}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "open_department_id") {
		t.Fatalf("prepareDirectoryDepartments cross-namespace error = %v, want identifier collision", err)
	}
}

func TestValidDirectoryDeletionIdentifierRequiresExplicitProviderIDType(t *testing.T) {
	tests := []struct {
		name     string
		object   string
		deletion DirectoryObjectDeletion
		want     bool
	}{
		{name: "user id", object: "user", deletion: DirectoryObjectDeletion{ObjectID: "u1", ObjectIDType: "user_id"}, want: true},
		{name: "open id", object: "user", deletion: DirectoryObjectDeletion{ObjectID: "o1", ObjectIDType: "open_id"}, want: true},
		{name: "union id", object: "user", deletion: DirectoryObjectDeletion{ObjectID: "n1", ObjectIDType: "union_id"}, want: true},
		{name: "department id", object: "department", deletion: DirectoryObjectDeletion{ObjectID: "d1", ObjectIDType: "department_id"}, want: true},
		{name: "open department id", object: "department", deletion: DirectoryObjectDeletion{ObjectID: "od1", ObjectIDType: "open_department_id"}, want: true},
		{name: "missing type", object: "user", deletion: DirectoryObjectDeletion{ObjectID: "n1"}, want: false},
		{name: "unknown type", object: "user", deletion: DirectoryObjectDeletion{ObjectID: "n1", ObjectIDType: "employee_id"}, want: false},
		{name: "missing id", object: "department", deletion: DirectoryObjectDeletion{ObjectIDType: "department_id"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validDirectoryDeletionIdentifier(test.object, test.deletion); got != test.want {
				t.Fatalf("validDirectoryDeletionIdentifier() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestAcquireSourceLockRejectsConcurrentSyncForSameSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	first, err := NewSyncService(SyncServiceConfig{Queries: queries, Provider: fakeSyncDirectoryProvider{}, TxStarter: pool})
	if err != nil {
		t.Fatalf("new first sync service: %v", err)
	}
	second, err := NewSyncService(SyncServiceConfig{Queries: queries, Provider: fakeSyncDirectoryProvider{}, TxStarter: pool})
	if err != nil {
		t.Fatalf("new second sync service: %v", err)
	}

	release, err := first.acquireSourceLock(ctx, "01HZZZZZZZ0000000000000001", "01HZZZZZZZ0000000000000002")
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer release()

	if _, err := second.acquireSourceLock(ctx, "01HZZZZZZZ0000000000000001", "01HZZZZZZZ0000000000000002"); !errors.Is(err, ErrSyncAlreadyRunning) {
		t.Fatalf("second lock error = %v, want ErrSyncAlreadyRunning", err)
	}
}

func TestFullSyncRollsBackPreparedSnapshotWhenDatabaseWriteFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Atomic Full Sync Entity",
		Slug:          "atomic-full-sync",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_atomic_full_sync_department() RETURNS trigger AS $$
		BEGIN
			IF NEW.external_department_id = 'od_force_failure' THEN
				RAISE EXCEPTION 'forced full sync failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_atomic_full_sync_department
		BEFORE INSERT OR UPDATE ON directory_departments
		FOR EACH ROW EXECUTE FUNCTION fail_atomic_full_sync_department();
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{Departments: []DirectoryDepartment{
			{ExternalDepartmentID: "od_before_failure", Name: "Before Failure", RawProfile: []byte(`{}`)},
			{ExternalDepartmentID: "od_force_failure", Name: "Force Failure", RawProfile: []byte(`{}`)},
		}}},
		TraceID:   func() string { return "trace-atomic-full-sync" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	result, err := service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if err == nil {
		t.Fatal("RunFullSync() error = nil, want forced database error")
	}

	var committedDepartments int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM directory_departments
		WHERE entity_id = $1 AND source_id = $2
	`, entity.ID, source.ID).Scan(&committedDepartments); err != nil {
		t.Fatalf("count committed departments: %v", err)
	}
	if committedDepartments != 0 {
		t.Fatalf("committed departments = %d, want 0 after rollback", committedDepartments)
	}

	var jobStatus string
	var jobError pgtype.Text
	if err := pool.QueryRow(ctx, `
		SELECT status, error_message
		FROM sync_jobs
		WHERE entity_id = $1 AND id = $2
	`, entity.ID, result.JobID).Scan(&jobStatus, &jobError); err != nil {
		t.Fatalf("read failed sync job: %v", err)
	}
	if jobStatus != "failed" || !jobError.Valid {
		t.Fatalf("job status/error = %q/%#v, want failed with error", jobStatus, jobError)
	}

	if _, err := pool.Exec(ctx, `
		DROP TRIGGER fail_atomic_full_sync_department ON directory_departments;
		DROP FUNCTION fail_atomic_full_sync_department();
	`); err != nil {
		t.Fatalf("remove failure trigger: %v", err)
	}
	service.provider = fakeSyncDirectoryProvider{data: FullSyncData{Departments: []DirectoryDepartment{
		{ExternalDepartmentID: "od_after_recovery", Name: "After Recovery", RawProfile: []byte(`{}`)},
	}}}
	successResult, err := service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("RunFullSync() after rollback error = %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status
		FROM sync_jobs
		WHERE entity_id = $1 AND id = $2
	`, entity.ID, successResult.JobID).Scan(&jobStatus); err != nil {
		t.Fatalf("read successful sync job: %v", err)
	}
	if jobStatus != "succeeded" {
		t.Fatalf("successful job status = %q, want succeeded", jobStatus)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM directory_departments
		WHERE entity_id = $1 AND source_id = $2 AND external_department_id = 'od_after_recovery'
	`, entity.ID, source.ID).Scan(&committedDepartments); err != nil {
		t.Fatalf("count recovered departments: %v", err)
	}
	if committedDepartments != 1 {
		t.Fatalf("recovered departments = %d, want 1", committedDepartments)
	}
}

func TestFullSyncUsesExactUserIDAndReconcilesStaleIdentifierDuplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "User Identifier Collision", Slug: "user-identifier-collision", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	for _, fixture := range []generated.UpsertDirectoryUserParams{
		{EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "reused_user_id", ExternalOpenID: textValue("open_a"), ExternalUnionID: textValue("union_a"), Name: "Existing A", Status: "active", RawProfile: []byte(`{"user_id":"reused_user_id","open_id":"open_a","union_id":"union_a"}`)},
		{EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "legacy_user_b", ExternalOpenID: textValue("stable_open_b"), ExternalUnionID: textValue("stable_union_b"), Name: "Existing B", Status: "active", RawProfile: []byte(`{"user_id":"legacy_user_b","open_id":"stable_open_b","union_id":"stable_union_b"}`)},
	} {
		if _, err := queries.UpsertDirectoryUser(ctx, fixture); err != nil {
			t.Fatalf("seed directory user: %v", err)
		}
	}
	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			Departments: []DirectoryDepartment{{ExternalDepartmentID: "new_department_before_collision", Name: "Must Roll Back", RawProfile: []byte(`{}`)}},
			Users: []DirectoryUser{
				{ExternalUserID: "reused_user_id", ExternalOpenID: "stable_open_b", ExternalUnionID: "stable_union_b", Name: "Conflicting User", Status: "active", RawProfile: []byte(`{"user_id":"reused_user_id","open_id":"stable_open_b","union_id":"stable_union_b"}`)},
				// Keep the snapshot size stable so the destructive-shrink guard does not mask
				// the stale-identifier reconciliation this test exercises.
				{ExternalUserID: "new_snapshot_user", ExternalOpenID: "new_snapshot_open", ExternalUnionID: "new_snapshot_union", Name: "Snapshot User", Status: "active", RawProfile: []byte(`{"user_id":"new_snapshot_user","open_id":"new_snapshot_open","union_id":"new_snapshot_union"}`)},
			},
		}},
		TraceID: func() string { return "trace-user-identifier-collision" }, TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	result, err := service.RunFullSync(ctx, FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu"})
	if err != nil {
		t.Fatalf("RunFullSync() error = %v", err)
	}
	if result.DirectoryUsersDeleted != 1 {
		t.Fatalf("DirectoryUsersDeleted = %d, want 1", result.DirectoryUsersDeleted)
	}
	var activeCurrent, deletedStale int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE external_user_id='reused_user_id' AND name='Conflicting User' AND status='active'), count(*) FILTER (WHERE external_user_id='legacy_user_b' AND status='deleted') FROM directory_users WHERE entity_id=$1 AND source_id=$2`, entity.ID, source.ID).Scan(&activeCurrent, &deletedStale); err != nil {
		t.Fatalf("count reconciled users: %v", err)
	}
	if activeCurrent != 1 || deletedStale != 1 {
		t.Fatalf("active current = %d, deleted stale = %d, want 1 each", activeCurrent, deletedStale)
	}
}

func TestFullSyncRejectsDepartmentIdentifiersMatchingDifferentRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Department Identifier Collision", Slug: "department-identifier-collision", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	for _, fixture := range []generated.UpsertDirectoryDepartmentParams{
		{EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "reused_department_id", Name: "Existing Department A", RawProfile: []byte(`{"department_id":"reused_department_id","open_department_id":"open_department_a"}`)},
		{EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "legacy_department_b", Name: "Existing Department B", RawProfile: []byte(`{"department_id":"legacy_department_b","open_department_id":"stable_open_department_b"}`)},
	} {
		if _, err := queries.UpsertDirectoryDepartment(ctx, fixture); err != nil {
			t.Fatalf("seed directory department: %v", err)
		}
	}
	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{Departments: []DirectoryDepartment{
			{
				ExternalDepartmentID: "reused_department_id", Name: "Conflicting Department",
				RawProfile: []byte(`{"department_id":"reused_department_id","open_department_id":"stable_open_department_b"}`),
			},
			// Keep the snapshot size stable so the destructive-shrink guard does not mask
			// the provider-identifier collision this test exercises.
			{ExternalDepartmentID: "new_snapshot_department", Name: "Snapshot Department", RawProfile: []byte(`{"department_id":"new_snapshot_department","open_department_id":"new_snapshot_open_department"}`)},
		}}},
		TraceID: func() string { return "trace-department-identifier-collision" }, TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	_, err = service.RunFullSync(ctx, FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu"})
	if err == nil || !strings.Contains(err.Error(), "multiple existing rows") {
		t.Fatalf("RunFullSync() error = %v, want provider identifier collision", err)
	}
	var unchanged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM directory_departments WHERE entity_id=$1 AND source_id=$2 AND ((external_department_id='reused_department_id' AND name='Existing Department A') OR (external_department_id='legacy_department_b' AND name='Existing Department B'))`, entity.ID, source.ID).Scan(&unchanged); err != nil {
		t.Fatalf("count unchanged departments: %v", err)
	}
	if unchanged != 2 {
		t.Fatalf("unchanged departments = %d, want 2", unchanged)
	}
}

func TestChildOnlyIncrementalSyncPreservesOrganizationParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Incremental Parent Entity", Slug: "incremental-parent", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	org, err := queries.CreateOrganization(ctx, generated.CreateOrganizationParams{EntityID: entity.ID, Name: "Company"})
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	for _, fixture := range []generated.UpsertDirectoryDepartmentParams{
		{EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "parent", Name: "Parent", RawProfile: []byte(`{"department_id":"parent","open_department_id":"od_parent"}`)},
		{EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "child", ParentExternalDepartmentID: textValue("parent"), Name: "Child", RawProfile: []byte(`{"department_id":"child","open_department_id":"od_child","parent_department_id":"parent"}`)},
	} {
		if _, err := queries.UpsertDirectoryDepartment(ctx, fixture); err != nil {
			t.Fatalf("seed directory department: %v", err)
		}
	}
	parent, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{EntityID: entity.ID, OrganizationID: org.ID, Name: "Parent", SourceID: textValue(source.ID), ExternalDepartmentID: textValue("parent")})
	if err != nil {
		t.Fatalf("create organization parent: %v", err)
	}
	child, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{EntityID: entity.ID, OrganizationID: org.ID, Name: "Child", ParentID: textValue(parent.ID), SourceID: textValue(source.ID), ExternalDepartmentID: textValue("child")})
	if err != nil {
		t.Fatalf("create organization child: %v", err)
	}
	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{Departments: []DirectoryDepartment{{
			ExternalDepartmentID: "child", ParentExternalDepartmentID: "parent", Name: "Child Updated",
			RawProfile: []byte(`{"department_id":"child","open_department_id":"od_child","parent_department_id":"parent"}`),
		}}}},
		TraceID: func() string { return "trace-incremental-parent" }, TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	if _, err := service.RunIncrementalSync(ctx, FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu"}); err != nil {
		t.Fatalf("RunIncrementalSync() error = %v", err)
	}
	childAfter, err := queries.GetDepartmentByID(ctx, generated.GetDepartmentByIDParams{EntityID: entity.ID, ID: child.ID})
	if err != nil {
		t.Fatalf("get child after sync: %v", err)
	}
	if !childAfter.ParentID.Valid || childAfter.ParentID.String != parent.ID {
		t.Fatalf("child parent after incremental sync = %#v, want %q", childAfter.ParentID, parent.ID)
	}
}

func TestFullSyncRollsBackWhenUnionIDMatchesMultipleBindings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Binding Union Collision", Slug: "binding-union-collision", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	for _, suffix := range []string{"a", "b"} {
		directoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
			EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "legacy_" + suffix,
			Name: "Legacy " + suffix, Status: "active", RawProfile: []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("create directory user %s: %v", suffix, err)
		}
		managedUser, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
			EntityID: entity.ID, Username: "legacy_" + suffix + "@example.test", DisplayName: "Legacy " + suffix,
			LifecycleStatus: "active", UserType: "employee", PrimarySourceID: textValue(source.ID), Locale: textValue("en-US"),
		})
		if err != nil {
			t.Fatalf("create managed user %s: %v", suffix, err)
		}
		if _, err := queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
			EntityID: entity.ID, UserID: managedUser.ID, SourceID: source.ID, DirectoryUserID: directoryUser.ID,
			ProviderUid: "legacy_" + suffix, ProviderUnionID: textValue("shared_union"), IsPrimary: true,
		}); err != nil {
			t.Fatalf("create account binding %s: %v", suffix, err)
		}
	}
	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{Users: []DirectoryUser{
			{
				ExternalUserID: "new_provider_user", ExternalUnionID: "shared_union", Name: "Ambiguous User", Status: "active",
				RawProfile: []byte(`{"user_id":"new_provider_user","union_id":"shared_union"}`),
			},
			// Keep the snapshot size stable so the destructive-shrink guard does not mask
			// the ambiguous union binding this test exercises.
			{ExternalUserID: "snapshot_padding_user", Name: "Snapshot Padding", Status: "active", RawProfile: []byte(`{"user_id":"snapshot_padding_user"}`)},
		}}},
		TraceID: func() string { return "trace-binding-union-collision" }, TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	_, err = service.RunFullSync(ctx, FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu"})
	if err == nil || !strings.Contains(err.Error(), "multiple account bindings") {
		t.Fatalf("RunFullSync() error = %v, want union binding collision", err)
	}
	var newDirectoryRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM directory_users WHERE entity_id=$1 AND source_id=$2 AND external_user_id='new_provider_user'`, entity.ID, source.ID).Scan(&newDirectoryRows); err != nil {
		t.Fatalf("count rolled-back directory users: %v", err)
	}
	if newDirectoryRows != 0 {
		t.Fatalf("new directory rows = %d, want rollback", newDirectoryRows)
	}
}

func TestIncrementalDeletionRejectsAmbiguousProviderIdentifiers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAtomicSyncTestPool(ctx, t)

	t.Run("user", func(t *testing.T) {
		queries := generated.New(pool)
		entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Ambiguous User Delete", Slug: "ambiguous-user-delete", DefaultLocale: "en-US"})
		if err != nil {
			t.Fatalf("create entity: %v", err)
		}
		source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true})
		if err != nil {
			t.Fatalf("create source: %v", err)
		}
		for _, externalID := range []string{"user_a", "user_b"} {
			if _, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
				EntityID: entity.ID, SourceID: source.ID, ExternalUserID: externalID, ExternalOpenID: textValue("shared_open_delete"),
				Name: externalID, Status: "active", RawProfile: []byte(`{"open_id":"shared_open_delete"}`),
			}); err != nil {
				t.Fatalf("seed directory user: %v", err)
			}
		}
		service, err := NewSyncService(SyncServiceConfig{Queries: queries, Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			UserDeletions: []DirectoryObjectDeletion{{ObjectID: "shared_open_delete", ObjectIDType: "open_id"}},
		}}, TraceID: func() string { return "trace-ambiguous-user-delete" }, TxStarter: pool})
		if err != nil {
			t.Fatalf("new sync service: %v", err)
		}
		_, _, err = service.applyIncrementalDeletions(ctx,
			FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu", SyncType: SyncModeIncremental},
			entity.ID, source.ID, "trace-ambiguous-user-delete", nil,
			[]DirectoryObjectDeletion{{ObjectID: "shared_open_delete", ObjectIDType: "open_id"}}, FullSyncResult{},
		)
		if err == nil || !strings.Contains(err.Error(), "user deletion identifier") {
			t.Fatalf("applyIncrementalDeletions() error = %v, want ambiguous user deletion", err)
		}
		var active int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM directory_users WHERE entity_id=$1 AND source_id=$2 AND status='active'`, entity.ID, source.ID).Scan(&active); err != nil {
			t.Fatalf("count active directory users: %v", err)
		}
		if active != 2 {
			t.Fatalf("active directory users = %d, want 2", active)
		}
	})

	t.Run("department", func(t *testing.T) {
		queries := generated.New(pool)
		entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Ambiguous Department Delete", Slug: "ambiguous-department-delete", DefaultLocale: "en-US"})
		if err != nil {
			t.Fatalf("create entity: %v", err)
		}
		source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true})
		if err != nil {
			t.Fatalf("create source: %v", err)
		}
		for _, externalID := range []string{"department_a", "department_b"} {
			if _, err := queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{
				EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: externalID, Name: externalID,
				RawProfile: []byte(`{"open_department_id":"shared_open_department_delete"}`),
			}); err != nil {
				t.Fatalf("seed directory department: %v", err)
			}
		}
		service, err := NewSyncService(SyncServiceConfig{Queries: queries, Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			DepartmentDeletions: []DirectoryObjectDeletion{{ObjectID: "shared_open_department_delete", ObjectIDType: "open_department_id"}},
		}}, TraceID: func() string { return "trace-ambiguous-department-delete" }, TxStarter: pool})
		if err != nil {
			t.Fatalf("new sync service: %v", err)
		}
		_, _, err = service.applyIncrementalDeletions(ctx,
			FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu", SyncType: SyncModeIncremental},
			entity.ID, source.ID, "trace-ambiguous-department-delete",
			[]DirectoryObjectDeletion{{ObjectID: "shared_open_department_delete", ObjectIDType: "open_department_id"}}, nil, FullSyncResult{},
		)
		if err == nil || !strings.Contains(err.Error(), "department deletion identifier") {
			t.Fatalf("applyIncrementalDeletions() error = %v, want ambiguous department deletion", err)
		}
		var remaining int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM directory_departments WHERE entity_id=$1 AND source_id=$2`, entity.ID, source.ID).Scan(&remaining); err != nil {
			t.Fatalf("count directory departments: %v", err)
		}
		if remaining != 2 {
			t.Fatalf("remaining directory departments = %d, want 2", remaining)
		}
	})
}

func newAtomicSyncTestPool(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	if conn := os.Getenv("OPEN_IDB_TEST_DATABASE_URL"); conn != "" {
		applySyncTestMigrations(ctx, t, conn)
		pool, err := pgxpool.New(ctx, conn)
		if err != nil {
			t.Fatalf("open configured pgx pool: %v", err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
	return newSyncTestPool(ctx, t)
}

func TestFullSyncRejectsEmptySnapshotWithoutMutatingExistingUsers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Sync Archive Entity",
		Slug:          "sync-archive",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	directoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_archive_1",
		Name:           "Archived User",
		Status:         "active",
		RawProfile:     []byte(`{"name":"Archived User"}`),
	})
	if err != nil {
		t.Fatalf("create directory user: %v", err)
	}
	managedUser, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "archived.user@example.test",
		DisplayName:     "Archived User",
		Email:           pgtype.Text{String: "archived.user@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create managed user: %v", err)
	}
	if _, err := queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
		EntityID:        entity.ID,
		UserID:          managedUser.ID,
		SourceID:        source.ID,
		DirectoryUserID: directoryUser.ID,
		ProviderUid:     "ou_archive_1",
		IsPrimary:       true,
	}); err != nil {
		t.Fatalf("create account binding: %v", err)
	}
	if _, err := queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "od_removed", Name: "Removed Department", RawProfile: []byte(`{}`)}); err != nil {
		t.Fatalf("create directory department: %v", err)
	}
	org, err := queries.CreateOrganization(ctx, generated.CreateOrganizationParams{EntityID: entity.ID, Name: "Company"})
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	if _, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{EntityID: entity.ID, OrganizationID: org.ID, Name: "Removed Department", SourceID: pgtype.Text{String: source.ID, Valid: true}, ExternalDepartmentID: pgtype.Text{String: "od_removed", Valid: true}}); err != nil {
		t.Fatalf("create mapped department: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries:   queries,
		Provider:  fakeSyncDirectoryProvider{data: FullSyncData{}},
		Audit:     &capturingAuditWriter{},
		TraceID:   func() string { return "trace-sync-archive" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	auditWriter := service.audit.(*capturingAuditWriter)

	_, err = service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if !errors.Is(err, ErrSuspiciousFullSnapshot) {
		t.Fatalf("RunFullSync() error = %v, want ErrSuspiciousFullSnapshot", err)
	}

	if _, err := queries.GetUserByID(ctx, generated.GetUserByIDParams{EntityID: entity.ID, ID: managedUser.ID}); err != nil {
		t.Fatalf("managed user changed after rejected snapshot: %v", err)
	}
	directoryUserAfter, err := queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalUserID: directoryUser.ExternalUserID,
	})
	if err != nil {
		t.Fatalf("directory user changed after rejected snapshot: %v", err)
	}
	if directoryUserAfter.Status != "active" {
		t.Fatalf("directory user status = %q, want active", directoryUserAfter.Status)
	}
	var archiveCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM archived_users WHERE entity_id = $1`, entity.ID).Scan(&archiveCount); err != nil {
		t.Fatalf("count archived users: %v", err)
	}
	if archiveCount != 0 {
		t.Fatalf("archive count = %d, want 0", archiveCount)
	}
	if len(auditWriter.events) != 2 {
		// A started + failed sync audit is expected; no archive mutation audit is.
		t.Fatalf("audit event count = %d, want 2", len(auditWriter.events))
	}
	for _, event := range auditWriter.events {
		if event.Action == audit.ActionSyncUserArchived {
			t.Fatalf("rejected snapshot emitted archive audit: %#v", event)
		}
	}
}

func TestFullSyncPreservesManagedUserULIDWhenProviderUIDChanges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Sync UID Change Entity",
		Slug:          "sync-uid-change",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	oldDirectoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_old_uid",
		Name:           "Stable User",
		Email:          pgtype.Text{String: "stable.user@example.test", Valid: true},
		Status:         "active",
		RawProfile:     []byte(`{"name":"Stable User"}`),
	})
	if err != nil {
		t.Fatalf("create old directory user: %v", err)
	}
	managedUser, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "stable.user@example.test",
		DisplayName:     "Stable User",
		Email:           pgtype.Text{String: "stable.user@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create managed user: %v", err)
	}
	if _, err := queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
		EntityID:        entity.ID,
		UserID:          managedUser.ID,
		SourceID:        source.ID,
		DirectoryUserID: oldDirectoryUser.ID,
		ProviderUid:     "ou_old_uid",
		IsPrimary:       true,
	}); err != nil {
		t.Fatalf("create old account binding: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{Users: []DirectoryUser{{
			ExternalUserID: "ou_new_uid",
			Name:           "Stable User",
			Email:          "stable.user@example.test",
			Status:         "active",
			RawProfile:     []byte(`{"name":"Stable User"}`),
		}}}},
		Audit:     &capturingAuditWriter{},
		TraceID:   func() string { return "trace-sync-uid-change" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	result, err := service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run full sync: %v", err)
	}
	if result.ManagedUsersDeleted != 0 {
		t.Fatalf("ManagedUsersDeleted = %d, want 0", result.ManagedUsersDeleted)
	}

	gotUser, err := queries.GetUserByID(ctx, generated.GetUserByIDParams{EntityID: entity.ID, ID: managedUser.ID})
	if err != nil {
		t.Fatalf("get managed user by original ULID: %v", err)
	}
	if gotUser.ID != managedUser.ID {
		t.Fatalf("managed user ID = %q, want original ULID %q", gotUser.ID, managedUser.ID)
	}
	newBinding, err := queries.GetAccountBindingByProviderUID(ctx, generated.GetAccountBindingByProviderUIDParams{
		EntityID:    entity.ID,
		SourceID:    source.ID,
		ProviderUid: "ou_new_uid",
	})
	if err != nil {
		t.Fatalf("get new account binding: %v", err)
	}
	if newBinding.UserID != managedUser.ID {
		t.Fatalf("new binding user ID = %q, want original ULID %q", newBinding.UserID, managedUser.ID)
	}
	oldDirectoryUser, err = queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_old_uid",
	})
	if err != nil {
		t.Fatalf("get old directory user: %v", err)
	}
	if oldDirectoryUser.Status != "deleted" {
		t.Fatalf("old directory user status = %q, want deleted", oldDirectoryUser.Status)
	}
}

func TestFullSyncProviderReturnedDeletedUserArchivesWithoutDisabledAudit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Sync Provider Deleted Entity",
		Slug:          "sync-provider-deleted",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	directoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_deleted_1",
		Name:           "Deleted User",
		Status:         "active",
		RawProfile:     []byte(`{"name":"Deleted User"}`),
	})
	if err != nil {
		t.Fatalf("create directory user: %v", err)
	}
	managedUser, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "deleted.user@example.test",
		DisplayName:     "Deleted User",
		Email:           pgtype.Text{String: "deleted.user@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create managed user: %v", err)
	}
	if _, err := queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
		EntityID:        entity.ID,
		UserID:          managedUser.ID,
		SourceID:        source.ID,
		DirectoryUserID: directoryUser.ID,
		ProviderUid:     "ou_deleted_1",
		IsPrimary:       true,
	}); err != nil {
		t.Fatalf("create account binding: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			Users: []DirectoryUser{{
				ExternalUserID: "ou_deleted_1",
				Name:           "Deleted User",
				Email:          "deleted.user@example.test",
				Status:         "deleted",
				RawProfile:     []byte(`{"name":"Deleted User","status":"deleted"}`),
			}},
		}},
		Audit:     &capturingAuditWriter{},
		TraceID:   func() string { return "trace-sync-provider-deleted" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	auditWriter := service.audit.(*capturingAuditWriter)

	result, err := service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run full sync: %v", err)
	}

	if result.ManagedUsersDeleted != 1 {
		t.Fatalf("ManagedUsersDeleted = %d, want 1", result.ManagedUsersDeleted)
	}

	_, err = queries.GetUserByID(ctx, generated.GetUserByIDParams{
		EntityID: entity.ID,
		ID:       managedUser.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected active user row to be deleted, got %v", err)
	}

	archive, err := queries.GetArchivedUserByOriginalID(ctx, generated.GetArchivedUserByOriginalIDParams{
		EntityID:       entity.ID,
		OriginalUserID: managedUser.ID,
	})
	if err != nil {
		t.Fatalf("get archived user: %v", err)
	}
	if archive.Username != managedUser.Username {
		t.Fatalf("archived username = %q, want %q", archive.Username, managedUser.Username)
	}

	foundArchivedAction := false
	for _, event := range auditWriter.events {
		if event.Action == audit.ActionSyncUserDisabled {
			t.Fatalf("unexpected disabled audit action for provider-returned deleted user: %+v", event)
		}
		if event.ResourceType == "archived_user" && event.Action == audit.ActionSyncUserArchived {
			foundArchivedAction = true
		}
	}
	if !foundArchivedAction {
		t.Fatal("expected archived_user audit event with sync.user.archived action")
	}
}

func TestFullSyncProviderReturnedDeletedUserWithoutBindingDoesNotMergeByUsername(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Sync Deleted Merge Guard Entity",
		Slug:          "sync-deleted-merge-guard",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	managedUser, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "preserve.user@example.test",
		DisplayName:     "Preserve Me",
		Email:           pgtype.Text{String: "preserve.user@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	})
	if err != nil {
		t.Fatalf("create managed user: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			Users: []DirectoryUser{{
				ExternalUserID: "ou_deleted_unbound_match",
				Name:           "Directory Deleted Name",
				Email:          managedUser.Username,
				Status:         "deleted",
				RawProfile:     []byte(`{"name":"Directory Deleted Name","status":"deleted"}`),
			}},
		}},
		Audit:     &capturingAuditWriter{},
		TraceID:   func() string { return "trace-sync-deleted-merge-guard" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	result, err := service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run full sync: %v", err)
	}

	if result.ManagedUsersUpdated != 0 {
		t.Fatalf("ManagedUsersUpdated = %d, want 0", result.ManagedUsersUpdated)
	}
	if result.ManagedUsersCreated != 0 {
		t.Fatalf("ManagedUsersCreated = %d, want 0", result.ManagedUsersCreated)
	}
	if result.ManagedUsersDeleted != 0 {
		t.Fatalf("ManagedUsersDeleted = %d, want 0", result.ManagedUsersDeleted)
	}
	if result.BindingsCreated != 0 {
		t.Fatalf("BindingsCreated = %d, want 0", result.BindingsCreated)
	}

	gotUser, err := queries.GetUserByID(ctx, generated.GetUserByIDParams{
		EntityID: entity.ID,
		ID:       managedUser.ID,
	})
	if err != nil {
		t.Fatalf("get managed user: %v", err)
	}
	if gotUser.DisplayName != managedUser.DisplayName {
		t.Fatalf("display_name = %q, want %q", gotUser.DisplayName, managedUser.DisplayName)
	}
	if gotUser.LifecycleStatus != "active" {
		t.Fatalf("lifecycle_status = %q, want active", gotUser.LifecycleStatus)
	}

	bindings, err := queries.ListAccountBindingsByUser(ctx, generated.ListAccountBindingsByUserParams{
		EntityID: entity.ID,
		UserID:   managedUser.ID,
	})
	if err != nil {
		t.Fatalf("list account bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("account bindings = %d, want 0", len(bindings))
	}

	_, err = queries.GetArchivedUserByOriginalID(ctx, generated.GetArchivedUserByOriginalIDParams{
		EntityID:       entity.ID,
		OriginalUserID: managedUser.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected no archived user row, got %v", err)
	}

	directoryUser, err := queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_deleted_unbound_match",
	})
	if err != nil {
		t.Fatalf("get directory user: %v", err)
	}
	if directoryUser.Status != "deleted" {
		t.Fatalf("directory user status = %q, want deleted", directoryUser.Status)
	}
}

func TestFullSyncProviderReturnedDeletedUserWithoutBindingDoesNotCreateManagedUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Sync Deleted Create Guard Entity",
		Slug:          "sync-deleted-create-guard",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "Feishu",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			Users: []DirectoryUser{{
				ExternalUserID: "ou_deleted_unbound_new",
				Name:           "Deleted User",
				Email:          "deleted.unbound.new@example.test",
				Status:         "deleted",
				RawProfile:     []byte(`{"name":"Deleted User","status":"deleted"}`),
			}},
		}},
		Audit:     &capturingAuditWriter{},
		TraceID:   func() string { return "trace-sync-deleted-create-guard" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	result, err := service.RunFullSync(ctx, FullSyncInput{
		EntityID: entity.ID,
		SourceID: source.ID,
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run full sync: %v", err)
	}

	if result.ManagedUsersCreated != 0 {
		t.Fatalf("ManagedUsersCreated = %d, want 0", result.ManagedUsersCreated)
	}
	if result.ManagedUsersDeleted != 0 {
		t.Fatalf("ManagedUsersDeleted = %d, want 0", result.ManagedUsersDeleted)
	}
	if result.BindingsCreated != 0 {
		t.Fatalf("BindingsCreated = %d, want 0", result.BindingsCreated)
	}

	_, err = queries.GetManagedUserByUsername(ctx, generated.GetManagedUserByUsernameParams{
		EntityID: entity.ID,
		Username: "deleted.unbound.new@example.test",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected no managed user row, got %v", err)
	}

	_, err = queries.GetAccountBindingByProviderUID(ctx, generated.GetAccountBindingByProviderUIDParams{
		EntityID:    entity.ID,
		SourceID:    source.ID,
		ProviderUid: "ou_deleted_unbound_new",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected no account binding row, got %v", err)
	}

	directoryUser, err := queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "ou_deleted_unbound_new",
	})
	if err != nil {
		t.Fatalf("get directory user: %v", err)
	}
	if directoryUser.Status != "deleted" {
		t.Fatalf("directory user status = %q, want deleted", directoryUser.Status)
	}
}

func TestIncrementalSyncMatchesProviderIdentifiersWithoutChangingExistingULIDs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAtomicSyncTestPool(ctx, t)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name: "Incremental Identity Compatibility", Slug: "incremental-identity-compatibility", DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID: entity.ID, Type: "feishu", Name: "Feishu", SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}

	type identityFixture struct {
		directory generated.DirectoryUser
		managed   generated.User
	}
	createIdentity := func(name, userID, openID, unionID string) identityFixture {
		t.Helper()
		directoryUser, createErr := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
			EntityID: entity.ID, SourceID: source.ID, ExternalUserID: userID,
			ExternalOpenID: textValue(openID), ExternalUnionID: textValue(unionID),
			Name: name, Email: textValue(userID + "@example.test"), Status: "active",
			RawProfile: []byte(`{"user_id":"` + userID + `","open_id":"` + openID + `","union_id":"` + unionID + `"}`),
		})
		if createErr != nil {
			t.Fatalf("create directory user %s: %v", name, createErr)
		}
		managedUser, createErr := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
			EntityID: entity.ID, Username: userID + "@example.test", DisplayName: name,
			Email: textValue(userID + "@example.test"), LifecycleStatus: "active", UserType: "employee",
			PrimarySourceID: textValue(source.ID), Locale: textValue("en-US"),
		})
		if createErr != nil {
			t.Fatalf("create managed user %s: %v", name, createErr)
		}
		if _, createErr = queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
			EntityID: entity.ID, UserID: managedUser.ID, SourceID: source.ID,
			DirectoryUserID: directoryUser.ID, ProviderUid: userID,
			ProviderUnionID: textValue(unionID), IsPrimary: true,
		}); createErr != nil {
			t.Fatalf("create account binding %s: %v", name, createErr)
		}
		return identityFixture{directory: directoryUser, managed: managedUser}
	}

	deletedByUserID := createIdentity("Delete by user_id", "uid_delete_user", "open_delete_user", "union_delete_user")
	deletedByOpenID := createIdentity("Delete by open_id", "uid_delete_open", "open_delete_open", "union_delete_open")
	deletedByUnionID := createIdentity("Delete by union_id", "uid_delete_union", "open_delete_union", "union_delete_union")

	// This row predates user_id availability: its canonical/provider UID is the
	// open_id. The incremental payload below upgrades it to user_id.
	upgraded := createIdentity("Upgrade Identity", "open_upgrade", "open_upgrade", "union_upgrade")
	if _, err := pool.Exec(ctx, `UPDATE directory_users SET raw_profile = '{"open_id":"open_upgrade","union_id":"union_upgrade"}'::jsonb WHERE id = $1`, upgraded.directory.ID); err != nil {
		t.Fatalf("remove legacy user_id from raw profile: %v", err)
	}

	org, err := queries.CreateOrganization(ctx, generated.CreateOrganizationParams{EntityID: entity.ID, Name: "Company"})
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	updatedDirectoryDepartment, err := queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "legacy_custom_department",
		Name: "Legacy Department", RawProfile: []byte(`{"department_id":"legacy_custom_department","open_department_id":"od_stable_update"}`),
	})
	if err != nil {
		t.Fatalf("create updated directory department: %v", err)
	}
	updatedOrganizationDepartment, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{
		EntityID: entity.ID, OrganizationID: org.ID, Name: "Legacy Department",
		SourceID: textValue(source.ID), ExternalDepartmentID: textValue("legacy_custom_department"),
	})
	if err != nil {
		t.Fatalf("create updated organization department: %v", err)
	}
	departmentMember, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "uid_department_member",
		Name: "Department Member", Status: "active",
		RawProfile: []byte(`{"user_id":"uid_department_member","department_ids":["new_custom_department"]}`),
	})
	if err != nil {
		t.Fatalf("create department member: %v", err)
	}
	deletedDirectoryDepartment, err := queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "custom_department_delete",
		Name: "Deleted Department", RawProfile: []byte(`{"department_id":"custom_department_delete","open_department_id":"od_stable_delete"}`),
	})
	if err != nil {
		t.Fatalf("create deleted directory department: %v", err)
	}
	deletedOrganizationDepartment, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{
		EntityID: entity.ID, OrganizationID: org.ID, Name: "Deleted Department",
		SourceID: textValue(source.ID), ExternalDepartmentID: textValue("custom_department_delete"),
	})
	if err != nil {
		t.Fatalf("create deleted organization department: %v", err)
	}
	legacyParentDirectory, err := queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "legacy_parent",
		Name: "Legacy Parent", RawProfile: []byte(`{"department_id":"legacy_parent","open_department_id":"od_parent_stable"}`),
	})
	if err != nil {
		t.Fatalf("create legacy parent directory department: %v", err)
	}
	legacyChildDirectory, err := queries.UpsertDirectoryDepartment(ctx, generated.UpsertDirectoryDepartmentParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalDepartmentID: "legacy_child",
		ParentExternalDepartmentID: textValue("legacy_parent"), Name: "Legacy Child",
		RawProfile: []byte(`{"department_id":"legacy_child","open_department_id":"od_child_stable"}`),
	})
	if err != nil {
		t.Fatalf("create legacy child directory department: %v", err)
	}
	legacyParentOrganization, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{
		EntityID: entity.ID, OrganizationID: org.ID, Name: "Legacy Parent",
		SourceID: textValue(source.ID), ExternalDepartmentID: textValue("legacy_parent"),
	})
	if err != nil {
		t.Fatalf("create legacy parent organization department: %v", err)
	}
	legacyChildOrganization, err := queries.UpsertDepartmentBySource(ctx, generated.UpsertDepartmentBySourceParams{
		EntityID: entity.ID, OrganizationID: org.ID, Name: "Legacy Child",
		SourceID: textValue(source.ID), ExternalDepartmentID: textValue("legacy_child"),
	})
	if err != nil {
		t.Fatalf("create legacy child organization department: %v", err)
	}
	if _, err := queries.UpdateDepartment(ctx, generated.UpdateDepartmentParams{
		EntityID: entity.ID, ID: legacyChildOrganization.ID, Name: textValue("Legacy Child"), ParentID: textValue(legacyParentOrganization.ID),
	}); err != nil {
		t.Fatalf("link legacy organization departments: %v", err)
	}

	service, err := NewSyncService(SyncServiceConfig{
		Queries: queries,
		Provider: fakeSyncDirectoryProvider{data: FullSyncData{
			Users: []DirectoryUser{{
				ExternalUserID: "uid_upgrade", ExternalOpenID: "open_upgrade", ExternalUnionID: "union_upgrade",
				Name: "Upgrade Identity Updated", Email: "upgraded-email@example.test", Status: "active",
				RawProfile: []byte(`{"user_id":"uid_upgrade","open_id":"open_upgrade","union_id":"union_upgrade"}`),
			}},
			Departments: []DirectoryDepartment{
				{
					ExternalDepartmentID: "new_custom_department", Name: "Department Updated",
					RawProfile: []byte(`{"department_id":"new_custom_department","open_department_id":"od_stable_update"}`),
				},
				{
					ExternalDepartmentID: "new_parent", Name: "Parent Updated",
					RawProfile: []byte(`{"department_id":"new_parent","open_department_id":"od_parent_stable"}`),
				},
				{
					ExternalDepartmentID: "new_child", ParentExternalDepartmentID: "new_parent", Name: "Child Updated",
					RawProfile: []byte(`{"department_id":"new_child","open_department_id":"od_child_stable"}`),
				},
			},
			UserDeletions: []DirectoryObjectDeletion{
				{ObjectID: "uid_delete_user", ObjectIDType: "user_id"},
				{ObjectID: "open_delete_open", ObjectIDType: "open_id"},
				{ObjectID: "union_delete_union", ObjectIDType: "union_id"},
				{ObjectID: "open_unknown", ObjectIDType: "open_id"},
			},
			DepartmentDeletions: []DirectoryObjectDeletion{{ObjectID: "od_stable_delete", ObjectIDType: "open_department_id"}},
		}},
		Audit: &capturingAuditWriter{}, TraceID: func() string { return "trace-incremental-identity-compatibility" }, TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}
	if _, err := service.SubmitWebhookEvent(ctx, entity.ID, source.ID, DirectorySyncEvent{
		EventType: "user.updated", ObjectType: "user", ObjectID: "uid_upgrade", ObjectIDType: "user_id", EventID: "event-incremental-compatibility",
	}); err != nil {
		t.Fatalf("submit webhook event: %v", err)
	}

	result, err := service.RunIncrementalSync(ctx, FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu"})
	if err != nil {
		t.Fatalf("run incremental sync: %v", err)
	}
	if result.DirectoryUsersDeleted != 3 || result.ManagedUsersDeleted != 3 || result.DepartmentsDeleted != 1 {
		t.Fatalf("unexpected deletion result: %#v", result)
	}

	upgradedDirectory, err := queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
		EntityID: entity.ID, SourceID: source.ID, ExternalUserID: "uid_upgrade",
	})
	if err != nil {
		t.Fatalf("get upgraded directory user: %v", err)
	}
	if upgradedDirectory.ID != upgraded.directory.ID {
		t.Fatalf("upgraded directory ULID = %q, want %q", upgradedDirectory.ID, upgraded.directory.ID)
	}
	if _, err := queries.GetUserByID(ctx, generated.GetUserByIDParams{EntityID: entity.ID, ID: upgraded.managed.ID}); err != nil {
		t.Fatalf("get upgraded managed user by original ULID: %v", err)
	}
	upgradedBinding, err := queries.GetAccountBindingByProviderUID(ctx, generated.GetAccountBindingByProviderUIDParams{
		EntityID: entity.ID, SourceID: source.ID, ProviderUid: "uid_upgrade",
	})
	if err != nil {
		t.Fatalf("get upgraded account binding: %v", err)
	}
	if upgradedBinding.UserID != upgraded.managed.ID || upgradedBinding.DirectoryUserID != upgraded.directory.ID {
		t.Fatalf("upgraded binding changed ULIDs: %#v", upgradedBinding)
	}

	for _, fixture := range []identityFixture{deletedByUserID, deletedByOpenID, deletedByUnionID} {
		directoryUser, getErr := queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
			EntityID: entity.ID, SourceID: source.ID, ExternalUserID: fixture.directory.ExternalUserID,
		})
		if getErr != nil {
			t.Fatalf("get deleted directory user %s: %v", fixture.directory.ExternalUserID, getErr)
		}
		if directoryUser.Status != "deleted" {
			t.Fatalf("directory user %s status = %q, want deleted", fixture.directory.ExternalUserID, directoryUser.Status)
		}
		if _, getErr = queries.GetUserByID(ctx, generated.GetUserByIDParams{EntityID: entity.ID, ID: fixture.managed.ID}); !errors.Is(getErr, pgx.ErrNoRows) {
			t.Fatalf("active managed user %s still exists: %v", fixture.managed.ID, getErr)
		}
		archive, getErr := queries.GetArchivedUserByOriginalID(ctx, generated.GetArchivedUserByOriginalIDParams{
			EntityID: entity.ID, OriginalUserID: fixture.managed.ID,
		})
		if getErr != nil {
			t.Fatalf("get archived user %s: %v", fixture.managed.ID, getErr)
		}
		if archive.OriginalUserID != fixture.managed.ID {
			t.Fatalf("archived original ULID = %q, want %q", archive.OriginalUserID, fixture.managed.ID)
		}
	}

	var tombstones int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM directory_users
		WHERE entity_id = $1 AND source_id = $2
		  AND external_user_id = ANY($3::text[])
	`, entity.ID, source.ID, []string{"open_delete_open", "union_delete_union", "open_unknown"}).Scan(&tombstones); err != nil {
		t.Fatalf("count wrong identifier tombstones: %v", err)
	}
	if tombstones != 0 {
		t.Fatalf("wrong identifier tombstones = %d, want 0", tombstones)
	}

	updatedDepartment, err := queries.GetDirectoryDepartmentByProviderIdentifier(ctx, generated.GetDirectoryDepartmentByProviderIdentifierParams{
		EntityID: entity.ID, SourceID: source.ID, Identifier: "od_stable_update", IdentifierType: "open_department_id",
	})
	if err != nil {
		t.Fatalf("get updated directory department: %v", err)
	}
	if updatedDepartment.ID != updatedDirectoryDepartment.ID || updatedDepartment.ExternalDepartmentID != "legacy_custom_department" {
		t.Fatalf("updated directory department changed identity: %#v", updatedDepartment)
	}
	var updatedOrganizationID string
	if err := pool.QueryRow(ctx, `SELECT id FROM departments WHERE entity_id = $1 AND source_id = $2 AND external_department_id = $3`,
		entity.ID, source.ID, "legacy_custom_department").Scan(&updatedOrganizationID); err != nil {
		t.Fatalf("get updated organization department: %v", err)
	}
	if updatedOrganizationID != updatedOrganizationDepartment.ID {
		t.Fatalf("updated organization department ULID = %q, want %q", updatedOrganizationID, updatedOrganizationDepartment.ID)
	}
	departmentMembers, err := queries.ListDirectoryUsersByDepartmentExternalID(ctx, generated.ListDirectoryUsersByDepartmentExternalIDParams{
		EntityID: entity.ID, SourceID: source.ID, Column3: "legacy_custom_department", Limit: 100, Offset: 0,
	})
	if err != nil {
		t.Fatalf("list members through preserved department identity: %v", err)
	}
	if len(departmentMembers) != 1 || departmentMembers[0].ID != departmentMember.ID {
		t.Fatalf("members after department ID alias update = %#v, want directory user %s", departmentMembers, departmentMember.ID)
	}
	parentAfter, err := queries.GetDirectoryDepartmentByProviderIdentifier(ctx, generated.GetDirectoryDepartmentByProviderIdentifierParams{
		EntityID: entity.ID, SourceID: source.ID, Identifier: "od_parent_stable", IdentifierType: "open_department_id",
	})
	if err != nil {
		t.Fatalf("get parent after alias update: %v", err)
	}
	childAfter, err := queries.GetDirectoryDepartmentByProviderIdentifier(ctx, generated.GetDirectoryDepartmentByProviderIdentifierParams{
		EntityID: entity.ID, SourceID: source.ID, Identifier: "od_child_stable", IdentifierType: "open_department_id",
	})
	if err != nil {
		t.Fatalf("get child after alias update: %v", err)
	}
	if parentAfter.ID != legacyParentDirectory.ID || childAfter.ID != legacyChildDirectory.ID ||
		childAfter.ParentExternalDepartmentID.String != "legacy_parent" {
		t.Fatalf("department hierarchy identity changed: parent=%#v child=%#v", parentAfter, childAfter)
	}
	var childOrganizationID string
	var childParentID pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT id, parent_id FROM departments WHERE entity_id = $1 AND source_id = $2 AND external_department_id = 'legacy_child'`,
		entity.ID, source.ID).Scan(&childOrganizationID, &childParentID); err != nil {
		t.Fatalf("get child organization after alias update: %v", err)
	}
	if childOrganizationID != legacyChildOrganization.ID || !childParentID.Valid || childParentID.String != legacyParentOrganization.ID {
		t.Fatalf("organization hierarchy changed: child=%q parent=%#v", childOrganizationID, childParentID)
	}
	var deletedDepartmentRows int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM directory_departments WHERE id = $1) +
		       (SELECT count(*) FROM departments WHERE id = $2)
	`, deletedDirectoryDepartment.ID, deletedOrganizationDepartment.ID).Scan(&deletedDepartmentRows); err != nil {
		t.Fatalf("count deleted department rows: %v", err)
	}
	if deletedDepartmentRows != 0 {
		t.Fatalf("deleted department rows = %d, want 0", deletedDepartmentRows)
	}

	if _, err := service.SubmitWebhookEvent(ctx, entity.ID, source.ID, DirectorySyncEvent{
		EventType: "user.updated", ObjectType: "user", ObjectID: "uid_upgrade", ObjectIDType: "user_id", EventID: "event-incremental-compatibility-repeat",
	}); err != nil {
		t.Fatalf("submit repeated webhook event: %v", err)
	}
	repeatedResult, err := service.RunIncrementalSync(ctx, FullSyncInput{EntityID: entity.ID, SourceID: source.ID, Provider: "feishu"})
	if err != nil {
		t.Fatalf("run repeated incremental sync: %v", err)
	}
	if repeatedResult.DirectoryUsersDeleted != 0 || repeatedResult.ManagedUsersDeleted != 0 || repeatedResult.DepartmentsDeleted != 0 {
		t.Fatalf("repeated deletion was not idempotent: %#v", repeatedResult)
	}
	var archiveCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM archived_users WHERE entity_id = $1`, entity.ID).Scan(&archiveCount); err != nil {
		t.Fatalf("count archives after repeated sync: %v", err)
	}
	if archiveCount != 3 {
		t.Fatalf("archives after repeated sync = %d, want 3", archiveCount)
	}
}

type fakeSyncDirectoryProvider struct {
	data FullSyncData
	err  error
}

func TestSuspiciousSnapshotShrink(t *testing.T) {
	tests := []struct {
		name              string
		current, incoming int64
		wantSuspicious    bool
	}{
		{name: "empty with current rows", current: 1, incoming: 0, wantSuspicious: true},
		{name: "large shrink", current: 100, incoming: 40, wantSuspicious: true},
		{name: "exact half is destructive", current: 100, incoming: 50, wantSuspicious: true},
		{name: "large ordinary churn remains bounded", current: 100, incoming: 51},
		{name: "small directory truncation", current: 10, incoming: 4, wantSuspicious: true},
		{name: "small directory one-row shrink", current: 10, incoming: 9, wantSuspicious: true},
		{name: "small unchanged snapshot", current: 10, incoming: 10},
		{name: "growth", current: 10, incoming: 11},
		{name: "greenfield empty snapshot", current: 0, incoming: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := suspiciousSnapshotShrink("users", test.current, test.incoming)
			if (got != "") != test.wantSuspicious {
				t.Fatalf("suspiciousSnapshotShrink() = %q, want suspicious=%v", got, test.wantSuspicious)
			}
		})
	}
}

type capturingAuditWriter struct {
	events []audit.Event
}

func (w *capturingAuditWriter) Write(_ context.Context, event audit.Event) error {
	w.events = append(w.events, event)
	return nil
}

func (f fakeSyncDirectoryProvider) FullSync(context.Context) (FullSyncData, error) {
	return f.data, f.err
}

func (f fakeSyncDirectoryProvider) IncrementalSync(context.Context, []DirectorySyncEvent) (FullSyncData, error) {
	return f.data, f.err
}

func newSyncTestPool(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	if conn := os.Getenv("OPEN_IDB_TEST_DATABASE_URL"); conn != "" {
		applySyncTestMigrations(ctx, t, conn)
		pool, err := pgxpool.New(ctx, conn)
		if err != nil {
			t.Fatalf("open configured pgx pool: %v", err)
		}
		t.Cleanup(pool.Close)
		return pool
	}

	testcontainers.SkipIfProviderIsNotHealthy(t)
	container, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("idbridge"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := container.Terminate(cleanupCtx); err != nil {
			t.Errorf("terminate postgres container: %v", err)
		}
	})

	conn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	applySyncTestMigrations(ctx, t, conn)

	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func applySyncTestMigrations(ctx context.Context, t *testing.T, conn string) {
	t.Helper()

	db, err := sql.Open("pgx", conn)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close sql db: %v", err)
		}
	})
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping sql db: %v", err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}
