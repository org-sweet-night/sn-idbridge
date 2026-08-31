// SPDX-License-Identifier: MIT

package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ory/fosite"
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/smices/open-idb/internal/id"
)

func TestNewServiceRequiresCoreDependencies(t *testing.T) {
	if _, err := NewService(ServiceConfig{}); err == nil {
		t.Fatal("NewService() error = nil, want error")
	}
}

func TestServiceDiscoveryDocument(t *testing.T) {
	service := newTestService(t)

	doc := service.DiscoveryDocument()

	if doc.Issuer != "https://idb.example.test" {
		t.Fatalf("Issuer = %q, want %q", doc.Issuer, "https://idb.example.test")
	}
	if doc.AuthorizationEndpoint != "https://idb.example.test/oauth2/authorize" {
		t.Fatalf("AuthorizationEndpoint = %q", doc.AuthorizationEndpoint)
	}
}

func TestServiceJWKS(t *testing.T) {
	service := newTestService(t)

	jwks := service.JWKS()

	if len(jwks.Keys) != 1 {
		t.Fatalf("len(Keys) = %d, want 1", len(jwks.Keys))
	}
	if jwks.Keys[0].KeyID != "dev-key-1" {
		t.Fatalf("kid = %q, want %q", jwks.Keys[0].KeyID, "dev-key-1")
	}
}

func TestValidateAuthorizeRequest(t *testing.T) {
	service := newTestService(t)

	decision, err := service.ValidateAuthorizeRequest(context.Background(), AuthorizeInput{
		EntityID:            "entity-1",
		ClientID:            "client-1",
		RedirectURI:         "https://app.example.test/callback",
		ResponseType:        "code",
		Scopes:              []string{"openid", "email"},
		State:               "state-1",
		Nonce:               "nonce-1",
		CodeChallenge:       "challenge",
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("ValidateAuthorizeRequest() error = %v", err)
	}
	if decision.ClientID != "client-1" {
		t.Fatalf("ClientID = %q, want %q", decision.ClientID, "client-1")
	}
	if decision.Nonce != "nonce-1" {
		t.Fatalf("Nonce = %q, want %q", decision.Nonce, "nonce-1")
	}
}

func TestValidateAuthorizeRequestRejectsUnsupportedGrantShape(t *testing.T) {
	service := newTestService(t)

	_, err := service.ValidateAuthorizeRequest(context.Background(), AuthorizeInput{
		EntityID:            "entity-1",
		ClientID:            "client-1",
		RedirectURI:         "https://app.example.test/callback",
		ResponseType:        "token",
		CodeChallenge:       "challenge",
		CodeChallengeMethod: "S256",
	})
	if err == nil {
		t.Fatal("ValidateAuthorizeRequest() error = nil, want error")
	}
}

func TestValidateAuthorizeRequestDelegatesToFosite(t *testing.T) {
	errBoom := errors.New("boom")
	service := newTestService(t)
	service.fosite = fakeFositeProvider{authorizeErr: errBoom}

	_, err := service.ValidateAuthorizeRequest(context.Background(), AuthorizeInput{
		EntityID:            "entity-1",
		ClientID:            "client-1",
		RedirectURI:         "https://app.example.test/callback",
		ResponseType:        "code",
		Scopes:              []string{"openid"},
		CodeChallenge:       "challenge",
		CodeChallengeMethod: "S256",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ValidateAuthorizeRequest() error = %v, want %v", err, errBoom)
	}
}

func TestEffectiveAuthorizeScopesKeepsRequestedSubset(t *testing.T) {
	got, err := effectiveAuthorizeScopes(
		[]string{"openid", "email"},
		[]string{"openid", "profile", "email", "directory:read"},
	)
	if err != nil {
		t.Fatalf("effectiveAuthorizeScopes() error = %v", err)
	}
	want := []string{"openid", "email"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %#v, want %#v", got, want)
		}
	}
}

func TestEffectiveAuthorizeScopesFallsBackToAllowedScopesForLegacyEmptyRequest(t *testing.T) {
	got, err := effectiveAuthorizeScopes(nil, []string{"openid", "profile", "email"})
	if err != nil {
		t.Fatalf("effectiveAuthorizeScopes() error = %v", err)
	}
	want := []string{"openid", "profile", "email"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %#v, want %#v", got, want)
		}
	}
}

func TestEffectiveAuthorizeScopesRejectsScopeOutsideClientGrant(t *testing.T) {
	_, err := effectiveAuthorizeScopes(
		[]string{"openid", "admin:write"},
		[]string{"openid", "profile", "email", "directory:read"},
	)
	if err == nil {
		t.Fatal("effectiveAuthorizeScopes() error = nil, want error")
	}
}

