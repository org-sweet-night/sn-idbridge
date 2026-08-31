// SPDX-License-Identifier: MIT

package adminapi

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/db/generated"
)

func TestOIDCClientDetailExposesSecretRequirement(t *testing.T) {
	response := oidcClientFromRow(generated.OidcClient{
		SecretRequired:   true,
		ClientSecretHash: pgtype.Text{String: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Valid: true},
	})

	if !response.SecretRequired {
		t.Fatal("secret_required = false, want true")
	}
	if response.ClientSecret != "" {
		t.Fatalf("client_secret = %q, want redacted", response.ClientSecret)
	}
}
