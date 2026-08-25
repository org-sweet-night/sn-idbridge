// SPDX-License-Identifier: MIT

package sso

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/smices/open-idb/internal/id"
)

var ErrUserNotEligibleForApplicationSSO = errors.New("user is not eligible for application sso")
var ErrUserInactive = errors.New("user is not active")
var ErrInvalidClient = errors.New("invalid client credentials")

// SSOTokenLookup is the result of looking up an OAuth token by hash.
// Mirrors the columns returned by the GetSSOTokenByHash query.
type SSOTokenLookup struct {
	ID        string
	EntityID  string
	UserID    string
	ClientID  string
	TokenType string
	Scopes    []string
	RevokedAt *time.Time
	ExpiresAt *time.Time
}

// UserInfoClaims holds the user fields needed to build OIDC userinfo responses.
// Mirrors the columns returned by the GetUserInfoForClaims query.
type UserInfoClaims struct {
	ID          string
	EntityID    string
	Username    string
	DisplayName string
	Email       string
	Phone       string
	AvatarURL   string
	Locale      string
}

type Service struct {
	issuer           string
	keyID            string
	privateKey       *rsa.PrivateKey
	store            Store
	tokenLookupStore TokenLookupStore
	fosite           FositeProvider
	now              func() time.Time
	authCodeTTL      time.Duration
	accessTokenTTL   time.Duration
	idTokenTTL       time.Duration
}

type ServiceConfig struct {
	Issuer           string
	KeyID            string
	PrivateKey       *rsa.PrivateKey
	Store            Store
	TokenLookupStore TokenLookupStore
	Queries          Store
	Querier          TokenLookupStore
	Fosite           FositeProvider
	Now              func() time.Time
	AuthCodeTTL      time.Duration
	AccessTokenTTL   time.Duration
	IDTokenTTL       time.Duration
}

type AuthorizeInput struct {
	EntityID            string
	ClientID            string
	RedirectURI         string
	ResponseType        string
	Scopes              []string
	State               string
	Nonce               string
	CodeChallenge       string
	CodeChallengeMethod string
}

type AuthorizeDecision struct {
	ClientID    string
	RedirectURI string
	Scopes      []string
	State       string
	Nonce       string
}

type TokenInput struct {
	EntityID             string
	ClientID             string
	ClientSecret         string
	ClientSecretProvided bool
	Code                 string
	RedirectURI          string
	CodeVerifier         string
}

// ClientCredentialsInput is a non-interactive token request made by a
// confidential client on behalf of itself. It is intentionally separate from
// TokenInput because it has no end-user, authorization code, redirect URI, or
// PKCE verifier.
type ClientCredentialsInput struct {
	EntityID             string
	ClientID             string
	ClientSecret         string
	ClientSecretProvided bool
	Scopes               []string
}

type TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token,omitempty"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("issuer is required")
	}
	if cfg.KeyID == "" {
		return nil, fmt.Errorf("key id is required")
	}
	if cfg.PrivateKey == nil {
		return nil, fmt.Errorf("private key is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AuthCodeTTL == 0 {
		cfg.AuthCodeTTL = 5 * time.Minute
	}
	if cfg.AccessTokenTTL == 0 {
		cfg.AccessTokenTTL = 15 * time.Minute
	}
	if cfg.IDTokenTTL == 0 {
		cfg.IDTokenTTL = 15 * time.Minute
	}
	if cfg.Store == nil {
		cfg.Store = cfg.Queries
	}
	if cfg.TokenLookupStore == nil {
		cfg.TokenLookupStore = cfg.Querier
	}

	return &Service{
		issuer:           strings.TrimRight(cfg.Issuer, "/"),
		keyID:            cfg.KeyID,
		privateKey:       cfg.PrivateKey,
		store:            cfg.Store,
		tokenLookupStore: cfg.TokenLookupStore,
		fosite:           cfg.Fosite,
		now:              cfg.Now,
		authCodeTTL:      cfg.AuthCodeTTL,
		accessTokenTTL:   cfg.AccessTokenTTL,
		idTokenTTL:       cfg.IDTokenTTL,
	}, nil
}

