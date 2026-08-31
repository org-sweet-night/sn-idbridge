// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestSandboxBootstrapConcurrentFirstRunAndRerunConverge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newBootstrapTestPool(ctx, t)
	cfg := testBootstrapConfig()
	service, err := NewService(pool)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	start := make(chan struct{})
	results := make(chan Result, 2)
	errorsByRun := make(chan error, 2)
	var runners sync.WaitGroup
	for range 2 {
		runners.Add(1)
		go func() {
			defer runners.Done()
			<-start
			result, err := service.Apply(ctx, cfg)
			results <- result
			errorsByRun <- err
		}()
	}
	close(start)
	runners.Wait()
	close(results)
	close(errorsByRun)
	for err := range errorsByRun {
		if err != nil {
			t.Fatalf("concurrent Apply() error = %v", err)
		}
	}
	entityIDs := map[string]bool{}
	var entityID string
	for result := range results {
		entityIDs[result.EntityID] = true
		entityID = result.EntityID
	}
	if len(entityIDs) != 1 {
		t.Fatalf("concurrent entity IDs = %#v, want one exact public ID", entityIDs)
	}

	var applicationCount, employeeAssignmentCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM applications WHERE name = $1`, cfg.DirectoryReaderApplicationName).Scan(&applicationCount); err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if applicationCount != 1 {
		t.Fatalf("application count = %d, want 1", applicationCount)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM application_assignments aa
JOIN applications app ON app.entity_id = aa.entity_id AND app.id = aa.application_id
JOIN roles role ON role.entity_id = aa.entity_id AND role.id = aa.subject_id
WHERE app.name = $1 AND aa.subject_type = 'role' AND aa.effect = 'allow' AND role.code = $2`,
		cfg.DirectoryReaderApplicationName, EmployeeRoleCode).Scan(&employeeAssignmentCount); err != nil {
		t.Fatalf("count employee assignment: %v", err)
	}
	if employeeAssignmentCount != 1 {
		t.Fatalf("employee assignment count = %d, want 1", employeeAssignmentCount)
	}

	assertFixtureRoleSet(ctx, t, pool, cfg.FixtureOwnerUsername, []string{EmployeeRoleCode, PAMApproverRoleCode, PAMRequesterRoleCode})
	assertFixtureRoleSet(ctx, t, pool, cfg.FixtureUnrelatedUsername, []string{EmployeeRoleCode})
	assertCanonicalRole(ctx, t, pool, entityID, EmployeeRoleCode, "员工", "默认员工角色；用于登录后访问已授权的业务应用。")
	assertCanonicalRole(ctx, t, pool, entityID, PAMRequesterRoleCode, "ISA PAM Requester", "May request ISA privileged access.")
	assertCanonicalRole(ctx, t, pool, entityID, PAMApproverRoleCode, "ISA PAM Approver", "May approve ISA privileged access.")

	var auditCountBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&auditCountBefore); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	rererun, err := service.Apply(ctx, cfg)
	if err != nil {
		t.Fatalf("idempotent Apply() error = %v", err)
	}
	if !entityIDs[rererun.EntityID] {
		t.Fatalf("rerun entity ID = %q, want original", rererun.EntityID)
	}
	var auditCountAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&auditCountAfter); err != nil {
		t.Fatalf("count audits after rerun: %v", err)
	}
	if auditCountAfter != auditCountBefore {
		t.Fatalf("rerun audit count = %d, want unchanged %d", auditCountAfter, auditCountBefore)
	}
	var secretAuditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE after_state::text LIKE '%' || $1 || '%'`, cfg.DirectoryReaderClientSecret).Scan(&secretAuditCount); err != nil {
		t.Fatalf("scan audits for secret: %v", err)
	}
	if secretAuditCount != 0 {
		t.Fatal("projected client secret appeared in audit state")
	}
	var storedVerifier string
	if err := pool.QueryRow(ctx, `
SELECT client.client_secret_hash
FROM oidc_clients client
JOIN applications app ON app.entity_id = client.entity_id AND app.id = client.application_id
WHERE app.name = $1`, cfg.DirectoryReaderApplicationName).Scan(&storedVerifier); err != nil {
		t.Fatalf("read stored client-secret verifier: %v", err)
	}
	if storedVerifier == cfg.DirectoryReaderClientSecret || !clientsecret.Matches(storedVerifier, cfg.DirectoryReaderClientSecret) {
		t.Fatalf("stored client-secret verifier is not one-way: %q", storedVerifier)
	}
	driftedConfig := cfg
	driftedConfig.DirectoryReaderClientSecret = "different-directory-secret-1234567890"
	if _, err := service.Apply(ctx, driftedConfig); err == nil || !strings.Contains(err.Error(), "credential drift; refusing rotation") {
		t.Fatalf("credential-drift Apply() error = %v", err)
	}
	var verifierAfterDrift string
	if err := pool.QueryRow(ctx, `