func TestCanRedirectAuthorizationErrorOnlyToRegisteredURI(t *testing.T) {
	store := newExchangeTestStore()
	service := newExchangeTestService(t, store)

	if !service.canRedirectAuthorizationError(context.Background(), store.client.EntityID, store.client.ClientID, store.client.RedirectUris[0]) {
		t.Fatal("registered redirect URI was rejected")
	}
	if service.canRedirectAuthorizationError(context.Background(), store.client.EntityID, store.client.ClientID, "https://attacker.example/callback") {
		t.Fatal("unregistered redirect URI was accepted")
	}
}

func TestHasExternalIdentitySource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources []string
		want    bool
	}{
		{name: "empty", sources: nil, want: false},
		{name: "local only", sources: []string{"local"}, want: false},
		{name: "feishu", sources: []string{"feishu"}, want: true},
		{name: "local and feishu", sources: []string{"local", "feishu"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasExternalIdentitySource(tc.sources); got != tc.want {
				t.Fatalf("hasExternalIdentitySource(%v) = %v, want %v", tc.sources, got, tc.want)
			}
		})
	}
}

func TestParseIdentitySourcesAcceptsDecodedJSONArray(t *testing.T) {
	got := parseIdentitySources([]interface{}{"feishu", "ldap"})
	if len(got) != 2 || got[0] != "feishu" || got[1] != "ldap" {
		t.Fatalf("parseIdentitySources decoded array = %#v", got)
	}
}

func TestExchangeCodeInfersEntityFromAuthorizationCode(t *testing.T) {
	store := newExchangeTestStore()
	service := newExchangeTestService(t, store)

	response, err := service.ExchangeCode(context.Background(), exchangeTokenInput(store, ""))
	if err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	if response.AccessToken == "" || response.IDToken == "" {
		t.Fatalf("ExchangeCode() response = %#v, want signed tokens", response)
	}
}

func TestExchangeCodeRejectsProvidedInvalidClientSecret(t *testing.T) {
	store := newExchangeTestStore()
	store.client.SecretRequired = true
	service := newExchangeTestService(t, store)
	input := exchangeTokenInput(store, store.code.EntityID)
	input.ClientSecret = "wrong-secret"
	input.ClientSecretProvided = true

	if _, err := service.ExchangeCode(context.Background(), input); err == nil {
		t.Fatal("ExchangeCode() error = nil, want invalid client error")
	}
}

func TestExchangeCodeRequiresSecretForNewClient(t *testing.T) {
	store := newExchangeTestStore()
	store.client.SecretRequired = true
	service := newExchangeTestService(t, store)

	_, err := service.ExchangeCode(context.Background(), exchangeTokenInput(store, store.code.EntityID))
	if !errors.Is(err, ErrInvalidClient) {
		t.Fatalf("ExchangeCode() error = %v, want %v", err, ErrInvalidClient)
	}
}

func TestExchangeCodeAcceptsValidSecretForNewClient(t *testing.T) {
	store := newExchangeTestStore()
	store.client.SecretRequired = true
	service := newExchangeTestService(t, store)
	input := exchangeTokenInput(store, store.code.EntityID)
	input.ClientSecret = store.clientSecret
	input.ClientSecretProvided = true

	if _, err := service.ExchangeCode(context.Background(), input); err != nil {
		t.Fatalf("ExchangeCode() error = %v, want valid client secret to succeed", err)
	}
	if !store.lastFinalize.ClientSecretVerifier.Valid || store.lastFinalize.ClientSecretVerifier.String != store.client.ClientSecretHash.String {
		t.Fatalf("finalize verifier = %#v, want current stored verifier", store.lastFinalize.ClientSecretVerifier)
	}
	if store.lastFinalize.ClientSecretVerifier.String == store.clientSecret {
		t.Fatal("FinalizeAuthorizationCodeExchange() received plaintext client secret")
	}
}

func TestExchangeCodeAllowsLegacyClientWithoutSecret(t *testing.T) {
	store := newExchangeTestStore()
	service := newExchangeTestService(t, store)
	input := exchangeTokenInput(store, store.code.EntityID)

	if _, err := service.ExchangeCode(context.Background(), input); err != nil {
		t.Fatalf("ExchangeCode() error = %v, want legacy exchange to remain compatible", err)
	}
}