func (s *Service) DiscoveryDocument() DiscoveryDocument {
	return BuildDiscoveryDocument(s.issuer)
}

func (s *Service) DiscoveryDocumentWithEndpointPrefix(endpointPrefix string) DiscoveryDocument {
	return BuildDiscoveryDocumentWithEndpointPrefix(s.issuer, endpointPrefix)
}

func (s *Service) JWKS() JWKS {
	jwk, err := PublicJWK(s.keyID, s.privateKey)
	if err != nil {
		return JWKS{}
	}
	return JWKS{Keys: []JWK{jwk}}
}

func (s *Service) ValidateAuthorizeRequest(ctx context.Context, input AuthorizeInput) (AuthorizeDecision, error) {
	if input.EntityID == "" || input.ClientID == "" || input.RedirectURI == "" {
		return AuthorizeDecision{}, fmt.Errorf("entity id, client id, and redirect uri are required")
	}
	if input.ResponseType != "code" {
		return AuthorizeDecision{}, fmt.Errorf("unsupported response type %q", input.ResponseType)
	}
	if input.CodeChallengeMethod != CodeChallengeS256 {
		return AuthorizeDecision{}, fmt.Errorf("code challenge method must be S256")
	}
	if input.CodeChallenge == "" {
		return AuthorizeDecision{}, fmt.Errorf("code challenge is required")
	}
	if s.store != nil {
		client, err := s.getActiveClient(ctx, input.EntityID, input.ClientID)
		if err != nil {
			return AuthorizeDecision{}, err
		}
		if !containsString(client.RedirectUris, input.RedirectURI) {
			return AuthorizeDecision{}, fmt.Errorf("redirect uri is not allowed")
		}
		effectiveScopes, err := effectiveAuthorizeScopes(input.Scopes, client.AllowedScopes)
		if err != nil {
			return AuthorizeDecision{}, err
		}
		if !containsString(effectiveScopes, "openid") {
			return AuthorizeDecision{}, fmt.Errorf("scope must include openid")
		}
		input.Scopes = effectiveScopes
	}
	if s.fosite != nil {
		req := authorizeHTTPRequest(input)
		if _, err := s.fosite.NewAuthorizeRequest(ContextWithEntityID(ctx, input.EntityID), req); err != nil {
			return AuthorizeDecision{}, err
		}
	}

	return AuthorizeDecision{
		ClientID:    input.ClientID,
		RedirectURI: input.RedirectURI,
		Scopes:      input.Scopes,
		State:       input.State,
		Nonce:       input.Nonce,
	}, nil
}

