// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/smices/open-idb/internal/sso"
)

type mockOIDCDirectoryService struct {
	rootFn     func(ctx context.Context, entityID string, limit, offset int32) (OrganizationTreeRootResponse, error)
	childrenFn func(ctx context.Context, entityID string, kind OrganizationTreeNodeKind, parentID string, limit, offset int32) ([]OrganizationTreeNode, error)
	searchFn   func(ctx context.Context, entityID, query string, limit, offset int32) (OrganizationTreeSearchResponse, error)
	subjectFn  func(ctx context.Context, entityID, subjectID string) (OrganizationTreeNode, error)
}

func (m mockOIDCDirectoryService) GetDirectorySubject(ctx context.Context, entityID, subjectID string) (OrganizationTreeNode, error) {
	if m.subjectFn != nil {
		return m.subjectFn(ctx, entityID, subjectID)
	}
	return OrganizationTreeNode{}, pgx.ErrNoRows
}

func (m mockOIDCDirectoryService) ResolveOrganizationTreeEntityID(_ context.Context, candidate string) (string, error) {
	return candidate, nil
}

func (m mockOIDCDirectoryService) GetOrganizationTreeRoot(ctx context.Context, entityID string, limit, offset int32) (OrganizationTreeRootResponse, error) {
	if m.rootFn != nil {
		return m.rootFn(ctx, entityID, limit, offset)
	}
	return OrganizationTreeRootResponse{}, nil
}

func (m mockOIDCDirectoryService) ListOrganizationTreeChildren(ctx context.Context, entityID string, kind OrganizationTreeNodeKind, parentID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	if m.childrenFn != nil {
		return m.childrenFn(ctx, entityID, kind, parentID, limit, offset)
	}
	return nil, nil
}

func (m mockOIDCDirectoryService) SearchOrganizationTree(ctx context.Context, entityID, query string, limit, offset int32) (OrganizationTreeSearchResponse, error) {
	if m.searchFn != nil {
		return m.searchFn(ctx, entityID, query, limit, offset)
	}
	return OrganizationTreeSearchResponse{}, nil
}

type mockOIDCTokenService struct {
	token sso.SSOTokenLookup
	err   error
}

func (m mockOIDCTokenService) IntrospectToken(_ context.Context, _, _ string) (sso.SSOTokenLookup, error) {
	return m.token, m.err
}

func TestOIDCDirectorySearchRequiresDirectoryReadScope(t *testing.T) {
	handler := NewOIDCDirectoryHandler(
		mockOIDCDirectoryService{},
		mockOIDCTokenService{token: sso.SSOTokenLookup{
			EntityID:  testEntityID(),
			UserID:    testUserID(),
			ClientID:  "client_1",
			TokenType: "access",
			Scopes:    []string{"openid", "profile"},
		}},
	)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/directory/organization-tree/search?q=jacky", nil)
	req.Header.Set("X-IDB-Entity-ID", testEntityID())
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestOIDCDirectorySearchUsesBearerTokenEntity(t *testing.T) {
	wantEntityID := testEntityID()
	handler := NewOIDCDirectoryHandler(
		mockOIDCDirectoryService{
			searchFn: func(_ context.Context, entityID, query string, limit, offset int32) (OrganizationTreeSearchResponse, error) {
				if entityID != wantEntityID {
					t.Fatalf("entityID = %q, want %q", entityID, wantEntityID)
				}
				if query != "jacky" {
					t.Fatalf("query = %q, want jacky", query)
				}
				return OrganizationTreeSearchResponse{
					Items: []OrganizationTreeNode{{
						ID:          testUserID(),
						Kind:        organizationTreeKindUser,
						Name:        "朱辉",
						EnglishName: "Jacky",
						Email:       "jacky@example.test",
						Status:      "active",
					}},
					Total:  1,
					Limit:  20,
					Offset: 0,
				}, nil
			},
		},
		mockOIDCTokenService{token: sso.SSOTokenLookup{
			EntityID:  wantEntityID,
			UserID:    testUserID(),
			ClientID:  "client_1",
			TokenType: "access",
			Scopes:    []string{"openid", directoryReadScope},
		}},
	)
	router := chi.NewRouter()
	handler.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/directory/organization-tree/search?q=jacky", nil)
	req.Header.Set("X-IDB-Entity-ID", wantEntityID)
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var response OrganizationTreeSearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.Items) != 1 || response.Items[0].EnglishName != "Jacky" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestOIDCDirectoryExactSubjectReturnsCurrentAuthoritativeRoles(t *testing.T) {
	entityID := testEntityID()
	subjectID := testUserID()
	handler := NewOIDCDirectoryHandler(mockOIDCDirectoryService{
		subjectFn: func(_ context.Context, gotEntityID, gotSubjectID string) (OrganizationTreeNode, error) {
			if gotEntityID != entityID || gotSubjectID != subjectID {
				t.Fatalf("lookup = (%q, %q), want (%q, %q)", gotEntityID, gotSubjectID, entityID, subjectID)
			}
			return OrganizationTreeNode{
				ID: subjectID, Kind: organizationTreeKindUser, Name: "Owner", Status: "active",
				Roles: []string{"employee", "isa:pam:requester"}, Version: "sha256:authoritative-state",
			}, nil
		},
	}, mockOIDCTokenService{token: sso.SSOTokenLookup{
		EntityID: entityID, TokenType: "access", Scopes: []string{directoryReadScope},
	}})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/api/directory/users/"+subjectID, nil)
	req.Header.Set("X-IDB-Entity-ID", entityID)
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var response OrganizationTreeNode
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if want := []string{"employee", "isa:pam:requester"}; !reflect.DeepEqual(response.Roles, want) {
		t.Fatalf("roles = %#v, want %#v", response.Roles, want)
	}
	if response.Version != "sha256:authoritative-state" {
		t.Fatalf("version = %q, want explicit authoritative version", response.Version)
	}
}

func TestOIDCDirectoryExactSubjectRejectsMalformedSubjectID(t *testing.T) {
	entityID := testEntityID()
	handler := NewOIDCDirectoryHandler(mockOIDCDirectoryService{}, mockOIDCTokenService{token: sso.SSOTokenLookup{
		EntityID: entityID, TokenType: "access", Scopes: []string{directoryReadScope},
	}})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/api/directory/users/not-a-ulid", nil)
	req.Header.Set("X-IDB-Entity-ID", entityID)
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestOIDCDirectoryExactSubjectHidesInactiveOrUnboundSubject(t *testing.T) {
	entityID := testEntityID()
	handler := NewOIDCDirectoryHandler(mockOIDCDirectoryService{
		subjectFn: func(context.Context, string, string) (OrganizationTreeNode, error) {
			return OrganizationTreeNode{}, pgx.ErrNoRows
		},
	}, mockOIDCTokenService{token: sso.SSOTokenLookup{
		EntityID: entityID, TokenType: "access", Scopes: []string{directoryReadScope},
	}})
	router := chi.NewRouter()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/api/directory/users/"+testUserID(), nil)
	req.Header.Set("X-IDB-Entity-ID", entityID)
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
