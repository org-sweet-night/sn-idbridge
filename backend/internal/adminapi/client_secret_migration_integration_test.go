// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/smices/open-idb/internal/db/generated"
)

func TestOIDCClientSecretVerifierMigrationConvertsLegacyPlaintext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newAdminServiceTestPool(ctx, t)
	db, err := sql.Open("pgx", pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open migration database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	if err := goose.DownToContext(ctx, db, "../../migrations", 9); err != nil {
		t.Fatalf("migrate down to legacy verifier storage: %v", err)
	}

	queries := generated.New(pool)
	entity, err := queries.CreateEntity(ctx, generated.CreateEntityParams{Name: "Legacy Secret", Slug: "legacy-secret", DefaultLocale: "en-US"})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}
	application, err := queries.CreateApplication(ctx, generated.CreateApplicationParams{EntityID: entity.ID, Name: "Legacy OIDC", Type: "oidc_client"})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	// A legacy plaintext may coincidentally look exactly like the new verifier
	// format. The transactional Goose migration must still hash every legacy row
	// once instead of treating this credential as already converted.
	plaintext := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := queries.CreateOIDCClient(ctx, generated.CreateOIDCClientParams{
		EntityID: entity.ID, ApplicationID: application.ID, ClientID: "legacy-client",
		ClientSecretHash: pgtype.Text{String: plaintext, Valid: true},
		RedirectUris:     []string{"https://legacy.example/callback"}, AllowedScopes: []string{"openid"},
		GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"}, PkceRequired: true,
	}); err != nil {
		t.Fatalf("create legacy client: %v", err)
	}

	if err := goose.UpToContext(ctx, db, "../../migrations", 11); err != nil {
		t.Fatalf("apply verifier migration: %v", err)
	}
	var verifier string
	if err := pool.QueryRow(ctx, `SELECT client_secret_hash FROM oidc_clients WHERE entity_id = $1 AND application_id = $2`, entity.ID, application.ID).Scan(&verifier); err != nil {
		t.Fatalf("read migrated verifier: %v", err)
	}
	if verifier == plaintext || !clientsecret.Matches(verifier, plaintext) {
		t.Fatalf("migrated verifier = %q, want one-way match", verifier)
	}
	invalidVerifier := "legacy-secret-that-is-not-a-sha256-verifier"
	if _, err := pool.Exec(ctx, `UPDATE oidc_clients SET client_secret_hash = $1 WHERE entity_id = $2 AND application_id = $3`, invalidVerifier, entity.ID, application.ID); err == nil {
		t.Fatal("migrated format constraint accepted plaintext")
	}
}
