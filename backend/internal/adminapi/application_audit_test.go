// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestApplicationDetailForAuditRemovesSecretsWithoutChangingAPIResponse(t *testing.T) {
	original := ApplicationDetailResponse{
		ApplicationResponse: ApplicationResponse{
			ID:     "application-id",
			Type:   "api_client",
			Config: json.RawMessage(`{"client_id":"api-client","client_secret":"readable-api-secret","workplace_app_secret":"workplace-secret"}`),
		},
		OIDCClient: &OIDCClientResponse{
			ClientID:           "oidc-client",
			ClientSecret:       "readable-oidc-secret",
			WorkplaceAppSecret: "readable-workplace-secret",
		},
	}

	auditCopy := applicationDetailForAudit(original)
	encoded, err := json.Marshal(auditCopy)
	if err != nil {
		t.Fatalf("marshal audit copy: %v", err)
	}
	if strings.Contains(string(encoded), "readable-api-secret") ||
		strings.Contains(string(encoded), "workplace-secret") ||
		strings.Contains(string(encoded), "readable-oidc-secret") ||
		strings.Contains(string(encoded), "client_secret") ||
		strings.Contains(string(encoded), "workplace_app_secret") {
		t.Fatalf("audit payload contains a secret: %s", encoded)
	}
	if !strings.Contains(string(original.Config), "readable-api-secret") {
		t.Fatal("applicationDetailForAudit mutated the API config")
	}
	if original.OIDCClient.ClientSecret == "" || original.OIDCClient.WorkplaceAppSecret == "" {
		t.Fatal("applicationDetailForAudit mutated the OIDC API response")
	}
}

func TestCreateApplicationDetailRequiresTypeSpecificPayload(t *testing.T) {
	service := &AdminService{}
	tests := []ApplicationWriteInput{
		{Name: "OIDC", Type: "oidc_client"},
		{Name: "OIDC without callback", Type: "oidc_client", OIDCClient: &ApplicationOIDCClientInput{}},
		{Name: "API", Type: "api_client"},
		{Name: "Internal", Type: "internal_app"},
	}
	for _, input := range tests {
		_, err := service.CreateApplicationDetail(context.Background(), "entity", input)
		var requestErr *applicationRequestError
		if !errors.As(err, &requestErr) {
			t.Fatalf("CreateApplicationDetail(%s) error = %v, want request validation error", input.Type, err)
		}
	}
}

func TestNormalizeOIDCRedirectURIsTrimsAndRejectsInvalidValues(t *testing.T) {
	got, err := normalizeOIDCRedirectURIs([]string{"  https://client.example/callback  "}, true)
	if err != nil {
		t.Fatalf("normalizeOIDCRedirectURIs() error = %v", err)
	}
	if len(got) != 1 || got[0] != "https://client.example/callback" {
		t.Fatalf("normalized redirect URIs = %#v", got)
	}
	for _, values := range [][]string{{}, {"abc"}, {"https://"}} {
		if _, err := normalizeOIDCRedirectURIs(values, true); err == nil {
			t.Fatalf("normalizeOIDCRedirectURIs(%#v) error = nil", values)
		}
	}
}

func TestNormalizeOIDCClientCreateSupportsNonInteractiveClientCredentials(t *testing.T) {
	input := ApplicationOIDCClientInput{
		AllowedScopes: []string{"directory:read"},
		GrantTypes:    []string{"client_credentials"},
	}

	if err := normalizeOIDCClientCreate(&input); err != nil {
		t.Fatalf("normalizeOIDCClientCreate() error = %v", err)
	}
	if len(input.RedirectURIs) != 0 || len(input.ResponseTypes) != 0 {
		t.Fatalf("non-interactive client carried browser fields: %#v", input)
	}
	if input.PKCERequired == nil || *input.PKCERequired {
		t.Fatalf("pkce_required = %#v, want explicit false", input.PKCERequired)
	}
}

func TestNormalizeOIDCClientCreateRejectsInteractiveFieldsOnClientCredentials(t *testing.T) {
	pkce := true
	for _, input := range []ApplicationOIDCClientInput{
		{
			AllowedScopes: []string{"directory:read"},
			GrantTypes:    []string{"client_credentials"},
			RedirectURIs:  []string{"https://client.example/callback"},
		},
		{
			AllowedScopes: []string{"directory:read"},
			GrantTypes:    []string{"client_credentials"},
			ResponseTypes: []string{"code"},
		},
		{
			AllowedScopes: []string{"directory:read"},
			GrantTypes:    []string{"client_credentials"},
			PKCERequired:  &pkce,
		},
		{
			GrantTypes: []string{"client_credentials"},
		},
	} {
		if err := normalizeOIDCClientCreate(&input); err == nil {
			t.Fatalf("normalizeOIDCClientCreate(%#v) error = nil", input)
		}
	}
}
