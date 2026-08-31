// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestArchiveUserWritesArchiveAndDeletesActiveRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool := newAdminServiceTestPool(ctx, t)
	queries := generated.New(pool)
	svc, err := NewAdminService(queries)
	if err != nil {
		t.Fatalf("new admin service: %v", err)
	}
	svc.SetTxStarter(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          "Acme Identity",
		Slug:          "acme-archive",
		DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	source, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entity.ID,
		Type:        "local",
		Name:        "Primary directory",
		SyncEnabled: true,
	})
	if err != nil {
		t.Fatalf("create identity source: %v", err)
	}
	directoryUser, err := queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
		EntityID:       entity.ID,
		SourceID:       source.ID,
		ExternalUserID: "external-ada",
		Name:           "Ada Lovelace",
		Status:         "active",
		RawProfile:     []byte(`{"department":"engineering"}`),
	})
	if err != nil {
		t.Fatalf("create directory user: %v", err)
	}
	user, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "ada@example.test",
		DisplayName:     "Ada Lovelace",
		Email:           pgtype.Text{String: "ada@example.test", Valid: true},
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
		UserID:          user.ID,
		SourceID:        source.ID,
		DirectoryUserID: directoryUser.ID,
		ProviderUid:     "ada@example.test",
		IsPrimary:       true,
	}); err != nil {
		t.Fatalf("create account binding: %v", err)
	}
	role, err := queries.CreateRole(ctx, generated.CreateRoleParams{
		EntityID: entity.ID,
		Name:     "Employee",
		Code:     "employee",
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := queries.AssignRoleToUser(ctx, generated.AssignRoleToUserParams{
		EntityID: entity.ID,
		UserID:   user.ID,
		RoleID:   role.ID,
	}); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	archive, err := svc.ArchiveUser(ctx, entity.ID, user.ID, "", "admin deleted user")
	if err != nil {
		t.Fatalf("archive user: %v", err)
	}

	if archive.OriginalUserID != user.ID {
		t.Fatalf("expected original user id %s, got %s", user.ID, archive.OriginalUserID)
	}
	if archive.Username != "ada@example.test" {
		t.Fatalf("expected username ada@example.test, got %s", archive.Username)
	}

	_, err = queries.GetUserByID(ctx, generated.GetUserByIDParams{EntityID: entity.ID, ID: user.ID})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected active user row to be deleted, got %v", err)
	}

	if _, err := queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
		EntityID:        entity.ID,
		Username:        "ada@example.test",
		DisplayName:     "Ada Recreated",
		Email:           pgtype.Text{String: "ada@example.test", Valid: true},
		LifecycleStatus: "active",
		UserType:        "employee",
		PrimarySourceID: pgtype.Text{String: source.ID, Valid: true},
		Locale:          pgtype.Text{String: "en-US", Valid: true},
	}); err != nil {
		t.Fatalf("expected username to be reusable after archive, got %v", err)
	}
}

