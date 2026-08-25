// SPDX-License-Identifier: MIT

package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	auditmodel "github.com/smices/open-idb/internal/audit/model"
	"github.com/smices/open-idb/internal/auth"
	"github.com/smices/open-idb/internal/ephemeral"
	"github.com/smices/open-idb/internal/id"
)

// AuditEventWriter records audit events. A nil value disables audit logging.
// *audit.Service satisfies this interface.
type AuditEventWriter interface {
	Write(ctx context.Context, event auditmodel.Event) error
}

type Handler struct {
	service   *Service
	audit     AuditEventWriter
	loginURL  string // URL to redirect unauthenticated users to (e.g., "/login")
	ephemeral ephemeral.Store
}

// NewHandler creates an SSO Handler. An optional AuditEventWriter may be
// provided to enable audit logging of authorize and revoke events. Existing
// callers that do not pass a writer are unaffected.
func NewHandler(service *Service, writers ...AuditEventWriter) Handler {
	h := Handler{service: service, loginURL: "/login"}
	if len(writers) > 0 {
		h.audit = writers[0]
	}
	return h
}

// SetLoginURL configures the URL to redirect unauthenticated users to.
func (h *Handler) SetLoginURL(loginURL string) {
	h.loginURL = loginURL
}

func (h *Handler) SetEphemeralStore(store ephemeral.Store) {
	h.ephemeral = store
}

func (h Handler) RegisterRoutes(r chi.Router) {
	r.Get("/.well-known/openid-configuration", h.discovery)
	r.Get("/.well-known/jwks.json", h.jwks)
	r.Get("/oauth2/authorize", h.authorize)
	r.Post("/oauth2/token", h.token)
	r.Get("/oauth2/userinfo", h.userinfo)
	r.Post("/oauth2/revoke", h.revoke)

	r.Get("/api/.well-known/openid-configuration", h.apiDiscovery)
	r.Get("/api/.well-known/jwks.json", h.jwks)
	r.Get("/api/oauth2/authorize", h.authorize)
	r.Post("/api/oauth2/token", h.token)
	r.Get("/api/oauth2/userinfo", h.userinfo)
	r.Post("/api/oauth2/revoke", h.revoke)
}

func (h Handler) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.DiscoveryDocument())
}

func (h Handler) apiDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.DiscoveryDocumentWithEndpointPrefix("/api"))
}

func (h Handler) jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.JWKS())
}

