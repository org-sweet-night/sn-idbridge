// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/db/generated"
)

// OIDCClientResponse represents an OIDC client in API responses.
type OIDCClientResponse struct {
	ID                 string   `json:"id"`
	EntityID           string   `json:"entity_id"`
	ApplicationID      string   `json:"application_id"`
	ClientID           string   `json:"client_id"`
	ClientSecret       string   `json:"client_secret,omitempty"`
	RedirectURIs       []string `json:"redirect_uris"`
	AllowedScopes      []string `json:"allowed_scopes"`
	GrantTypes         []string `json:"grant_types"`
	ResponseTypes      []string `json:"response_types"`
	PKCERequired       bool     `json:"pkce_required"`
	SecretRequired     bool     `json:"secret_required"`
	WorkplaceProvider  string   `json:"workplace_provider"`
	WorkplaceAppID     string   `json:"workplace_app_id"`
	WorkplaceAppSecret string   `json:"workplace_app_secret,omitempty"`
	Status             string   `json:"status"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
}

// oidcClientService defines the data-access contract for OIDC client operations.
type oidcClientService interface {
	ListOIDCClients(ctx context.Context, entityID string, limit, offset int32) ([]OIDCClientResponse, error)
	CountOIDCClients(ctx context.Context, entityID string) (int64, error)
	GetOIDCClientByID(ctx context.Context, entityID, id string) (OIDCClientResponse, error)
	CreateOIDCClient(ctx context.Context, params generated.CreateOIDCClientParams) (OIDCClientResponse, string, error)
	UpdateOIDCClient(ctx context.Context, params generated.UpdateOIDCClientParams) (OIDCClientResponse, error)
	DeleteOIDCClient(ctx context.Context, entityID, id string) error
	RotateOIDCClientSecret(ctx context.Context, entityID, id string) (OIDCClientResponse, string, error)
}

// OIDCClientHandler handles OIDC client CRUD endpoints.
type OIDCClientHandler struct {
	service oidcClientService
}

func NewOIDCClientHandler(service oidcClientService) OIDCClientHandler {
	return OIDCClientHandler{service: service}
}

func (h OIDCClientHandler) RegisterRoutes(r chi.Router) {
	r.Get("/sapi/oidc-clients", h.listOIDCClients)
	r.Get("/sapi/oidc-clients/{id}", h.getOIDCClient)
	r.Post("/sapi/oidc-clients", h.createOIDCClient)
	r.Put("/sapi/oidc-clients/{id}", h.updateOIDCClient)
	r.Delete("/sapi/oidc-clients/{id}", h.deleteOIDCClient)
	r.Post("/sapi/oidc-clients/{id}/rotate-secret", h.rotateSecret)
}

func (h OIDCClientHandler) listOIDCClients(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	entityID, err := ulidValue(session.EntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_entity_id", err.Error())
		return
	}
	limit, offset := parsePagination(r)
	clients, err := h.service.ListOIDCClients(r.Context(), entityID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oidc_client_list_failed", err.Error())
		return
	}
	total, err := h.service.CountOIDCClients(r.Context(), entityID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oidc_client_count_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":   clients,
		"clients": clients,
		"total":   total,
		"limit":   int(limit),
		"offset":  int(offset),
	})
}

func (h OIDCClientHandler) getOIDCClient(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	entityID, err := ulidValue(session.EntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_entity_id", err.Error())
		return
	}
	id, err := ulidValue(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_client_id", err.Error())
		return
	}
	client, err := h.service.GetOIDCClientByID(r.Context(), entityID, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "oidc_client_not_found", err.Error())
		return
	}
	setApplicationNoStoreHeaders(w)
	writeJSON(w, http.StatusOK, client)
}

func (h OIDCClientHandler) createOIDCClient(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	entityID, err := ulidValue(session.EntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_entity_id", err.Error())
		return
	}
	var body struct {
		ApplicationID      string   `json:"application_id"`
		ClientID           string   `json:"client_id"`
		RedirectURIs       []string `json:"redirect_uris"`
		AllowedScopes      []string `json:"allowed_scopes"`
		GrantTypes         []string `json:"grant_types"`
		ResponseTypes      []string `json:"response_types"`
		PKCERequired       *bool    `json:"pkce_required"`
		WorkplaceProvider  string   `json:"workplace_provider"`
		WorkplaceAppID     string   `json:"workplace_app_id"`
		WorkplaceAppSecret string   `json:"workplace_app_secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	if body.ApplicationID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "application_id is required")
		return
	}
	appID, err := ulidValue(body.ApplicationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_application_id", err.Error())
		return
	}
	if body.ClientID == "" {
		clientID, err := generateOIDCClientID()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "client_id_generate_failed", err.Error())
			return
		}
		body.ClientID = clientID
	}
	if len(body.AllowedScopes) == 0 && !isClientCredentialsOnly(body.GrantTypes) {
		// Preserve the legacy standalone OIDC-client endpoint's interactive
		// defaults. Client-credentials clients must declare a narrow scope.
		body.AllowedScopes = []string{"openid", "profile", "email", "directory:read"}
	}
	input := ApplicationOIDCClientInput{
		ClientID:           body.ClientID,
		RedirectURIs:       body.RedirectURIs,
		AllowedScopes:      body.AllowedScopes,
		GrantTypes:         body.GrantTypes,
		ResponseTypes:      body.ResponseTypes,
		PKCERequired:       body.PKCERequired,
		WorkplaceProvider:  &body.WorkplaceProvider,
		WorkplaceAppID:     &body.WorkplaceAppID,
		WorkplaceAppSecret: &body.WorkplaceAppSecret,
	}
	if err := normalizeOIDCClientCreate(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_client", err.Error())
		return
	}
	body.RedirectURIs = input.RedirectURIs
	body.AllowedScopes = input.AllowedScopes
	body.GrantTypes = input.GrantTypes
	body.ResponseTypes = input.ResponseTypes
	body.PKCERequired = input.PKCERequired
	workplaceProvider, workplaceAppID, workplaceAppSecret, err := normalizeWorkplaceConfig(body.WorkplaceProvider, body.WorkplaceAppID, body.WorkplaceAppSecret)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_workplace_config", err.Error())
		return
	}

	client, secret, err := h.service.CreateOIDCClient(r.Context(), generated.CreateOIDCClientParams{
		EntityID:           entityID,
		ApplicationID:      appID,
		ClientID:           body.ClientID,
		RedirectUris:       body.RedirectURIs,
		AllowedScopes:      body.AllowedScopes,
		GrantTypes:         body.GrantTypes,
		ResponseTypes:      body.ResponseTypes,
		PkceRequired:       *body.PKCERequired,
		WorkplaceProvider:  workplaceProvider,
		WorkplaceAppID:     workplaceAppID,
		WorkplaceAppSecret: workplaceAppSecret,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oidc_client_create_failed", err.Error())
		return
	}

	setApplicationNoStoreHeaders(w)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"client":        client,
		"client_secret": secret,
	})
}