func TestApplicationDetailMutationsAreAtomicAndKeepReadableSecretsOutOfAudit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAdminServiceTestPool(ctx, t)
	queries := generated.New(pool)
	auditService := audit.NewService(queries)
	svc, err := NewAdminService(queries, auditService)
	if err != nil {
		t.Fatalf("new admin service: %v", err)
	}
	svc.SetTxStarter(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name: "Application Tenant", Slug: "application-tenant", DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	if _, err := queries.CreateRole(ctx, generated.CreateRoleParams{
		EntityID: entity.ID, Name: "Employee", Code: "employee",
	}); err != nil {
		t.Fatalf("create employee role: %v", err)
	}
	pkce := true
	workplaceProvider := "feishu"
	workplaceAppID := "workplace-app-id"
	workplaceAppSecret := "readable-workplace-secret"
	created, err := svc.CreateApplicationDetail(ctx, entity.ID, ApplicationWriteInput{
		Name: "OIDC Console", Type: "oidc_client", Status: "active",
		OIDCClient: &ApplicationOIDCClientInput{
			ClientID:           "shared-client-id",
			RedirectURIs:       []string{"https://client.example/callback"},
			AllowedScopes:      []string{"openid", "profile"},
			GrantTypes:         []string{"authorization_code"},
			ResponseTypes:      []string{"code"},
			PKCERequired:       &pkce,
			WorkplaceProvider:  &workplaceProvider,
			WorkplaceAppID:     &workplaceAppID,
			WorkplaceAppSecret: &workplaceAppSecret,
		},
	})
	if err != nil {
		t.Fatalf("create complete OIDC application: %v", err)
	}
	if created.OIDCClient == nil || created.OIDCClient.ClientSecret == "" {
		t.Fatalf("create response does not contain a readable client secret: %#v", created.OIDCClient)
	}
	clientSecret := created.OIDCClient.ClientSecret
	var storedVerifier string
	if err := pool.QueryRow(ctx, `SELECT client_secret_hash FROM oidc_clients WHERE entity_id = $1 AND application_id = $2`, entity.ID, created.ID).Scan(&storedVerifier); err != nil {
		t.Fatalf("read stored client-secret verifier: %v", err)
	}
	if storedVerifier == clientSecret || !clientsecret.Matches(storedVerifier, clientSecret) {
		t.Fatalf("stored client-secret verifier is not one-way: %q", storedVerifier)
	}
	if _, err := pool.Exec(ctx, `UPDATE oidc_clients SET client_secret_hash = $1 WHERE entity_id = $2 AND application_id = $3`, clientSecret, entity.ID, created.ID); err == nil {
		t.Fatal("database accepted a plaintext client secret")
	}
	loaded, err := svc.GetApplicationDetail(ctx, entity.ID, created.ID)
	if err != nil {
		t.Fatalf("get complete OIDC application: %v", err)
	}
	if loaded.OIDCClient == nil || loaded.OIDCClient.ClientSecret != "" {
		t.Fatalf("detail re-exposed client secret: %#v", loaded.OIDCClient)
	}

	if _, err := svc.CreateApplicationDetail(ctx, entity.ID, ApplicationWriteInput{
		Name: "Conflicting OIDC Console", Type: "oidc_client",
		OIDCClient: &ApplicationOIDCClientInput{ClientID: "shared-client-id"},
	}); err == nil {
		t.Fatal("expected duplicate client_id to fail")
	}
	count, err := queries.CountApplications(ctx, entity.ID)
	if err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if count != 1 {
		t.Fatalf("application count after failed complete create = %d, want 1", count)
	}

	if _, err := svc.UpdateApplicationDetail(ctx, entity.ID, created.ID, ApplicationWriteInput{
		Name: "Name That Must Roll Back", Type: "oidc_client", Status: "active",
		OIDCClient: &ApplicationOIDCClientInput{ClientID: "different-client-id"},
	}); err == nil {
		t.Fatal("expected immutable client_id update to fail")
	}
	stored, err := queries.GetApplicationByID(ctx, generated.GetApplicationByIDParams{EntityID: entity.ID, ID: created.ID})
	if err != nil {
		t.Fatalf("get application after failed update: %v", err)
	}
	if stored.Name != "OIDC Console" {
		t.Fatalf("application name after failed complete update = %q, want rollback", stored.Name)
	}

	updated, err := svc.UpdateApplicationDetail(ctx, entity.ID, created.ID, ApplicationWriteInput{
		Name: "Updated OIDC Console", Type: "oidc_client", Status: "active",
		OIDCClient: &ApplicationOIDCClientInput{
			ClientID:           "shared-client-id",
			RedirectURIs:       []string{"https://new.example/callback"},
			AllowedScopes:      []string{"openid", "profile"},
			GrantTypes:         []string{"authorization_code"},
			ResponseTypes:      []string{"code"},
			PKCERequired:       &pkce,
			WorkplaceProvider:  &workplaceProvider,
			WorkplaceAppID:     &workplaceAppID,
			WorkplaceAppSecret: &workplaceAppSecret,
		},
	})
	if err != nil {
		t.Fatalf("update complete OIDC application: %v", err)
	}
	if updated.OIDCClient == nil || len(updated.OIDCClient.RedirectURIs) != 1 || updated.OIDCClient.RedirectURIs[0] != "https://new.example/callback" {
		t.Fatalf("updated OIDC callback = %#v", updated.OIDCClient)
	}
	if updated.OIDCClient.ClientSecret != "" {
		t.Fatalf("ordinary edit re-exposed client secret: %q", updated.OIDCClient.ClientSecret)
	}
	assertApplicationAuditHasNoSecret(ctx, t, pool, created.ID, clientSecret, "readable-workplace-secret")

	apiApplication, err := svc.CreateApplicationDetail(ctx, entity.ID, ApplicationWriteInput{
		Name: "Automation API", Type: "api_client", Status: "active",
		Config: json.RawMessage(`{"client_id":"automation-api","client_secret":"readable-api-secret","allowed_scopes":["inventory:read"]}`),
	})
	if err != nil {
		t.Fatalf("create API application: %v", err)
	}
	var apiConfig apiClientApplicationConfig
	if err := json.Unmarshal(apiApplication.Config, &apiConfig); err != nil {
		t.Fatalf("decode API application config: %v", err)
	}
	if apiConfig.ClientID != "automation-api" || apiConfig.ClientSecret != "readable-api-secret" {
		t.Fatalf("stored API credentials = %#v", apiConfig)
	}
	assertApplicationAuditHasNoSecret(ctx, t, pool, apiApplication.ID, apiConfig.ClientSecret)
}

type failingApplicationAuditLogger struct{}

func (failingApplicationAuditLogger) Write(context.Context, audit.Event) error {
	return errors.New("forced audit failure")
}