SELECT client.client_secret_hash
FROM oidc_clients client
JOIN applications app ON app.entity_id = client.entity_id AND app.id = client.application_id
WHERE app.name = $1`, cfg.DirectoryReaderApplicationName).Scan(&verifierAfterDrift); err != nil {
		t.Fatalf("read verifier after drift rejection: %v", err)
	}
	if verifierAfterDrift != storedVerifier {
		t.Fatal("bootstrap rotated the directory-reader credential on drift")
	}
}

func TestSandboxBootstrapFailsOnCanonicalRoleMetadataOrExtraFixtureRoleDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newBootstrapTestPool(ctx, t)
	cfg := testBootstrapConfig()
	service, err := NewService(pool)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	result, err := service.Apply(ctx, cfg)
	if err != nil {
		t.Fatalf("initial Apply() error = %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE roles SET name = 'drifted' WHERE entity_id = $1 AND code = $2`, result.EntityID, PAMRequesterRoleCode); err != nil {
		t.Fatalf("drift role metadata: %v", err)
	}
	if _, err := service.Apply(ctx, cfg); err == nil || !strings.Contains(err.Error(), "metadata drift") {
		t.Fatalf("metadata-drift Apply() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE roles SET name = 'ISA PAM Requester' WHERE entity_id = $1 AND code = $2`, result.EntityID, PAMRequesterRoleCode); err != nil {
		t.Fatalf("restore role metadata: %v", err)
	}

	var unrelatedUserID, extraRoleID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE entity_id = $1 AND username = $2`, result.EntityID, cfg.FixtureUnrelatedUsername).Scan(&unrelatedUserID); err != nil {
		t.Fatalf("read unrelated fixture: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO roles (entity_id, name, code, description) VALUES ($1, 'Unexpected', 'unexpected:privileged', 'test drift') RETURNING id`, result.EntityID).Scan(&extraRoleID); err != nil {
		t.Fatalf("create unexpected role: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_roles (entity_id, user_id, role_id) VALUES ($1, $2, $3)`, result.EntityID, unrelatedUserID, extraRoleID); err != nil {
		t.Fatalf("assign unexpected role: %v", err)
	}
	if _, err := service.Apply(ctx, cfg); err == nil || !strings.Contains(err.Error(), `unexpected authoritative role "unexpected:privileged"`) {
		t.Fatalf("extra-role Apply() error = %v", err)
	}
}

func assertFixtureRoleSet(ctx context.Context, t *testing.T, pool *pgxpool.Pool, username string, want []string) {
	t.Helper()
	rows, err := pool.Query(ctx, `
SELECT role.code
FROM users managed
JOIN user_roles assignment ON assignment.entity_id = managed.entity_id AND assignment.user_id = managed.id
JOIN roles role ON role.entity_id = assignment.entity_id AND role.id = assignment.role_id
WHERE managed.username = $1
ORDER BY role.code`, username)
	if err != nil {
		t.Fatalf("query roles for %s: %v", username, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			t.Fatalf("scan role: %v", err)
		}
		got = append(got, code)
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("roles for %s = %#v, want %#v", username, got, want)
	}
}

func assertCanonicalRole(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entityID, code, wantName, wantDescription string) {
	t.Helper()
	var name, description string
	if err := pool.QueryRow(ctx, `SELECT name, description FROM roles WHERE entity_id = $1 AND code = $2`, entityID, code).Scan(&name, &description); err != nil {
		t.Fatalf("read role %s: %v", code, err)
	}
	if name != wantName || description != wantDescription {
		t.Fatalf("role %s metadata = (%q, %q), want (%q, %q)", code, name, description, wantName, wantDescription)
	}
}

func testBootstrapConfig() Config {
	return Config{
		DatabaseURL: "unused-by-service-test", DefaultLocale: "en-US",
		EntitySlug: "isa-sandbox", EntityName: "ISA Sandbox",
		AdminUsername: "sandbox-admin", AdminPassword: "StrongPassword123", AdminRole: "enterprise_admin",
		DirectoryReaderApplicationName: "ISA IAM Directory Reader",
		DirectoryReaderClientID:        "isa-iam-directory-reader",
		DirectoryReaderClientSecret:    "directory-reader-secret-1234567890",
		FixtureSourceName:              "ISA Sandbox Fixtures",
		FixtureOwnerUsername:           "fixture-owner",
		FixtureOwnerTROBSUserID:        "opaque-136",
		FixtureUnrelatedUsername:       "fixture-unrelated",
		SandboxSingleDeveloper:         true,
	}
}

func newBootstrapTestPool(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	if conn := testDatabaseURL(); conn != "" {
		applyBootstrapTestMigrations(ctx, t, conn)
		pool, err := pgxpool.New(ctx, conn)
		if err != nil {
			t.Fatalf("open configured test database: %v", err)
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
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := container.Terminate(cleanupCtx); err != nil {
			t.Errorf("terminate postgres container: %v", err)
		}
	})
	conn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("test database connection string: %v", err)
	}
	applyBootstrapTestMigrations(ctx, t, conn)
	pool, err := pgxpool.New(ctx, conn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testDatabaseURL() string {
	// Kept in a helper so tests never include a configured URL in failure text.
	return os.Getenv("OPEN_IDB_TEST_DATABASE_URL")
}

func applyBootstrapTestMigrations(ctx context.Context, t *testing.T, conn string) {
	t.Helper()
	db, err := sql.Open("pgx", conn)
	if err != nil {
		t.Fatalf("open migration database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set migration dialect: %v", err)
	}
	if err := goose.UpContext(ctx, db, "../../migrations"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}
