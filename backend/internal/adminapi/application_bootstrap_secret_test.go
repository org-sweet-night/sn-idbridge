// SPDX-License-Identifier: MIT

package adminapi

import (
	"testing"

	"github.com/smices/open-idb/internal/clientsecret"
)

func TestNewOIDCClientParamsUsesOnlyExplicitInProcessBootstrapSecret(t *testing.T) {
	secret := "projected-directory-reader-secret-1234567890"
	pkceRequired := false
	params, returnedSecret, err := newOIDCClientParams(testEntityID(), testUserID(), "active", ApplicationOIDCClientInput{
		ClientID:      "isa-iam-directory-reader",
		AllowedScopes: []string{"directory:read"},
		GrantTypes:    []string{"client_credentials"},
		RedirectURIs:  []string{},
		ResponseTypes: []string{},
		PKCERequired:  &pkceRequired,
	}, &secret)
	if err != nil {
		t.Fatalf("newOIDCClientParams() error = %v", err)
	}
	if returnedSecret != secret {
		t.Fatal("newOIDCClientParams() did not retain the secret for the in-process create")
	}
	if !params.ClientSecretHash.Valid || params.ClientSecretHash.String == secret || !clientsecret.Matches(params.ClientSecretHash.String, secret) {
		t.Fatal("newOIDCClientParams() did not create a one-way verifier")
	}

	auditView := oidcClientForAudit(OIDCClientResponse{ClientSecret: secret})
	if auditView.ClientSecret != "" {
		t.Fatal("projected secret was retained in the audit view")
	}
}