func TestApplicationDetailMutationsRollBackWhenAuditWriteFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAdminServiceTestPool(ctx, t)
	queries := generated.New(pool)
	svc, err := NewAdminService(queries, failingApplicationAuditLogger{})
	if err != nil {
		t.Fatalf("new admin service: %v", err)
	}
	svc.SetTxStarter(pool)

	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name: "Audit Failure Tenant", Slug: "application-audit-failure", DefaultLocale: "en-US",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	if _, err := svc.CreateApplicationDetail(ctx, entity.ID, ApplicationWriteInput{
		Name: "Must Not Exist", Type: "internal_app", Status: "active",
		Config: json.RawMessage(`{"app_url":"https://example.test"}`),
	}); err == nil {
		t.Fatal("expected create to fail when audit write fails")
	}
	count, err := queries.CountApplications(ctx, entity.ID)
	if err != nil {
		t.Fatalf("count applications after failed create: %v", err)
	}
	if count != 0 {
		t.Fatalf("application count after failed audited create = %d, want 0", count)
	}

	existing, err := queries.CreateApplication(ctx, generated.CreateApplicationParams{
		EntityID: entity.ID,
		Name:     "Existing App",
		Type:     "internal_app",
		Status:   pgtype.Text{String: "active", Valid: true},
		Config:   []byte(`{"app_url":"https://old.example.test"}`),
	})
	if err != nil {
		t.Fatalf("seed application: %v", err)
	}

	if _, err := svc.UpdateApplicationDetail(ctx, entity.ID, existing.ID, ApplicationWriteInput{
		Name: "Must Roll Back", Type: "internal_app", Status: "active",
		Config: json.RawMessage(`{"app_url":"https://new.example.test"}`),
	}); err == nil {
		t.Fatal("expected update to fail when audit write fails")
	}
	stored, err := queries.GetApplicationByID(ctx, generated.GetApplicationByIDParams{EntityID: entity.ID, ID: existing.ID})
	if err != nil {
		t.Fatalf("get application after failed audited update: %v", err)
	}
	if stored.Name != "Existing App" || !strings.Contains(string(stored.Config), "old.example.test") {
		t.Fatalf("application changed after failed audited update: %#v", stored)
	}

	if err := svc.DeleteApplication(ctx, entity.ID, existing.ID); err == nil {
		t.Fatal("expected delete to fail when audit write fails")
	}
	if _, err := queries.GetApplicationByID(ctx, generated.GetApplicationByIDParams{EntityID: entity.ID, ID: existing.ID}); err != nil {
		t.Fatalf("application missing after failed audited delete: %v", err)
	}
}

func TestNormalizeApplicationWorkplaceUpdatePreservesOmittedFields(t *testing.T) {
	before := OIDCClientResponse{
		WorkplaceProvider:  "feishu",
		WorkplaceAppID:     "existing-app-id",
		WorkplaceAppSecret: "existing-secret",
	}

	provider, appID, appSecret, err := normalizeApplicationWorkplaceUpdate(before, ApplicationOIDCClientInput{})
	if err != nil {
		t.Fatalf("normalize omitted workplace update: %v", err)
	}
	if provider.Valid || appID.Valid || appSecret.Valid {
		t.Fatalf("omitted fields must be preserved, got provider=%#v appID=%#v appSecret=%#v", provider, appID, appSecret)
	}

	newSecret := "new-secret"
	provider, appID, appSecret, err = normalizeApplicationWorkplaceUpdate(before, ApplicationOIDCClientInput{
		WorkplaceAppSecret: &newSecret,
	})
	if err != nil {
		t.Fatalf("normalize secret-only workplace update: %v", err)
	}
	if provider.Valid || appID.Valid || !appSecret.Valid || appSecret.String != newSecret {
		t.Fatalf("secret-only update changed other fields: provider=%#v appID=%#v appSecret=%#v", provider, appID, appSecret)
	}

	empty := ""
	provider, appID, appSecret, err = normalizeApplicationWorkplaceUpdate(before, ApplicationOIDCClientInput{
		WorkplaceProvider:  &empty,
		WorkplaceAppID:     &empty,
		WorkplaceAppSecret: &empty,
	})
	if err != nil {
		t.Fatalf("normalize explicit workplace clear: %v", err)
	}
	if !provider.Valid || provider.String != "" || !appID.Valid || appID.String != "" || !appSecret.Valid || appSecret.String != "" {
		t.Fatalf("explicit clear was not retained: provider=%#v appID=%#v appSecret=%#v", provider, appID, appSecret)
	}
}

func assertApplicationAuditHasNoSecret(ctx context.Context, t *testing.T, pool *pgxpool.Pool, applicationID string, secrets ...string) {
	t.Helper()
	var afterState string
	if err := pool.QueryRow(ctx, `
		SELECT after_state::text
		FROM audit_logs
		WHERE resource_type = 'application' AND resource_id = $1
		ORDER BY created_at DESC
		LIMIT 1`, applicationID).Scan(&afterState); err != nil {
		t.Fatalf("read application audit: %v", err)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(afterState, secret) {
			t.Fatalf("application audit contains secret %q: %s", secret, afterState)
		}
	}
}

func newAdminServiceTestPool(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	if conn := os.Getenv("OPEN_IDB_TEST_DATABASE_URL"); conn != "" {
		applyAdminServiceTestMigrations(ctx, t, conn)
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
	applyAdminServiceTestMigrations(ctx, t, conn)

	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func applyAdminServiceTestMigrations(ctx context.Context, t *testing.T, conn string) {
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
