// SPDX-License-Identifier: MIT

package sso

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/smices/open-idb/internal/ephemeral"
	"github.com/smices/open-idb/internal/id"
)

func TestDiscoveryHandler(t *testing.T) {
	router := newHandlerTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var doc DiscoveryDocument
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if doc.Issuer != "https://idb.example.test" {
		t.Fatalf("Issuer = %q", doc.Issuer)
	}
}

func TestAPIDiscoveryHandlerUsesAPIEndpoints(t *testing.T) {
	router := newHandlerTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/.well-known/openid-configuration", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var doc DiscoveryDocument
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if doc.Issuer != "https://idb.example.test" {
		t.Fatalf("Issuer = %q", doc.Issuer)
	}
	if doc.AuthorizationEndpoint != "https://idb.example.test/api/oauth2/authorize" {
		t.Fatalf("AuthorizationEndpoint = %q", doc.AuthorizationEndpoint)
	}
	if doc.TokenEndpoint != "https://idb.example.test/api/oauth2/token" {
		t.Fatalf("TokenEndpoint = %q", doc.TokenEndpoint)
	}
	if doc.UserinfoEndpoint != "https://idb.example.test/api/oauth2/userinfo" {
		t.Fatalf("UserinfoEndpoint = %q", doc.UserinfoEndpoint)
	}
}

func TestJWKSHandler(t *testing.T) {
	router := newHandlerTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var jwks JWKS
	if err := json.NewDecoder(rec.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].KeyID != "dev-key-1" {
		t.Fatalf("JWKS = %#v", jwks)
	}
}

func TestAPIJWKSHandler(t *testing.T) {
	router := newHandlerTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/.well-known/jwks.json", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var jwks JWKS
	if err := json.NewDecoder(rec.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].KeyID != "dev-key-1" {
		t.Fatalf("JWKS = %#v", jwks)
	}
}

func TestAuthorizeRedirectsToLoginWhenUnauthenticated(t *testing.T) {
	router := newHandlerTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/authorize?client_id=test&redirect_uri=http://example.com/callback", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "/login") {
		t.Fatalf("Location = %q, want redirect to /login", location)
	}
	if !strings.Contains(location, "return_to") {
		t.Fatalf("Location = %q, want return_to parameter", location)
	}
}