func TestExchangeCodeConsumesCodeAndPersistsTokensAtomically(t *testing.T) {
	store := newExchangeTestStore()
	service := newExchangeTestService(t, store)

	if _, err := service.ExchangeCode(context.Background(), exchangeTokenInput(store, store.code.EntityID)); err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	if store.finalizeCalls != 1 {
		t.Fatalf("FinalizeAuthorizationCodeExchange() calls = %d, want 1", store.finalizeCalls)
	}
	if store.markUsedCalls != 0 || store.createTokenCalls != 0 {
		t.Fatalf("legacy persistence calls = mark:%d create:%d, want none", store.markUsedCalls, store.createTokenCalls)
	}
}

func TestIssueClientCredentialsIssuesScopedServiceToken(t *testing.T) {
	store := newExchangeTestStore()
	store.client.GrantTypes = []string{"client_credentials"}
	store.client.AllowedScopes = []string{"directory:read"}
	service := newExchangeTestService(t, store)

	response, err := service.IssueClientCredentials(context.Background(), ClientCredentialsInput{
		EntityID:             store.client.EntityID,
		ClientID:             store.client.ClientID,
		ClientSecret:         store.clientSecret,
		ClientSecretProvided: true,
		Scopes:               []string{"directory:read"},
	})
	if err != nil {
		t.Fatalf("IssueClientCredentials() error = %v", err)
	}
	if response.AccessToken == "" || response.IDToken != "" || response.Scope != "directory:read" {
		t.Fatalf("response = %#v", response)
	}
	if store.createTokenCalls != 1 {
		t.Fatalf("CreateOAuthToken() calls = %d, want 1", store.createTokenCalls)
	}
	if store.lastToken.UserID != store.client.ID || store.lastToken.ClientID != store.client.ClientID {
		t.Fatalf("persisted token = %#v, want client subject", store.lastToken)
	}
}

func TestIssueClientCredentialsRejectsUnapprovedScope(t *testing.T) {
	store := newExchangeTestStore()
	store.client.GrantTypes = []string{"client_credentials"}
	store.client.AllowedScopes = []string{"directory:read"}
	service := newExchangeTestService(t, store)

	_, err := service.IssueClientCredentials(context.Background(), ClientCredentialsInput{
		EntityID:             store.client.EntityID,
		ClientID:             store.client.ClientID,
		ClientSecret:         store.clientSecret,
		ClientSecretProvided: true,
		Scopes:               []string{"profile"},
	})
	if err == nil {
		t.Fatal("IssueClientCredentials() error = nil, want rejected scope")
	}
	if store.createTokenCalls != 0 {
		t.Fatalf("CreateOAuthToken() calls = %d, want 0", store.createTokenCalls)
	}
}

type exchangeTestStore struct {
	code             generated.OauthAuthorizationCode
	client           generated.OidcClient
	clientSecret     string
	finalizeCalls    int
	lastFinalize     generated.FinalizeAuthorizationCodeExchangeParams
	markUsedCalls    int
	createTokenCalls int
	lastToken        generated.CreateOAuthTokenParams
}

func newExchangeTestStore() *exchangeTestStore {
	now := time.Unix(1_800_000_000, 0).UTC()
	entityID := id.NewULID()
	userID := id.NewULID()
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	digest := sha256.Sum256([]byte(verifier))
	clientSecretValue := "secret-1"
	return &exchangeTestStore{
		code: generated.OauthAuthorizationCode{
			ID:                  id.NewULID(),
			EntityID:            entityID,
			ClientID:            "client-1",
			UserID:              userID,
			CodeHash:            HashToken("code-1"),
			RedirectUri:         "https://app.example.test/callback",
			Scopes:              []string{"openid", "email"},
			CodeChallenge:       base64.RawURLEncoding.EncodeToString(digest[:]),
			CodeChallengeMethod: CodeChallengeS256,
			Nonce:               pgtype.Text{String: "nonce-1", Valid: true},
			ExpiresAt:           pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true},
		},
		client: generated.OidcClient{
			ID:               id.NewULID(),
			EntityID:         entityID,
			ApplicationID:    id.NewULID(),
			ClientID:         "client-1",
			ClientSecretHash: pgtype.Text{String: clientsecret.Hash(clientSecretValue), Valid: true},
			RedirectUris:     []string{"https://app.example.test/callback"},
			AllowedScopes:    []string{"openid", "profile", "email"},
			Status:           "active",
		},
		clientSecret: clientSecretValue,
	}
}

func exchangeTokenInput(store *exchangeTestStore, entityID string) TokenInput {
	return TokenInput{
		EntityID:     entityID,
		ClientID:     store.code.ClientID,
		Code:         "code-1",
		RedirectURI:  store.code.RedirectUri,
		CodeVerifier: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
	}
}

