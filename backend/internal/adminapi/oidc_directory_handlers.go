// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/smices/open-idb/internal/sso"
)

const directoryReadScope = "directory:read"

type oidcDirectoryTokenService interface {
	IntrospectToken(ctx context.Context, entityID, tokenHash string) (sso.SSOTokenLookup, error)
}

type oidcDirectoryService interface {
	organizationService
	GetDirectorySubject(ctx context.Context, entityID, subjectID string) (OrganizationTreeNode, error)
}

// OIDCDirectoryHandler exposes the synchronized organization read model to
// authorized OIDC clients. It intentionally uses bearer-token auth instead of
// admin sessions so business applications can power people pickers and search.
type OIDCDirectoryHandler struct {
	directory oidcDirectoryService
	tokens    oidcDirectoryTokenService
}

func NewOIDCDirectoryHandler(directory oidcDirectoryService, tokens oidcDirectoryTokenService) OIDCDirectoryHandler {
	return OIDCDirectoryHandler{directory: directory, tokens: tokens}
}

func (h OIDCDirectoryHandler) RegisterRoutes(r chi.Router) {
	r.Get("/api/directory/organization-tree/root", h.getOrganizationTreeRoot)
	r.Get("/api/directory/organization-tree/children", h.listOrganizationTreeChildren)
	r.Get("/api/directory/organization-tree/search", h.searchOrganizationTree)
	r.Get("/api/directory/users/{subject_id}", h.getDirectorySubject)
}

func (h OIDCDirectoryHandler) getDirectorySubject(w http.ResponseWriter, r *http.Request) {
	token, ok := h.requireDirectoryRead(w, r)
	if !ok {
		return
	}
	subjectID, err := ulidValue(strings.TrimSpace(chi.URLParam(r, "subject_id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_subject_id", "subject id must be a ULID")
		return
	}
	subject, err := h.directory.GetDirectorySubject(r.Context(), token.EntityID, subjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "directory_subject_not_found", "active directory subject was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "directory_subject_lookup_failed", "directory subject lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, subject)
}

func (h OIDCDirectoryHandler) getOrganizationTreeRoot(w http.ResponseWriter, r *http.Request) {
	subject, ok := h.requireDirectoryRead(w, r)
	if !ok {
		return
	}
	limit, offset := parsePagination(r)
	root, err := h.directory.GetOrganizationTreeRoot(r.Context(), subject.EntityID, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "organization_tree_root_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, root)
}

func (h OIDCDirectoryHandler) listOrganizationTreeChildren(w http.ResponseWriter, r *http.Request) {
	subject, ok := h.requireDirectoryRead(w, r)
	if !ok {
		return
	}
	parentID, err := ulidValue(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parent_id", err.Error())
		return
	}
	kind := OrganizationTreeNodeKind(r.URL.Query().Get("kind"))
	limit, offset := parsePagination(r)
	children, err := h.directory.ListOrganizationTreeChildren(r.Context(), subject.EntityID, kind, parentID, limit, offset)
	if err != nil {
		writeError(w, http.StatusBadRequest, "organization_tree_children_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, PagedResponse{Items: children, Total: int64(len(children)), Limit: int(limit), Offset: int(offset)})
}

func (h OIDCDirectoryHandler) searchOrganizationTree(w http.ResponseWriter, r *http.Request) {
	subject, ok := h.requireDirectoryRead(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := parsePagination(r)
	results, err := h.directory.SearchOrganizationTree(r.Context(), subject.EntityID, query, limit, offset)
	if err != nil {
		writeError(w, http.StatusBadRequest, "organization_tree_search_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (h OIDCDirectoryHandler) requireDirectoryRead(w http.ResponseWriter, r *http.Request) (sso.SSOTokenLookup, bool) {
	var empty sso.SSOTokenLookup
	if h.tokens == nil {
		writeError(w, http.StatusInternalServerError, "directory_auth_not_configured", "directory token auth is not configured")
		return empty, false
	}
	entityID, err := ulidValue(strings.TrimSpace(r.Header.Get("X-IDB-Entity-ID")))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_token", "entity id is required")
		return empty, false
	}
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="missing or malformed bearer token"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "missing or malformed bearer token")
		return empty, false
	}
	rawToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if rawToken == "" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="empty bearer token"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "empty bearer token")
		return empty, false
	}
	token, err := h.tokens.IntrospectToken(r.Context(), entityID, sso.HashToken(rawToken))
	if err != nil || token.TokenType != "access" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="token is invalid, revoked, or expired"`)
		writeError(w, http.StatusUnauthorized, "invalid_token", "token is invalid, revoked, or expired")
		return empty, false
	}
	if !hasScope(token.Scopes, directoryReadScope) {
		writeError(w, http.StatusForbidden, "insufficient_scope", "directory:read scope is required")
		return empty, false
	}
	return token, true
}

func hasScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == required {
			return true
		}
	}
	return false
}