func TestAPIAuthorizeRedirectsToLoginWhenUnauthenticated(t *testing.T) {
	router := newHandlerTestRouter(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/oauth2/authorize?client_id=test&redirect_uri=http://example.com/callback", nil)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	location := rec.Header().Get("Location")
	if !strings.Contains(location, "/login") {
		t.Fatalf("Location = %q, want redirect to /login", location)
	}
	if !strings.Contains(location, "return_to") {
		t.Fatalf("Location = %q, want return_to parameter", location)
	}
}

func TestTokenEndpointIssuesClientCredentials(t *testing.T) {
	store := newExchangeTestStore()
	store.client.GrantTypes = []string{"client_credentials"}
	store.client.AllowedScopes = []string{"directory:read"}
	service := newExchangeTestService(t, store)
	handler := NewHandler(service)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	form := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {"directory:read"},
		"entity_id":  {store.client.EntityID},
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(store.client.ClientID, store.clientSecret)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response TokenResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.AccessToken == "" || response.IDToken != "" || response.Scope != "directory:read" {
		t.Fatalf("response = %#v", response)
	}
}

func TestAuthorizationRedirectURLPreservesQueryAndEncodesSuccessParameters(t *testing.T) {
	got, err := authorizationRedirectURL(
		"https://client.example/callback?tenant=alpha#complete",
		map[string]string{
			"code":  "code&with#reserved",
			"state": "state&next=#/home",
		},
	)
	if err != nil {
		t.Fatalf("authorizationRedirectURL() error = %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if u.Query().Get("tenant") != "alpha" {
		t.Fatalf("tenant = %q, want alpha", u.Query().Get("tenant"))
	}
	if u.Query().Get("code") != "code&with#reserved" {
		t.Fatalf("code = %q", u.Query().Get("code"))
	}
	if u.Query().Get("state") != "state&next=#/home" {
		t.Fatalf("state = %q", u.Query().Get("state"))
	}
	if u.Fragment != "complete" {
		t.Fatalf("fragment = %q, want complete", u.Fragment)
	}
}

func TestAuthorizationRedirectURLEncodesErrorParameters(t *testing.T) {
	got, err := authorizationRedirectURL(
		"https://client.example/callback?tenant=alpha",
		map[string]string{
			"error":             "invalid_request",
			"error_description": "scope a&b is invalid #1",
			"state":             "return=/orders&tab=#open",
		},
	)
	if err != nil {
		t.Fatalf("authorizationRedirectURL() error = %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if u.Query().Get("tenant") != "alpha" {
		t.Fatalf("tenant = %q, want alpha", u.Query().Get("tenant"))
	}
	if u.Query().Get("error_description") != "scope a&b is invalid #1" {
		t.Fatalf("error_description = %q", u.Query().Get("error_description"))
	}
	if u.Query().Get("state") != "return=/orders&tab=#open" {
		t.Fatalf("state = %q", u.Query().Get("state"))
	}
}

func TestShouldRestartAuthorizeLogin(t *testing.T) {
	if !shouldRestartAuthorizeLogin(fmt.Errorf("%w: status is disabled", ErrUserInactive)) {
		t.Fatal("inactive user should restart login")
	}
	if !shouldRestartAuthorizeLogin(ErrUserNotEligibleForApplicationSSO) {
		t.Fatal("ineligible SSO user should restart login")
	}
	if shouldRestartAuthorizeLogin(fmt.Errorf("invalid redirect uri")) {
		t.Fatal("invalid authorize request should not restart login")
	}
}

func TestUnsupportedEndpoints(t *testing.T) {
	router := newHandlerTestRouter(t)
	for _, tc := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{name: "userinfo without bearer token", method: http.MethodGet, path: "/oauth2/userinfo", wantStatus: http.StatusUnauthorized},
		{name: "revoke with no body returns 200", method: http.MethodPost, path: "/oauth2/revoke", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)

			router.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body = %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestTokenEndpointRateLimitsByClientAndIP(t *testing.T) {
	router := newHandlerTestRouterWithEphemeralStore(t, ephemeral.NewMemoryStore())
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"client-1"},
		"code":          {"code-1"},
		"redirect_uri":  {"https://app.example.test/callback"},
		"code_verifier": {"verifier-1"},
	}

	var rec *httptest.ResponseRecorder
	for i := 0; i < 31; i++ {
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		router.ServeHTTP(rec, req)
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
}

func TestTokenClientCredentialsAcceptsBasicAuthentication(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(url.Values{
		"grant_type": {"authorization_code"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("client-1", "secret-1")
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm() error = %v", err)
	}

	clientID, secret, provided, err := tokenClientCredentials(req)
	if err != nil {
		t.Fatalf("tokenClientCredentials() error = %v", err)
	}
	if clientID != "client-1" || secret != "secret-1" || !provided {
		t.Fatalf("credentials = (%q, %q, %v), want basic credentials", clientID, secret, provided)
	}
}

func TestTokenClientCredentialsAcceptsFormAuthentication(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(url.Values{
		"client_id":     {"client-1"},
		"client_secret": {"secret-1"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm() error = %v", err)
	}

	clientID, secret, provided, err := tokenClientCredentials(req)
	if err != nil {
		t.Fatalf("tokenClientCredentials() error = %v", err)
	}
	if clientID != "client-1" || secret != "secret-1" || !provided {
		t.Fatalf("credentials = (%q, %q, %v), want form credentials", clientID, secret, provided)
	}
}

func TestTokenClientCredentialsRejectsConflictingAuthenticationMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(url.Values{
		"client_id":     {"other-client"},
		"client_secret": {"other-secret"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("client-1", "secret-1")
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm() error = %v", err)
	}

	if _, _, _, err := tokenClientCredentials(req); err == nil {
		t.Fatal("tokenClientCredentials() error = nil, want conflict error")
	}
}

func TestTokenClientCredentialsKeepsLegacyClientWithoutSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(url.Values{
		"client_id": {"client-1"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatalf("ParseForm() error = %v", err)
	}

	clientID, secret, provided, err := tokenClientCredentials(req)
	if err != nil {
		t.Fatalf("tokenClientCredentials() error = %v", err)
	}
	if clientID != "client-1" || secret != "" || provided {
		t.Fatalf("credentials = (%q, %q, %v), want legacy client without secret", clientID, secret, provided)
	}
}

func TestTokenEndpointAcceptsBasicCredentialsAndInfersEntity(t *testing.T) {
	store := newExchangeTestStore()
	store.client.SecretRequired = true
	router := newExchangeHandlerTestRouter(t, store)
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-1"},
		"redirect_uri":  {store.code.RedirectUri},
		"code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(store.client.ClientID, store.clientSecret)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body = %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}

func TestTokenEndpointReturnsInvalidClientForWrongProvidedSecret(t *testing.T) {
	store := newExchangeTestStore()
	store.client.SecretRequired = true
	router := newExchangeHandlerTestRouter(t, store)
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-1"},
		"redirect_uri":  {store.code.RedirectUri},
		"code_verifier": {"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth2/token", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(store.client.ClientID, "wrong-secret")

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (body = %q)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	var response map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["error"] != "invalid_client" {
		t.Fatalf("error = %#v, want invalid_client", response["error"])
	}
}

func TestUserInfoWithValidToken(t *testing.T) {
	entityID := id.NewULID()
	userID := id.NewULID()
	rawToken := "test-access-token-xyz"
	tokenHash := HashToken(rawToken)

	querier := &mockSSOQuerier{
		token: SSOTokenLookup{
			ID:        id.NewULID(),
			EntityID:  entityID,
			UserID:    userID,
			ClientID:  "client-1",
			TokenType: "access",
			Scopes:    []string{"openid", "profile", "email"},
		},
		user: UserInfoClaims{
			ID:          userID,
			EntityID:    entityID,
			Username:    "alice",
			DisplayName: "Alice Smith",
			Email:       "alice@example.com",
			Phone:       "+15555550100",
			AvatarURL:   "https://example.com/alice.png",
			Locale:      "en-US",
		},
	}
	router := newHandlerTestRouterWithQuerier(t, querier)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("X-IDB-Entity-ID", entityID)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body = %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if querier.lastLookupHash != tokenHash {
		t.Fatalf("lookup hash = %q, want %q", querier.lastLookupHash, tokenHash)
	}

	var claims map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&claims); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if claims["sub"] != userID {
		t.Fatalf("sub = %v, want %q", claims["sub"], userID)
	}
	if claims["name"] != "Alice Smith" {
		t.Fatalf("name = %v, want %q", claims["name"], "Alice Smith")
	}
	if claims["email"] != "alice@example.com" {
		t.Fatalf("email = %v, want %q", claims["email"], "alice@example.com")
	}
	if claims["preferred_username"] != "alice" {
		t.Fatalf("preferred_username = %v, want %q", claims["preferred_username"], "alice")
	}
	// phone is not in scopes (no "phone" scope), so should be absent.
	if _, ok := claims["phone_number"]; ok {
		t.Fatalf("phone_number should be absent when phone scope not granted, got %v", claims["phone_number"])
	}
}

func TestUserInfoInfersEntityFromGloballyUniqueBearerToken(t *testing.T) {
	entityID := id.NewULID()
	userID := id.NewULID()
	rawToken := "globally-unique-access-token"
	querier := &mockSSOQuerier{
		token: SSOTokenLookup{
			ID:        id.NewULID(),
			EntityID:  entityID,
			UserID:    userID,
			ClientID:  "client-1",
			TokenType: "access",
			Scopes:    []string{"openid", "profile"},
		},
		user: UserInfoClaims{
			ID:          userID,
			EntityID:    entityID,
			Username:    "alice",
			DisplayName: "Alice",
		},
	}
	router := newHandlerTestRouterWithQuerier(t, querier)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body = %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if querier.globalLookupCalls != 1 {
		t.Fatalf("global lookup calls = %d, want 1", querier.globalLookupCalls)
	}
	if querier.lastFetchEntity != entityID {
		t.Fatalf("userinfo entity = %q, want %q", querier.lastFetchEntity, entityID)
	}
}

func TestUserInfoRejectsAmbiguousGlobalBearerToken(t *testing.T) {
	querier := &mockSSOQuerier{globalTokenErr: fmt.Errorf("ambiguous token hash")}
	router := newHandlerTestRouterWithQuerier(t, querier)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer shared-token")

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if querier.lastFetchEntity != "" {
		t.Fatalf("userinfo entity = %q, want no user lookup", querier.lastFetchEntity)
	}
}

func TestUserInfoRejectsRevokedToken(t *testing.T) {
	entityID := id.NewULID()
	now := time.Now()
	querier := &mockSSOQuerier{
		token: SSOTokenLookup{
			ID:        id.NewULID(),
			EntityID:  entityID,
			UserID:    id.NewULID(),
			TokenType: "access",
			Scopes:    []string{"openid"},
			RevokedAt: &now,
		},
	}
	router := newHandlerTestRouterWithQuerier(t, querier)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer revoked-token")
	req.Header.Set("X-IDB-Entity-ID", entityID)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUserInfoRejectsNonAccessToken(t *testing.T) {
	entityID := id.NewULID()
	querier := &mockSSOQuerier{
		token: SSOTokenLookup{
			ID:        id.NewULID(),
			EntityID:  entityID,
			UserID:    id.NewULID(),
			TokenType: "id",
			Scopes:    []string{"openid"},
		},
	}
	router := newHandlerTestRouterWithQuerier(t, querier)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer id-token-value")
	req.Header.Set("X-IDB-Entity-ID", entityID)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestRevokeCallsQuerier(t *testing.T) {
	entityID := id.NewULID()
	rawToken := "token-to-revoke"
	tokenHash := HashToken(rawToken)

	querier := &mockSSOQuerier{}
	router := newHandlerTestRouterWithQuerier(t, querier)

	body := url.Values{"token": {rawToken}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth2/revoke", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-IDB-Entity-ID", entityID)

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if querier.lastRevokeHash != tokenHash {
		t.Fatalf("revoke hash = %q, want %q", querier.lastRevokeHash, tokenHash)
	}
}

func TestRevokeInfersEntityFromGloballyUniqueToken(t *testing.T) {
	entityID := id.NewULID()
	rawToken := "globally-unique-token-to-revoke"
	querier := &mockSSOQuerier{globalRevokeEntity: entityID}
	router := newHandlerTestRouterWithQuerier(t, querier)

	body := url.Values{"token": {rawToken}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth2/revoke", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if querier.lastGlobalRevokeHash != HashToken(rawToken) {
		t.Fatalf("global revoke hash = %q, want %q", querier.lastGlobalRevokeHash, HashToken(rawToken))
	}
}

func TestRevokeDoesNotRevokeAmbiguousGlobalToken(t *testing.T) {
	querier := &mockSSOQuerier{globalRevokeErr: fmt.Errorf("ambiguous token hash")}
	router := newHandlerTestRouterWithQuerier(t, querier)
	body := url.Values{"token": {"shared-token"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth2/revoke", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if querier.lastRevokeHash != "" {
		t.Fatalf("scoped revoke hash = %q, want no scoped revoke", querier.lastRevokeHash)
	}
}

func TestRevokeReturns200ForUnknownToken(t *testing.T) {
	entityID := id.NewULID()
	querier := &mockSSOQuerier{revokeErr: fmt.Errorf("no rows")}
	router := newHandlerTestRouterWithQuerier(t, querier)

	body := url.Values{"token": {"unknown-token"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth2/revoke", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-IDB-Entity-ID", entityID)

	router.ServeHTTP(rec, req)

	// RFC 7009: always 200, even if token not found.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestBuildUserInfoResponseScopes(t *testing.T) {
	user := UserInfoClaims{
		ID:          id.NewULID(),
		EntityID:    id.NewULID(),
		Username:    "alice",
		DisplayName: "Alice Smith",
		Email:       "alice@example.com",
		Phone:       "+15555550100",
		AvatarURL:   "https://example.com/alice.png",
		Locale:      "en-US",
	}

	t.Run("openid only returns sub and entity_id", func(t *testing.T) {
		claims := buildUserInfoResponse(user, []string{"openid"})
		if _, ok := claims["name"]; ok {
			t.Fatal("name should not be present without profile scope")
		}
		if _, ok := claims["email"]; ok {
			t.Fatal("email should not be present without email scope")
		}
		if claims["sub"] != user.ID {
			t.Fatalf("sub = %v, want %q", claims["sub"], user.ID)
		}
	})

	t.Run("profile scope includes profile claims", func(t *testing.T) {
		claims := buildUserInfoResponse(user, []string{"openid", "profile"})
		if claims["name"] != "Alice Smith" {
			t.Fatalf("name = %v", claims["name"])
		}
		if claims["locale"] != "en-US" {
			t.Fatalf("locale = %v", claims["locale"])
		}
		if _, ok := claims["email"]; ok {
			t.Fatal("email should not be present without email scope")
		}
	})

	t.Run("email scope includes email", func(t *testing.T) {
		claims := buildUserInfoResponse(user, []string{"openid", "email"})
		if claims["email"] != "alice@example.com" {
			t.Fatalf("email = %v", claims["email"])
		}
		if _, ok := claims["name"]; ok {
			t.Fatal("name should not be present without profile scope")
		}
	})

	t.Run("phone scope includes phone_number", func(t *testing.T) {
		claims := buildUserInfoResponse(user, []string{"openid", "phone"})
		if claims["phone_number"] != "+15555550100" {
			t.Fatalf("phone_number = %v", claims["phone_number"])
		}
	})
}

// mockSSOQuerier is a test double for the TokenLookupStore interface.
type mockSSOQuerier struct {
	token                SSOTokenLookup
	tokenErr             error
	globalTokenErr       error
	user                 UserInfoClaims
	userErr              error
	revokeErr            error
	globalRevokeEntity   string
	globalRevokeErr      error
	lastLookupHash       string
	lastRevokeHash       string
	lastGlobalRevokeHash string
	lastFetchEntity      string
	globalLookupCalls    int
}

func (m *mockSSOQuerier) LookupToken(_ context.Context, _ string, tokenHash string) (SSOTokenLookup, error) {
	m.lastLookupHash = tokenHash
	return m.token, m.tokenErr
}

func (m *mockSSOQuerier) LookupTokenGlobally(_ context.Context, tokenHash string) (SSOTokenLookup, error) {
	m.globalLookupCalls++
	m.lastLookupHash = tokenHash
	return m.token, m.globalTokenErr
}

func (m *mockSSOQuerier) MarkTokenRevoked(_ context.Context, _ string, tokenHash string) error {
	m.lastRevokeHash = tokenHash
	return m.revokeErr
}

func (m *mockSSOQuerier) MarkTokenRevokedGlobally(_ context.Context, tokenHash string) (string, error) {
	m.lastGlobalRevokeHash = tokenHash
	return m.globalRevokeEntity, m.globalRevokeErr
}

func (m *mockSSOQuerier) FetchUserInfo(_ context.Context, entityID string, _ string) (UserInfoClaims, error) {
	m.lastFetchEntity = entityID
	return m.user, m.userErr
}

func newHandlerTestRouter(t *testing.T) http.Handler {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Issuer:     "https://idb.example.test",
		KeyID:      "dev-key-1",
		PrivateKey: key,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	handler := NewHandler(service)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux := chi.NewRouter()
		handler.RegisterRoutes(mux)
		mux.ServeHTTP(w, r)
	})
}

func newHandlerTestRouterWithEphemeralStore(t *testing.T, store ephemeral.Store) http.Handler {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Issuer:     "https://idb.example.test",
		KeyID:      "dev-key-1",
		PrivateKey: key,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	handler := NewHandler(service)
	handler.SetEphemeralStore(store)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux := chi.NewRouter()
		handler.RegisterRoutes(mux)
		mux.ServeHTTP(w, r)
	})
}

func newHandlerTestRouterWithQuerier(t *testing.T, store TokenLookupStore) http.Handler {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	service, err := NewService(ServiceConfig{
		Issuer:           "https://idb.example.test",
		KeyID:            "dev-key-1",
		PrivateKey:       key,
		TokenLookupStore: store,
	})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	handler := NewHandler(service)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux := chi.NewRouter()
		handler.RegisterRoutes(mux)
		mux.ServeHTTP(w, r)
	})
}

func newExchangeHandlerTestRouter(t *testing.T, store Store) http.Handler {
	t.Helper()
	service := newExchangeTestService(t, store)
	handler := NewHandler(service)
	handler.SetEphemeralStore(ephemeral.NewMemoryStore())
	mux := chi.NewRouter()
	handler.RegisterRoutes(mux)
	return mux
}