func newExchangeTestService(t *testing.T, store Store) *Service {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Issuer:         "https://idb.example.test",
		KeyID:          "dev-key-1",
		PrivateKey:     key,
		Store:          store,
		Now:            func() time.Time { return time.Unix(1_800_000_000, 0).UTC() },
		AuthCodeTTL:    5 * time.Minute,
		IDTokenTTL:     15 * time.Minute,
		AccessTokenTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func (s *exchangeTestStore) GetOIDCClientByClientID(context.Context, generated.GetOIDCClientByClientIDParams) (generated.OidcClient, error) {
	return s.client, nil
}
func (s *exchangeTestStore) GetUserLifecycleStatus(context.Context, generated.GetUserLifecycleStatusParams) (string, error) {
	return "active", nil
}
func (s *exchangeTestStore) GetUserClaimsForToken(context.Context, generated.GetUserClaimsForTokenParams) (generated.GetUserClaimsForTokenRow, error) {
	return generated.GetUserClaimsForTokenRow{Username: "ada", DisplayName: "Ada", IdentitySources: []interface{}{"feishu"}}, nil
}
func (s *exchangeTestStore) HasApplicationAccess(context.Context, generated.HasApplicationAccessParams) (pgtype.Bool, error) {
	return pgtype.Bool{Bool: true, Valid: true}, nil
}
func (s *exchangeTestStore) GetUserRolesForToken(context.Context, generated.GetUserRolesForTokenParams) ([]string, error) {
	return []string{"employee"}, nil
}
func (s *exchangeTestStore) GetPermissionsVersion(context.Context, string) (int64, error) {
	return 1, nil
}
func (s *exchangeTestStore) GetResourceScopesVersion(context.Context, string) (int64, error) {
	return 1, nil
}
func (s *exchangeTestStore) CreateAuthorizationCode(context.Context, generated.CreateAuthorizationCodeParams) (generated.OauthAuthorizationCode, error) {
	return s.code, nil
}
func (s *exchangeTestStore) GetAuthorizationCode(context.Context, generated.GetAuthorizationCodeParams) (generated.OauthAuthorizationCode, error) {
	return s.code, nil
}
func (s *exchangeTestStore) GetAuthorizationCodeByHash(context.Context, string) (generated.OauthAuthorizationCode, error) {
	return s.code, nil
}
func (s *exchangeTestStore) MarkAuthorizationCodeUsed(context.Context, generated.MarkAuthorizationCodeUsedParams) (generated.OauthAuthorizationCode, error) {
	s.markUsedCalls++
	return s.code, nil
}
func (s *exchangeTestStore) CreateOAuthToken(_ context.Context, arg generated.CreateOAuthTokenParams) (generated.OauthToken, error) {
	s.createTokenCalls++
	s.lastToken = arg
	return generated.OauthToken{}, nil
}
func (s *exchangeTestStore) FinalizeAuthorizationCodeExchange(_ context.Context, arg generated.FinalizeAuthorizationCodeExchangeParams) (generated.FinalizeAuthorizationCodeExchangeRow, error) {
	s.finalizeCalls++
	s.lastFinalize = arg
	return generated.FinalizeAuthorizationCodeExchangeRow{
		ID:       s.code.ID,
		EntityID: s.code.EntityID,
		ClientID: s.code.ClientID,
		UserID:   s.code.UserID,
	}, nil
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Issuer:         "https://idb.example.test/",
		KeyID:          "dev-key-1",
		PrivateKey:     key,
		AuthCodeTTL:    5 * time.Minute,
		AccessTokenTTL: 15 * time.Minute,
		IDTokenTTL:     15 * time.Minute,
		Now:            func() time.Time { return time.Unix(1_800_000_000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

type fakeFositeProvider struct {
	authorizeErr error
}

func (f fakeFositeProvider) NewAuthorizeRequest(context.Context, *http.Request) (fosite.AuthorizeRequester, error) {
	return nil, f.authorizeErr
}

func (f fakeFositeProvider) NewAccessRequest(context.Context, *http.Request, fosite.Session) (fosite.AccessRequester, error) {
	return nil, nil
}

func (f fakeFositeProvider) WriteAuthorizeError(context.Context, http.ResponseWriter, fosite.AuthorizeRequester, error) {
}

func (f fakeFositeProvider) WriteAccessError(context.Context, http.ResponseWriter, fosite.AccessRequester, error) {
}
