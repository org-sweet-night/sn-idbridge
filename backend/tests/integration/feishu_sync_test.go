// SPDX-License-Identifier: MIT

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/smices/open-idb/internal/audit"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/smices/open-idb/internal/idp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestFeishuFullSyncCreatesAndUpdatesIdentityRecords(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

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
	applyMigrations(ctx, t, conn)

	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Feishu Entity",
		Slug:          "feishu",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "飞书",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	provider := &fakeDirectoryProvider{data: idp.FullSyncData{
		Departments: []idp.DirectoryDepartment{
			{
				ExternalDepartmentID:       "od-1",
				ParentExternalDepartmentID: "0",
				Name:                       "研发中心",
				RawProfile:                 []byte(`{"name":"研发中心"}`),
			},
		},
		Users: []idp.DirectoryUser{
			{
				ExternalUserID:  "ou_1",
				ExternalUnionID: "on_1",
				ExternalOpenID:  "open_1",
				Name:            "张三",
				Email:           "zhangsan@example.test",
				Phone:           "13800000000",
				Status:          "active",
				RawProfile:      []byte(`{"name":"张三","title":"运营"}`),
			},
		},
	}}
	service, err := idp.NewSyncService(idp.SyncServiceConfig{
		Queries:   queries,
		Provider:  provider,
		TraceID:   func() string { return "trace-feishu-sync" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	result, err := service.RunFullSync(ctx, idp.FullSyncInput{
		EntityID: pgULIDString(entity.ID),
		SourceID: pgULIDString(source.ID),
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run sync: %v", err)
	}
	if result.DepartmentsUpserted != 1 || result.UsersUpserted != 1 || result.ManagedUsersCreated != 1 || result.BindingsCreated != 1 {
		t.Fatalf("result = %#v", result)
	}

	assertSingleString(t, ctx, pool, "select name from directory_departments where entity_id=$1 and source_id=$2", "研发中心", entity.ID, source.ID)
	assertSingleString(t, ctx, pool, "select name from directory_users where entity_id=$1 and source_id=$2", "张三", entity.ID, source.ID)
	assertSingleString(t, ctx, pool, "select display_name from users where entity_id=$1", "张三", entity.ID)
	assertSingleString(t, ctx, pool, "select status from sync_jobs where entity_id=$1 and source_id=$2 order by started_at desc limit 1", "succeeded", entity.ID, source.ID)
	assertSingleInt(t, ctx, pool, "select count(*) from account_bindings where entity_id=$1 and source_id=$2", 1, entity.ID, source.ID)

	provider.data.Users[0].Name = "张三-更新"
	provider.data.Users[0].Status = "disabled"
	if _, err := service.SubmitWebhookEvent(ctx, pgULIDString(entity.ID), pgULIDString(source.ID), idp.DirectorySyncEvent{
		EventType:  "user.updated",
		ObjectType: "user",
		ObjectID:   provider.data.Users[0].ExternalUserID,
	}); err != nil {
		t.Fatalf("submit webhook event: %v", err)
	}
	second, err := service.RunIncrementalSync(ctx, idp.FullSyncInput{
		EntityID: pgULIDString(entity.ID),
		SourceID: pgULIDString(source.ID),
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run second incremental sync: %v", err)
	}
	if second.ManagedUsersCreated != 0 || second.ManagedUsersUpdated != 1 || second.BindingsCreated != 0 {
		t.Fatalf("second result = %#v", second)
	}
	assertSingleString(t, ctx, pool, "select display_name from users where entity_id=$1", "张三-更新", entity.ID)
	assertSingleString(t, ctx, pool, "select lifecycle_status from users where entity_id=$1", "disabled", entity.ID)
}

func TestFeishuFullSyncMergesExistingUserAndArchivesMissing(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

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
	applyMigrations(ctx, t, conn)

	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	queries := generated.New(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Merge Entity",
		Slug:          "merge",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "feishu",
		Name:        "飞书",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	existing, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "merge@example.test",
		DisplayName:     "旧用户",
		LifecycleStatus: "active",
		UserType:        "employee",
		Locale:          pgtype.Text{String: "zh-CN", Valid: true},
	})
	if err != nil {
		t.Fatalf("create existing user: %v", err)
	}

	provider := &fakeDirectoryProvider{data: idp.FullSyncData{
		Users: []idp.DirectoryUser{
			{
				ExternalUserID:  "ou_merge",
				ExternalUnionID: "on_merge",
				ExternalOpenID:  "open_merge",
				Name:            "飞书用户",
				Email:           "merge@example.test",
				Status:          "active",
				RawProfile:      []byte(`{"name":"飞书用户"}`),
			},
			{
				ExternalUserID:  "ou_merge",
				ExternalUnionID: "on_merge",
				ExternalOpenID:  "open_merge",
				Name:            "飞书用户",
				Email:           "merge@example.test",
				Status:          "active",
				RawProfile:      []byte(`{"name":"飞书用户","duplicate":true}`),
			},
		},
	}}
	service, err := idp.NewSyncService(idp.SyncServiceConfig{
		Queries:   queries,
		Provider:  provider,
		Audit:     audit.NewService(queries),
		TraceID:   func() string { return "trace-merge-sync" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	first, err := service.RunFullSync(ctx, idp.FullSyncInput{
		EntityID: pgULIDString(entity.ID),
		SourceID: pgULIDString(source.ID),
		Provider: "feishu",
	})
	if err != nil {
		t.Fatalf("run first sync: %v", err)
	}
	if first.UsersUpserted != 1 || first.ManagedUsersCreated != 0 || first.ManagedUsersUpdated != 1 || first.BindingsCreated != 1 {
		t.Fatalf("first result = %#v", first)
	}
	assertSingleString(t, ctx, pool, "select id from users where entity_id=$1", existing.ID, entity.ID)
	assertSingleString(t, ctx, pool, "select display_name from users where entity_id=$1", "飞书用户", entity.ID)
	assertSingleString(t, ctx, pool, "select provider_uid from account_bindings where entity_id=$1 and source_id=$2", "ou_merge", entity.ID, source.ID)

	provider.data.Users = nil
	_, err = service.RunFullSync(ctx, idp.FullSyncInput{
		EntityID: pgULIDString(entity.ID),
		SourceID: pgULIDString(source.ID),
		Provider: "feishu",
	})
	var snapshotErr *idp.SuspiciousFullSnapshotError
	if !errors.As(err, &snapshotErr) {
		t.Fatalf("run second sync without confirmation: %v, want suspicious snapshot error", err)
	}
	second, err := service.RunFullSync(ctx, idp.FullSyncInput{
		EntityID: pgULIDString(entity.ID),
		SourceID: pgULIDString(source.ID),
		Provider: "feishu",
		DestructiveConfirmation: &idp.DestructiveSnapshotConfirmation{
			SnapshotFingerprint: snapshotErr.SnapshotFingerprint,
			CurrentUsers:        snapshotErr.CurrentUsers,
			CurrentDepartments:  snapshotErr.CurrentDepartments,
			ConfirmedBy:         pgULIDString(existing.ID),
		},
	})
	if err != nil {
		t.Fatalf("run confirmed second sync: %v", err)
	}
	if second.DirectoryUsersDeleted != 1 || second.ManagedUsersDeleted != 1 {
		t.Fatalf("second result = %#v", second)
	}
	assertSingleString(t, ctx, pool, "select status from directory_users where entity_id=$1 and source_id=$2", "deleted", entity.ID, source.ID)
	assertSingleInt(t, ctx, pool, "select count(*) from users where entity_id=$1", 0, entity.ID)
	assertSingleString(t, ctx, pool, "select original_user_id from archived_users where entity_id=$1", existing.ID, entity.ID)
	assertSingleString(t, ctx, pool, "select action from audit_logs where entity_id=$1 and action=$2 order by created_at desc limit 1", audit.ActionSyncDestructiveSnapshotConfirmed, entity.ID, audit.ActionSyncDestructiveSnapshotConfirmed)
}

func TestFeishuFullSyncMarksJobFailedOnProviderError(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
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
	applyMigrations(ctx, t, conn)
	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)
	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Fail Entity", Slug: "fail", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{EntityID: entity.ID, Type: "feishu", Name: "飞书", SyncEnabled: true})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	service, err := idp.NewSyncService(idp.SyncServiceConfig{
		Queries:   queries,
		Provider:  fakeDirectoryProvider{err: errProviderDown{}},
		TraceID:   func() string { return "trace-failed" },
		TxStarter: pool,
	})
	if err != nil {
		t.Fatalf("new sync service: %v", err)
	}

	_, err = service.RunFullSync(ctx, idp.FullSyncInput{EntityID: pgULIDString(entity.ID), SourceID: pgULIDString(source.ID), Provider: "feishu"})
	if err == nil {
		t.Fatal("RunFullSync() error = nil, want provider error")
	}
	assertSingleString(t, ctx, pool, "select status from sync_jobs where entity_id=$1 and source_id=$2 order by started_at desc limit 1", "failed", entity.ID, source.ID)
	var message string
	if err := pool.QueryRow(ctx, "select error_message from sync_jobs where entity_id=$1 and source_id=$2 order by started_at desc limit 1", entity.ID, source.ID).Scan(&message); err != nil {
		t.Fatalf("query error message: %v", err)
	}
	if !strings.Contains(message, "provider down") {
		t.Fatalf("error message = %q", message)
	}
}

type fakeDirectoryProvider struct {
	data idp.FullSyncData
	err  error
}

func (f fakeDirectoryProvider) FullSync(context.Context) (idp.FullSyncData, error) {
	return f.data, f.err
}

func (f fakeDirectoryProvider) IncrementalSync(context.Context, []idp.DirectorySyncEvent) (idp.FullSyncData, error) {
	return f.data, f.err
}

type errProviderDown struct{}

func (errProviderDown) Error() string {
	return "provider down"
}

func assertSingleString(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, want string, args ...interface{}) {
	t.Helper()
	var got string
	if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q = %q, want %q", query, got, want)
	}
}

func assertSingleInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, want int, args ...interface{}) {
	t.Helper()
	var got int
	if err := pool.QueryRow(ctx, query, args...).Scan(&got); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q = %d, want %d", query, got, want)
	}
}