func (h Handler) authorize(w http.ResponseWriter, r *http.Request) {
	subject, err := h.resolveSubject(r)
	if err != nil {
		loginRedirect := h.buildLoginRedirectURL(r)
		http.Redirect(w, r, loginRedirect, http.StatusFound)
		return
	}

	input := AuthorizeInput{
		EntityID:            subject.EntityID,
		ClientID:            r.URL.Query().Get("client_id"),
		RedirectURI:         r.URL.Query().Get("redirect_uri"),
		ResponseType:        r.URL.Query().Get("response_type"),
		Scopes:              splitScopes(r.URL.Query().Get("scope")),
		State:               r.URL.Query().Get("state"),
		Nonce:               r.URL.Query().Get("nonce"),
		CodeChallenge:       r.URL.Query().Get("code_challenge"),
		CodeChallengeMethod: r.URL.Query().Get("code_challenge_method"),
	}
	traceID := id.NewULID()
	code, err := h.service.IssueAuthorizationCode(r.Context(), input, subject)
	if err != nil {
		if shouldRestartAuthorizeLogin(err) {
			clearSessionCookie(w)
			http.Redirect(w, r, h.buildLoginRedirectURL(r), http.StatusFound)
			return
		}
		h.writeAudit(r, auditmodel.Event{
			EntityID:     subject.EntityID,
			ActorUserID:  subject.UserID,
			ActorType:    "user",
			Action:       auditmodel.ActionAuthorizeDenied,
			ResourceType: "oauth2_client",
			ResourceID:   input.ClientID,
			IP:           r.RemoteAddr,
			UserAgent:    r.UserAgent(),
			TraceID:      traceID,
			After:        map[string]string{"reason": err.Error(), "client_id": input.ClientID},
		})
		if h.service.canRedirectAuthorizationError(r.Context(), input.EntityID, input.ClientID, input.RedirectURI) {
			params := map[string]string{
				"error":             "invalid_request",
				"error_description": err.Error(),
			}
			if input.State != "" {
				params["state"] = input.State
			}
			if redirectURL, redirectErr := authorizationRedirectURL(input.RedirectURI, params); redirectErr == nil {
				http.Redirect(w, r, redirectURL, http.StatusFound)
				return
			}
		}
		writeError(w, http.StatusBadRequest, "invalid_authorize_request", err.Error())
		return
	}

	h.writeAudit(r, auditmodel.Event{
		EntityID:     subject.EntityID,
		ActorUserID:  subject.UserID,
		ActorType:    "user",
		Action:       auditmodel.ActionAuthorizeSuccess,
		ResourceType: "oauth2_client",
		ResourceID:   input.ClientID,
		IP:           r.RemoteAddr,
		UserAgent:    r.UserAgent(),
		TraceID:      traceID,
		After:        map[string]interface{}{"client_id": input.ClientID, "scopes": input.Scopes},
	})

	params := map[string]string{"code": code}
	if input.State != "" {
		params["state"] = input.State
	}
	redirectURL, err := authorizationRedirectURL(input.RedirectURI, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "unable to build authorization redirect")
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func authorizationRedirectURL(redirectURI string, params map[string]string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	query := u.Query()
	for key, value := range params {
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func shouldRestartAuthorizeLogin(err error) bool {
	return errors.Is(err, ErrUserInactive) || errors.Is(err, ErrUserNotEligibleForApplicationSSO)
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "idb_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h Handler) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_token_request", "invalid form body")
		return
	}
	grantType := r.PostForm.Get("grant_type")
	if grantType != "authorization_code" && grantType != "client_credentials" {
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or client_credentials")
		return
	}
	clientID, clientSecret, clientSecretProvided, err := tokenClientCredentials(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth2/token"`)
		writeError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}
	limitKey := ephemeral.Key("rate:token_exchange", clientID, r.RemoteAddr)
	limit, limitErr := ephemeral.CheckLimit(r.Context(), h.ephemeral, limitKey, 30, time.Minute)
	if limitErr != nil {
		writeError(w, http.StatusServiceUnavailable, "rate_limit_unavailable", "token exchange rate limit is unavailable")
		return
	}
	if !limit.Allowed {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many token exchange attempts")
		return
	}

	entityID := firstNonEmpty(r.Header.Get("X-IDB-Entity-ID"), r.PostForm.Get("entity_id"))
	var response TokenResponse
	if grantType == "client_credentials" {
		response, err = h.service.IssueClientCredentials(r.Context(), ClientCredentialsInput{
			EntityID:             entityID,
			ClientID:             clientID,
			ClientSecret:         clientSecret,
			ClientSecretProvided: clientSecretProvided,
			Scopes:               splitScopes(r.PostForm.Get("scope")),
		})
	} else {
		response, err = h.service.ExchangeCode(r.Context(), TokenInput{
			EntityID:             entityID,
			ClientID:             clientID,
			ClientSecret:         clientSecret,
			ClientSecretProvided: clientSecretProvided,
			Code:                 r.PostForm.Get("code"),
			RedirectURI:          r.PostForm.Get("redirect_uri"),
			CodeVerifier:         r.PostForm.Get("code_verifier"),
		})
	}
	if err != nil {
		if errors.Is(err, ErrInvalidClient) {
			w.Header().Set("WWW-Authenticate", `Basic realm="oauth2/token"`)
			writeError(w, http.StatusUnauthorized, "invalid_client", err.Error())
			return
		}
		code := "invalid_grant"
		if grantType == "client_credentials" {
			code = "invalid_scope"
		}
		writeError(w, http.StatusBadRequest, code, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, response)
}

func tokenClientCredentials(r *http.Request) (clientID string, clientSecret string, clientSecretProvided bool, err error) {
	formClientID := r.PostForm.Get("client_id")
	formSecrets, formSecretProvided := r.PostForm["client_secret"]
	formSecret := ""
	if formSecretProvided && len(formSecrets) > 0 {
		formSecret = formSecrets[0]
	}

	basicClientID, basicSecret, basicProvided := r.BasicAuth()
	if !basicProvided {
		if scheme, _, found := strings.Cut(r.Header.Get("Authorization"), " "); found && strings.EqualFold(scheme, "Basic") {
			return "", "", false, fmt.Errorf("invalid basic client credentials")
		}
		return formClientID, formSecret, formSecretProvided, nil
	}
	if formClientID != "" && formClientID != basicClientID {
		return "", "", false, fmt.Errorf("conflicting client identifiers")
	}
	if formSecretProvided && formSecret != basicSecret {
		return "", "", false, fmt.Errorf("conflicting client secrets")
	}
	return basicClientID, basicSecret, true, nil
}

func (h Handler) userinfo(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="missing or malformed bearer token"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "missing or malformed bearer token")
		return
	}
	rawToken := strings.TrimPrefix(authHeader, "Bearer ")
	if rawToken == "" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="empty bearer token"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "empty bearer token")
		return
	}

	tokenHash := HashToken(rawToken)
	entityID := r.Header.Get("X-IDB-Entity-ID")
	tokenRecord, err := h.service.IntrospectToken(r.Context(), entityID, tokenHash)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="token is invalid, revoked, or expired"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "token is invalid, revoked, or expired")
		return
	}
	if tokenRecord.TokenType != "access" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="token type is not an access token"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "token type is not an access token")
		return
	}

	userInfo, err := h.service.GetUserInfo(r.Context(), tokenRecord.EntityID, tokenRecord.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "unable to load user info")
		return
	}

	claims := buildUserInfoResponse(userInfo, tokenRecord.Scopes)
	writeJSON(w, http.StatusOK, claims)
}