func (s *Service) IssueAuthorizationCode(ctx context.Context, input AuthorizeInput, subject TokenSubject) (string, error) {
	if s.store == nil {
		return "", fmt.Errorf("sso store is required")
	}
	decision, err := s.ValidateAuthorizeRequest(ctx, input)
	if err != nil {
		return "", err
	}
	input.Scopes = decision.Scopes
	client, err := s.getActiveClient(ctx, input.EntityID, input.ClientID)
	if err != nil {
		return "", err
	}
	userID, err := ulidValue(subject.UserID)
	if err != nil {
		return "", err
	}

	// Check user lifecycle status is active (spec authorization order #1)
	entityID, err := ulidValue(input.EntityID)
	if err != nil {
		return "", err
	}
	lifecycleStatus, err := s.store.GetUserLifecycleStatus(ctx, generated.GetUserLifecycleStatusParams{
		EntityID: entityID,
		ID:       userID,
	})
	if err != nil {
		return "", fmt.Errorf("user not found")
	}
	if lifecycleStatus != "active" {
		return "", fmt.Errorf("%w: status is %s", ErrUserInactive, lifecycleStatus)
	}

	claims, err := s.store.GetUserClaimsForToken(ctx, generated.GetUserClaimsForTokenParams{
		EntityID: entityID,
		ID:       userID,
	})
	if err != nil {
		return "", fmt.Errorf("user claims not found")
	}
	if !hasExternalIdentitySource(parseIdentitySources(claims.IdentitySources)) {
		return "", ErrUserNotEligibleForApplicationSSO
	}

	allowed, err := s.store.HasApplicationAccess(ctx, generated.HasApplicationAccessParams{
		EntityID:      client.EntityID,
		ApplicationID: client.ApplicationID,
		SubjectID:     userID,
	})
	if err != nil {
		return "", err
	}
	if !allowed.Bool {
		return "", fmt.Errorf("application access denied")
	}

	code, err := RandomURLSafeToken(32)
	if err != nil {
		return "", err
	}

	_, err = s.store.CreateAuthorizationCode(ctx, generated.CreateAuthorizationCodeParams{
		EntityID:      entityID,
		ClientID:      input.ClientID,
		UserID:        userID,
		CodeHash:      HashToken(code),
		RedirectUri:   input.RedirectURI,
		Scopes:        input.Scopes,
		CodeChallenge: input.CodeChallenge,
		Nonce:         textValue(input.Nonce),
		ExpiresAt: pgtype.Timestamptz{
			Time:  s.now().Add(s.authCodeTTL),
			Valid: true,
		},
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

func (s *Service) ExchangeCode(ctx context.Context, input TokenInput) (TokenResponse, error) {
	if s.store == nil {
		return TokenResponse{}, fmt.Errorf("sso store is required")
	}
	if input.ClientID == "" || input.Code == "" || input.RedirectURI == "" || input.CodeVerifier == "" {
		return TokenResponse{}, fmt.Errorf("client id, code, redirect uri, and code verifier are required")
	}

	codeHash := HashToken(input.Code)
	codeRecord, err := s.authorizationCodeForExchange(ctx, input.EntityID, codeHash)
	if err != nil {
		return TokenResponse{}, err
	}
	entityID, err := ulidValue(codeRecord.EntityID)
	if err != nil {
		return TokenResponse{}, err
	}
	if codeRecord.UsedAt.Valid {
		return TokenResponse{}, fmt.Errorf("authorization code has already been used")
	}
	if !codeRecord.ExpiresAt.Valid || !s.now().Before(codeRecord.ExpiresAt.Time) {
		return TokenResponse{}, fmt.Errorf("authorization code has expired")
	}
	if codeRecord.ClientID != input.ClientID {
		return TokenResponse{}, fmt.Errorf("authorization code client mismatch")
	}
	if codeRecord.RedirectUri != input.RedirectURI {
		return TokenResponse{}, fmt.Errorf("authorization code redirect uri mismatch")
	}
	if codeRecord.CodeChallengeMethod != CodeChallengeS256 {
		return TokenResponse{}, fmt.Errorf("unsupported code challenge method")
	}
	if !VerifyS256CodeChallenge(input.CodeVerifier, codeRecord.CodeChallenge) {
		return TokenResponse{}, fmt.Errorf("code verifier does not match code challenge")
	}

	client, err := s.getActiveClient(ctx, entityID, input.ClientID)
	if err != nil {
		return TokenResponse{}, err
	}
	secretMatches := client.ClientSecretHash.Valid && constantTimeSecretEqual(client.ClientSecretHash.String, input.ClientSecret)
	if client.SecretRequired && (!input.ClientSecretProvided || !secretMatches) {
		return TokenResponse{}, ErrInvalidClient
	}
	// Clients predating the secret_required marker retain anonymous endpoint
	// compatibility, but a secret they do provide must still authenticate.
	if !client.SecretRequired && input.ClientSecretProvided && !secretMatches {
		return TokenResponse{}, ErrInvalidClient
	}

	sessionID, err := RandomURLSafeToken(24)
	if err != nil {
		return TokenResponse{}, err
	}
	subject := TokenSubject{
		EntityID:  entityID,
		UserID:    ulidString(codeRecord.UserID),
		ClientID:  input.ClientID,
		SessionID: sessionID,
	}
	if codeRecord.Nonce.Valid {
		subject.Nonce = codeRecord.Nonce.String
	}

	// Enrich subject with user claims and roles for token embedding
	if claims, err := s.store.GetUserClaimsForToken(ctx, generated.GetUserClaimsForTokenParams{
		EntityID: entityID,
		ID:       codeRecord.UserID,
	}); err == nil {
		subject.PreferredUsername = claims.Username
		subject.Name = claims.DisplayName
		if claims.Email.Valid {
			subject.Email = claims.Email.String
		}
		if claims.Phone.Valid {
			subject.PhoneNumber = claims.Phone.String
		}
		if claims.AvatarUrl.Valid {
			subject.Picture = claims.AvatarUrl.String
		}
		if claims.Locale.Valid {
			subject.Locale = claims.Locale.String
		}
		subject.IdentitySources = parseIdentitySources(claims.IdentitySources)
	}
	if roles, err := s.store.GetUserRolesForToken(ctx, generated.GetUserRolesForTokenParams{
		EntityID: entityID,
		UserID:   codeRecord.UserID,
	}); err == nil {
		subject.Roles = roles
	}

	// Query real version numbers for cache invalidation by downstream apps
	if permVersion, err := s.store.GetPermissionsVersion(ctx, entityID); err == nil {
		subject.PermissionsVersion = permVersion
	}
	if rsVersion, err := s.store.GetResourceScopesVersion(ctx, entityID); err == nil {
		subject.ResourceScopesVersion = rsVersion
	}

	now := s.now()
	accessClaims := BuildAccessTokenClaims(s.issuer, subject, codeRecord.Scopes, now, s.accessTokenTTL)
	idClaims := BuildIDTokenClaims(s.issuer, subject, now, s.idTokenTTL)

	accessToken, err := SignRS256(accessClaims, s.keyID, s.privateKey)
	if err != nil {
		return TokenResponse{}, err
	}
	idToken, err := SignRS256(idClaims, s.keyID, s.privateKey)
	if err != nil {
		return TokenResponse{}, err
	}

	if _, err := s.store.FinalizeAuthorizationCodeExchange(ctx, generated.FinalizeAuthorizationCodeExchangeParams{
		EntityID:             entityID,
		CodeHash:             codeHash,
		ClientSecretProvided: input.ClientSecretProvided,
		ClientSecret:         pgtype.Text{String: input.ClientSecret, Valid: input.ClientSecretProvided},
		AccessTokenHash:      HashToken(accessToken),
		AccessTokenExpiresAt: pgtype.Timestamptz{Time: now.Add(s.accessTokenTTL), Valid: true},
		IDTokenHash:          HashToken(idToken),
		IDTokenExpiresAt:     pgtype.Timestamptz{Time: now.Add(s.idTokenTTL), Valid: true},
	}); err != nil {
		return TokenResponse{}, fmt.Errorf("authorization code has already been used, expired, or its client is disabled")
	}

	return TokenResponse{
		AccessToken: accessToken,
		IDToken:     idToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(s.accessTokenTTL.Seconds()),
		Scope:       strings.Join(codeRecord.Scopes, " "),
	}, nil
}

// IssueClientCredentials issues a short-lived access token for a confidential
// OIDC client. The token subject is the internal client ULID, never an end
// user. Only scopes explicitly requested by the caller and allowed for that
// client may be issued.
func (s *Service) IssueClientCredentials(ctx context.Context, input ClientCredentialsInput) (TokenResponse, error) {
	if s.store == nil {
		return TokenResponse{}, fmt.Errorf("sso store is required")
	}
	if input.EntityID == "" || input.ClientID == "" || input.ClientSecret == "" || !input.ClientSecretProvided {
		return TokenResponse{}, ErrInvalidClient
	}
	if len(input.Scopes) == 0 {
		return TokenResponse{}, fmt.Errorf("scope is required for client_credentials")
	}

	client, err := s.getActiveClient(ctx, input.EntityID, input.ClientID)
	if err != nil {
		return TokenResponse{}, err
	}
	if !containsString(client.GrantTypes, "client_credentials") {
		return TokenResponse{}, fmt.Errorf("client_credentials is not enabled for this client")
	}
	if !client.ClientSecretHash.Valid || !constantTimeSecretEqual(client.ClientSecretHash.String, input.ClientSecret) {
		return TokenResponse{}, ErrInvalidClient
	}
	scopes, err := effectiveClientCredentialScopes(input.Scopes, client.AllowedScopes)
	if err != nil {
		return TokenResponse{}, err
	}

	now := s.now()
	subject := TokenSubject{
		EntityID:          input.EntityID,
		UserID:            client.ID,
		ClientID:          input.ClientID,
		PreferredUsername: input.ClientID,
	}
	accessToken, err := SignRS256(BuildAccessTokenClaims(s.issuer, subject, scopes, now, s.accessTokenTTL), s.keyID, s.privateKey)
	if err != nil {
		return TokenResponse{}, err
	}
	if _, err := s.store.CreateOAuthToken(ctx, generated.CreateOAuthTokenParams{
		EntityID:  input.EntityID,
		UserID:    client.ID,
		ClientID:  input.ClientID,
		TokenType: "access",
		TokenHash: HashToken(accessToken),
		Scopes:    scopes,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(s.accessTokenTTL), Valid: true},
	}); err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int64(s.accessTokenTTL.Seconds()),
		Scope:       strings.Join(scopes, " "),
	}, nil
}

func (s *Service) authorizationCodeForExchange(ctx context.Context, entityIDValue string, codeHash string) (generated.OauthAuthorizationCode, error) {
	if entityIDValue == "" {
		return s.store.GetAuthorizationCodeByHash(ctx, codeHash)
	}
	entityID, err := ulidValue(entityIDValue)
	if err != nil {
		return generated.OauthAuthorizationCode{}, err
	}
	return s.store.GetAuthorizationCode(ctx, generated.GetAuthorizationCodeParams{
		EntityID: entityID,
		CodeHash: codeHash,
	})
}

// IntrospectToken looks up an OAuth token by its hash and validates it.
// Returns the token record if found, not revoked, and not expired.
func (s *Service) IntrospectToken(ctx context.Context, entityID, tokenHash string) (SSOTokenLookup, error) {
	if s.tokenLookupStore == nil {
		return SSOTokenLookup{}, fmt.Errorf("token introspection is not configured")
	}
	var (
		token SSOTokenLookup
		err   error
	)
	if entityID == "" {
		globalStore, ok := s.tokenLookupStore.(GlobalTokenLookupStore)
		if !ok {
			return SSOTokenLookup{}, fmt.Errorf("global token introspection is not configured")
		}
		token, err = globalStore.LookupTokenGlobally(ctx, tokenHash)
	} else {
		entityULID, parseErr := ulidValue(entityID)
		if parseErr != nil {
			return SSOTokenLookup{}, fmt.Errorf("invalid entity id: %w", parseErr)
		}
		token, err = s.tokenLookupStore.LookupToken(ctx, entityULID, tokenHash)
	}
	if err != nil {
		return SSOTokenLookup{}, err
	}
	if token.RevokedAt != nil {
		return SSOTokenLookup{}, fmt.Errorf("token has been revoked")
	}
	if token.ExpiresAt != nil && !s.now().Before(*token.ExpiresAt) {
		return SSOTokenLookup{}, fmt.Errorf("token has expired")
	}
	return token, nil
}

// RevokeToken marks an OAuth token as revoked. Per RFC 7009, this does not
// return an error if the token is not found — the caller should always
// respond with 200 OK to the client.
func (s *Service) RevokeToken(ctx context.Context, entityID, tokenHash string) error {
	s.revokeTokenAndResolveEntity(ctx, entityID, tokenHash)
	return nil
}

func (s *Service) revokeTokenAndResolveEntity(ctx context.Context, entityID, tokenHash string) string {
	if s.tokenLookupStore == nil {
		return ""
	}
	if entityID == "" {
		globalStore, ok := s.tokenLookupStore.(GlobalTokenLookupStore)
		if !ok {
			return ""
		}
		resolvedEntityID, err := globalStore.MarkTokenRevokedGlobally(ctx, tokenHash)
		if err != nil {
			return ""
		}
		return resolvedEntityID
	}
	entityULID, err := ulidValue(entityID)
	if err != nil {
		return ""
	}
	// Ignore errors — RFC 7009 requires 200 OK even for invalid tokens.
	_ = s.tokenLookupStore.MarkTokenRevoked(ctx, entityULID, tokenHash)
	return entityULID
}

// GetUserInfo retrieves user fields for OIDC userinfo claims.
func (s *Service) GetUserInfo(ctx context.Context, entityID, userID string) (UserInfoClaims, error) {
	if s.tokenLookupStore == nil {
		return UserInfoClaims{}, fmt.Errorf("user info is not configured")
	}
	entityULID, err := ulidValue(entityID)
	if err != nil {
		return UserInfoClaims{}, fmt.Errorf("invalid entity id: %w", err)
	}
	userULID, err := ulidValue(userID)
	if err != nil {
		return UserInfoClaims{}, fmt.Errorf("invalid user id: %w", err)
	}
	return s.tokenLookupStore.FetchUserInfo(ctx, entityULID, userULID)
}

func authorizeHTTPRequest(input AuthorizeInput) *http.Request {
	values := url.Values{}
	values.Set("client_id", input.ClientID)
	values.Set("redirect_uri", input.RedirectURI)
	values.Set("response_type", input.ResponseType)
	values.Set("scope", strings.Join(input.Scopes, " "))
	values.Set("state", input.State)
	values.Set("nonce", input.Nonce)
	values.Set("code_challenge", input.CodeChallenge)
	values.Set("code_challenge_method", input.CodeChallengeMethod)

	req, _ := http.NewRequest(http.MethodGet, "/oauth2/authorize?"+values.Encode(), nil)
	return req
}

func (s *Service) getActiveClient(ctx context.Context, entityIDValue string, clientID string) (generated.OidcClient, error) {
	entityID, err := ulidValue(entityIDValue)
	if err != nil {
		return generated.OidcClient{}, err
	}
	client, err := s.store.GetOIDCClientByClientID(ctx, generated.GetOIDCClientByClientIDParams{
		EntityID: entityID,
		ClientID: clientID,
	})
	if err != nil {
		return generated.OidcClient{}, err
	}
	if client.Status != "active" {
		return generated.OidcClient{}, fmt.Errorf("client is disabled")
	}
	return client, nil
}

func (s *Service) canRedirectAuthorizationError(ctx context.Context, entityIDValue, clientID, redirectURI string) bool {
	if s.store == nil || entityIDValue == "" || clientID == "" || redirectURI == "" {
		return false
	}
	client, err := s.getActiveClient(ctx, entityIDValue, clientID)
	if err != nil {
		return false
	}
	return containsString(client.RedirectUris, redirectURI)
}

func constantTimeSecretEqual(expected, provided string) bool {
	expectedDigest := sha256.Sum256([]byte(expected))
	providedDigest := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedDigest[:], providedDigest[:]) == 1
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func effectiveAuthorizeScopes(requested []string, allowed []string) ([]string, error) {
	if len(allowed) == 0 {
		return nil, fmt.Errorf("client has no allowed scopes")
	}
	if len(requested) > 0 && !isSubset(requested, allowed) {
		return nil, fmt.Errorf("requested scope is not allowed")
	}
	if len(requested) == 0 {
		return uniqueStrings(allowed), nil
	}
	return uniqueStrings(requested), nil
}

func effectiveClientCredentialScopes(requested []string, allowed []string) ([]string, error) {
	requested = uniqueStrings(requested)
	if len(requested) == 0 {
		return nil, fmt.Errorf("scope is required for client_credentials")
	}
	if !isSubset(requested, allowed) {
		return nil, fmt.Errorf("requested scope is not allowed")
	}
	return requested, nil
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func isSubset(values []string, allowed []string) bool {
	for _, value := range values {
		if !containsString(allowed, value) {
			return false
		}
	}
	return true
}

func ulidValue(value string) (string, error) {
	if err := id.ValidateULID(value); err != nil {
		return "", err
	}
	return value, nil
}

func textValue(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

func ulidString(value string) string {
	return value
}

// parseIdentitySources converts the json_agg result (interface{}) to []string.
// The query returns JSON like ["feishu","ldap"] or "[]" for empty.
func parseIdentitySources(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	// json_agg returns []byte when scanned into interface{}
	switch v := raw.(type) {
	case []byte:
		var out []string
		if err := json.Unmarshal(v, &out); err != nil {
			return nil
		}
		return out
	case string:
		var out []string
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			return nil
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func hasExternalIdentitySource(sources []string) bool {
	for _, source := range sources {
		if source != "" && source != "local" {
			return true
		}
	}
	return false
}