func (h OIDCClientHandler) updateOIDCClient(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	entityID, err := ulidValue(session.EntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_entity_id", err.Error())
		return
	}
	id, err := ulidValue(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_client_id", err.Error())
		return
	}
	var body struct {
		RedirectURIs       []string `json:"redirect_uris"`
		AllowedScopes      []string `json:"allowed_scopes"`
		GrantTypes         []string `json:"grant_types"`
		ResponseTypes      []string `json:"response_types"`
		PKCERequired       *bool    `json:"pkce_required"`
		WorkplaceProvider  *string  `json:"workplace_provider"`
		WorkplaceAppID     *string  `json:"workplace_app_id"`
		WorkplaceAppSecret *string  `json:"workplace_app_secret"`
		Status             string   `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid json body")
		return
	}
	if body.RedirectURIs != nil {
		body.RedirectURIs, err = normalizeOIDCRedirectURIs(body.RedirectURIs, false)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_redirect_uris", err.Error())
			return
		}
	}

	var pkceRequired pgtype.Bool
	if body.PKCERequired != nil {
		pkceRequired = pgtype.Bool{Valid: true, Bool: *body.PKCERequired}
	}
	var workplaceProvider, workplaceAppID, workplaceAppSecret pgtype.Text
	if body.WorkplaceProvider != nil || body.WorkplaceAppID != nil || body.WorkplaceAppSecret != nil {
		before, err := h.service.GetOIDCClientByID(r.Context(), entityID, id)
		if err != nil {
			writeError(w, http.StatusNotFound, "oidc_client_not_found", err.Error())
			return
		}
		workplaceProvider, workplaceAppID, workplaceAppSecret, err = normalizeApplicationWorkplaceUpdate(before, ApplicationOIDCClientInput{
			WorkplaceProvider:  body.WorkplaceProvider,
			WorkplaceAppID:     body.WorkplaceAppID,
			WorkplaceAppSecret: body.WorkplaceAppSecret,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_workplace_config", err.Error())
			return
		}
	}

	client, err := h.service.UpdateOIDCClient(r.Context(), generated.UpdateOIDCClientParams{
		EntityID:           entityID,
		ID:                 id,
		RedirectUris:       body.RedirectURIs,
		AllowedScopes:      body.AllowedScopes,
		GrantTypes:         body.GrantTypes,
		ResponseTypes:      body.ResponseTypes,
		PkceRequired:       pkceRequired,
		WorkplaceProvider:  workplaceProvider,
		WorkplaceAppID:     workplaceAppID,
		WorkplaceAppSecret: workplaceAppSecret,
		Status:             optionalText(body.Status),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oidc_client_update_failed", err.Error())
		return
	}
	setApplicationNoStoreHeaders(w)
	writeJSON(w, http.StatusOK, client)
}

func normalizeWorkplaceConfig(provider, appID, appSecret string) (string, string, string, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	appID = strings.TrimSpace(appID)
	appSecret = strings.TrimSpace(appSecret)
	if provider == "" {
		if appID != "" || appSecret != "" {
			return "", "", "", errInvalidWorkplaceConfig()
		}
		return "", "", "", nil
	}
	if provider != "feishu" {
		return "", "", "", errInvalidWorkplaceConfig()
	}
	if appID == "" || appSecret == "" {
		return "", "", "", errInvalidWorkplaceConfig()
	}
	return provider, appID, appSecret, nil
}

func errInvalidWorkplaceConfig() error {
	return &workplaceConfigError{}
}

type workplaceConfigError struct{}

func (workplaceConfigError) Error() string {
	return "workplace provider, app_id, and app_secret must be configured together"
}

func (h OIDCClientHandler) deleteOIDCClient(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	entityID, err := ulidValue(session.EntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_entity_id", err.Error())
		return
	}
	id, err := ulidValue(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_client_id", err.Error())
		return
	}
	if err := h.service.DeleteOIDCClient(r.Context(), entityID, id); err != nil {
		writeError(w, http.StatusInternalServerError, "oidc_client_delete_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h OIDCClientHandler) rotateSecret(w http.ResponseWriter, r *http.Request) {
	session, ok := readSession(w, r)
	if !ok {
		return
	}
	entityID, err := ulidValue(session.EntityID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_entity_id", err.Error())
		return
	}
	id, err := ulidValue(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_oidc_client_id", err.Error())
		return
	}
	client, secret, err := h.service.RotateOIDCClientSecret(r.Context(), entityID, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "oidc_client_rotate_secret_failed", err.Error())
		return
	}
	setApplicationNoStoreHeaders(w)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"client":        client,
		"client_secret": secret,
	})
}

// oidcClientFromRow converts a generated OIDC client row to a response.
func oidcClientFromRow(row generated.OidcClient) OIDCClientResponse {
	return OIDCClientResponse{
		ID:                 ulidString(row.ID),
		EntityID:           ulidString(row.EntityID),
		ApplicationID:      ulidString(row.ApplicationID),
		ClientID:           row.ClientID,
		RedirectURIs:       row.RedirectUris,
		AllowedScopes:      row.AllowedScopes,
		GrantTypes:         row.GrantTypes,
		ResponseTypes:      row.ResponseTypes,
		PKCERequired:       row.PkceRequired,
		SecretRequired:     row.SecretRequired,
		WorkplaceProvider:  row.WorkplaceProvider,
		WorkplaceAppID:     row.WorkplaceAppID,
		WorkplaceAppSecret: row.WorkplaceAppSecret,
		Status:             row.Status,
		CreatedAt:          row.CreatedAt.Time.Format(time.RFC3339),
		UpdatedAt:          row.UpdatedAt.Time.Format(time.RFC3339),
	}
}

func oidcClientFromListRow(row generated.ListOIDCClientsRow) OIDCClientResponse {
	return OIDCClientResponse{
		ID:                ulidString(row.ID),
		EntityID:          ulidString(row.EntityID),
		ApplicationID:     ulidString(row.ApplicationID),
		ClientID:          row.ClientID,
		RedirectURIs:      row.RedirectUris,
		AllowedScopes:     row.AllowedScopes,
		GrantTypes:        row.GrantTypes,
		ResponseTypes:     row.ResponseTypes,
		PKCERequired:      row.PkceRequired,
		SecretRequired:    row.SecretRequired,
		WorkplaceProvider: row.WorkplaceProvider,
		WorkplaceAppID:    row.WorkplaceAppID,
		Status:            row.Status,
		CreatedAt:         row.CreatedAt.Time.Format(time.RFC3339),
		UpdatedAt:         row.UpdatedAt.Time.Format(time.RFC3339),
	}
}