func (h Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		// RFC 7009: always return 200, even on malformed requests.
		w.WriteHeader(http.StatusOK)
		return
	}
	rawToken := r.PostForm.Get("token")
	if rawToken == "" {
		// RFC 7009: if no token is provided, treat as if revocation succeeded.
		w.WriteHeader(http.StatusOK)
		return
	}

	entityID := firstNonEmpty(r.Header.Get("X-IDB-Entity-ID"), r.PostForm.Get("entity_id"))
	// token_type_hint is accepted but not used to change behavior — we hash
	// and look up regardless. Valid values: "access_token", "refresh_token".
	tokenHash := HashToken(rawToken)
	entityID = h.service.revokeTokenAndResolveEntity(r.Context(), entityID, tokenHash)
	if entityID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	traceID := id.NewULID()
	h.writeAudit(r, auditmodel.Event{
		EntityID:     entityID,
		ActorType:    "user",
		Action:       auditmodel.ActionTokenRevoke,
		ResourceType: "oauth2_token",
		ResourceID:   tokenHash,
		IP:           r.RemoteAddr,
		UserAgent:    r.UserAgent(),
		TraceID:      traceID,
		After:        map[string]string{"token_hash": tokenHash},
	})
	w.WriteHeader(http.StatusOK)
}

// writeAudit records an audit event if an audit writer is configured.
// Errors are silently ignored so they do not disrupt the request flow.
func (h Handler) writeAudit(r *http.Request, event auditmodel.Event) {
	if h.audit == nil {
		return
	}
	_ = h.audit.Write(r.Context(), event)
}

// resolveSubject identifies the user from a valid idb_session cookie.
func (h Handler) resolveSubject(r *http.Request) (TokenSubject, error) {
	if cookie, err := r.Cookie("idb_session"); err == nil && cookie.Value != "" {
		session, err := auth.ResolveSession(r.Context(), cookie.Value)
		if err == nil {
			return TokenSubject{
				EntityID:          session.EntityID,
				UserID:            session.UserID,
				PreferredUsername: session.Username,
				Name:              session.DisplayName,
				Locale:            "en-US",
				IdentitySources:   []string{"session"},
			}, nil
		}
	}
	return TokenSubject{}, fmt.Errorf("idb_session cookie is required")
}

// buildLoginRedirectURL constructs the login page URL with a return_to
// parameter so the user is sent back to /oauth2/authorize after login.
func (h Handler) buildLoginRedirectURL(r *http.Request) string {
	returnTo := r.URL.RequestURI()
	loginURL := h.loginURL
	if loginURL == "" {
		loginURL = "/login"
	}

	u, err := url.Parse(loginURL)
	if err != nil {
		return loginURL
	}
	q := u.Query()
	q.Set("return_to", returnTo)
	u.RawQuery = q.Encode()
	return u.String()
}

func splitScopes(scope string) []string {
	if scope == "" {
		return nil
	}
	return strings.Fields(scope)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": message,
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// buildUserInfoResponse assembles the OIDC UserInfo response based on the
// user record and the scopes granted to the access token.
// See: https://openid.net/specs/openid-connect-core-1_0.html#UserInfoResponse
func buildUserInfoResponse(user UserInfoClaims, scopes []string) map[string]interface{} {
	claims := map[string]interface{}{
		"sub":       user.ID,
		"entity_id": user.EntityID,
	}
	if hasScope(scopes, "profile") {
		if user.DisplayName != "" {
			claims["name"] = user.DisplayName
		}
		if user.Username != "" {
			claims["preferred_username"] = user.Username
		}
		if user.AvatarURL != "" {
			claims["picture"] = user.AvatarURL
		}
		if user.Locale != "" {
			claims["locale"] = user.Locale
		}
	}
	if hasScope(scopes, "email") && user.Email != "" {
		claims["email"] = user.Email
	}
	if hasScope(scopes, "phone") && user.Phone != "" {
		claims["phone_number"] = user.Phone
	}
	return claims
}

func hasScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
}
