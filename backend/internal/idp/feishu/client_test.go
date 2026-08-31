// SPDX-License-Identifier: MIT

package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smices/open-idb/internal/idp"
)

func TestFullSyncFetchesTokenDepartmentsAndUsers(t *testing.T) {
	var tokenRequest map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			if err := json.NewDecoder(r.Body).Decode(&tokenRequest); err != nil {
				t.Fatalf("decode token request: %v", err)
			}
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/departments/0/children":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("department auth = %q", got)
			}
			if got := r.URL.Query().Get("page_size"); got != "50" {
				t.Fatalf("department page_size = %q, want 50", got)
			}
			if got := r.URL.Query().Get("fetch_child"); got != "true" {
				t.Fatalf("department fetch_child = %q, want true", got)
			}
			if got := r.URL.Query().Get("department_id_type"); got != "department_id" {
				t.Fatalf("department_id_type = %q, want department_id", got)
			}
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"items": []map[string]interface{}{
						{
							"department_id":        "od-1",
							"parent_department_id": "0",
							"name":                 "研发中心",
						},
					},
				},
			})
		case "/open-apis/contact/v3/users/find_by_department":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("user auth = %q", got)
			}
			if got := r.URL.Query().Get("page_size"); got != "50" {
				t.Fatalf("user page_size = %q, want 50", got)
			}
			if got := r.URL.Query().Get("department_id"); got != "0" && got != "od-1" {
				t.Fatalf("user department_id = %q, want root or synced department", got)
			}
			user := map[string]interface{}{
				"user_id":    "ou_1",
				"union_id":   "on_1",
				"open_id":    "open_1",
				"name":       "张三",
				"email":      "zhangsan@example.test",
				"mobile":     "13800000000",
				"avatar_url": "https://example.test/a.png",
				"department_ids": []string{
					"0",
				},
				"status": map[string]interface{}{
					"is_activated": true,
					"is_frozen":    false,
					"is_resigned":  false,
				},
			}
			if r.URL.Query().Get("department_id") == "od-1" {
				user["en_name"] = "Zhang San"
			}
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"items": []map[string]interface{}{user},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	data, err := client.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync() error = %v", err)
	}

	if tokenRequest["app_id"] != "app-id" || tokenRequest["app_secret"] != "secret" {
		t.Fatalf("token request = %#v", tokenRequest)
	}
	if len(data.Departments) != 1 || data.Departments[0].Name != "研发中心" {
		t.Fatalf("departments = %#v", data.Departments)
	}
	if !strings.Contains(string(data.Departments[0].RawProfile), "研发中心") {
		t.Fatalf("department raw profile = %s", string(data.Departments[0].RawProfile))
	}
	if len(data.Users) != 1 || data.Users[0].Name != "张三" || data.Users[0].Status != "active" {
		t.Fatalf("users = %#v", data.Users)
	}
	if data.Users[0].EnglishName != "Zhang San" {
		t.Fatalf("EnglishName = %q, want Zhang San", data.Users[0].EnglishName)
	}
	if !strings.Contains(string(data.Users[0].RawProfile), "张三") {
		t.Fatalf("user raw profile = %s", string(data.Users[0].RawProfile))
	}
	var rawUser map[string]interface{}
	if err := json.Unmarshal(data.Users[0].RawProfile, &rawUser); err != nil {
		t.Fatalf("unmarshal user raw profile: %v", err)
	}
	departmentIDs, ok := rawUser["department_ids"].([]interface{})
	if !ok || len(departmentIDs) != 1 || departmentIDs[0] != "0" {
		t.Fatalf("user department_ids = %#v, want provider value [0]", rawUser["department_ids"])
	}
	if _, ok := rawUser["department_id"]; ok {
		t.Fatalf("user raw profile should not include synthetic department_id: %#v", rawUser["department_id"])
	}
}

func TestMergeDirectoryUserPreservesExplicitEnglishNameAgainstPinyinFallback(t *testing.T) {
	existing := idp.DirectoryUser{
		Name:        "张三",
		EnglishName: "Zhang San",
		RawProfile:  []byte(`{"user_id":"ou_1","name":"张三","en_name":"Zhang San"}`),
	}
	next := idp.DirectoryUser{
		Name:        "张三",
		EnglishName: "ZhangSan",
		RawProfile:  []byte(`{"user_id":"ou_1","name":"张三"}`),
	}

	if got := mergeDirectoryUser(existing, next).EnglishName; got != "Zhang San" {
		t.Fatalf("EnglishName = %q, want Zhang San", got)
	}
}

func TestFullSyncNormalizesOpenDepartmentParentIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/departments/0/children":
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"items": []map[string]interface{}{
						{
							"department_id":        "dep_parent",
							"open_department_id":   "od_parent",
							"parent_department_id": "0",
							"name":                 "总部",
						},
						{
							"department_id":        "dep_child",
							"open_department_id":   "od_child",
							"parent_department_id": "od_parent",
							"name":                 "平台部",
						},
					},
				},
			})
		case "/open-apis/contact/v3/users/find_by_department":
			writeJSON(t, w, map[string]interface{}{"code": 0, "data": map[string]interface{}{"items": []map[string]interface{}{}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	data, err := client.FullSync(context.Background())
	if err != nil {
		t.Fatalf("FullSync() error = %v", err)
	}
	if len(data.Departments) != 2 {
		t.Fatalf("len(departments) = %d, want 2", len(data.Departments))
	}
	if data.Departments[1].ExternalDepartmentID != "dep_child" {
		t.Fatalf("child external ID = %q, want dep_child", data.Departments[1].ExternalDepartmentID)
	}
	if data.Departments[1].ParentExternalDepartmentID != "dep_parent" {
		t.Fatalf("child parent external ID = %q, want dep_parent", data.Departments[1].ParentExternalDepartmentID)
	}
}

func TestHTTPErrorIncludesResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":40011,"msg":"page size is invalid"}`, http.StatusBadRequest)
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	_, err = client.FullSync(context.Background())
	if err == nil {
		t.Fatal("FullSync() error = nil, want http error")
	}
	if !strings.Contains(err.Error(), "40011") || !strings.Contains(err.Error(), "page size is invalid") {
		t.Fatalf("error = %q, want response body included", err.Error())
	}
}

func TestFullSyncReturnsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]interface{}{"code": 999, "msg": "bad app"})
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, err := client.FullSync(context.Background()); err == nil {
		t.Fatal("FullSync() error = nil, want provider error")
	}
}

func TestIncrementalSyncFallsBackToFullSync(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/users/ou_1":
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"user": map[string]interface{}{
						"user_id":    "ou_1",
						"union_id":   "on_1",
						"open_id":    "open_1",
						"name":       "李四",
						"email":      "lisi@example.test",
						"mobile":     "13900000000",
						"avatar_url": "https://example.test/b.png",
						"status": map[string]interface{}{
							"is_activated": true,
							"is_frozen":    false,
							"is_resigned":  false,
						},
					},
				},
			})
		case "/open-apis/contact/v3/departments/od_1":
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"department": map[string]interface{}{
						"department_id":        "od_1",
						"open_department_id":   "open_od_1",
						"parent_department_id": "0",
						"name":                 "平台部",
					},
				},
			})
		default:
			if r.URL.Path == "/open-apis/contact/v3/departments/od_missing" {
				writeJSON(t, w, map[string]interface{}{"code": 60005, "msg": "department not found"})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/open-apis/contact/v3/departments") {
				writeJSON(t, w, map[string]interface{}{"code": 999, "msg": "unexpected department error"})
				return
			}
			if r.URL.Path == "/open-apis/contact/v3/users/ou_missing" {
				writeJSON(t, w, map[string]interface{}{"code": 60004, "msg": "user not found"})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/open-apis/contact/v3/users") {
				writeJSON(t, w, map[string]interface{}{"code": 999, "msg": "unexpected user error"})
				return
			}
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	data, err := client.IncrementalSync(context.Background(), []idp.DirectorySyncEvent{
		{
			EventType:    "added_user",
			ObjectType:   "user",
			ObjectID:     "ou_1",
			ObjectIDType: "user_id",
		},
		{
			EventType:    "delete_user",
			ObjectType:   "user",
			ObjectID:     "ou_missing",
			ObjectIDType: "open_id",
		},
		{
			EventType:    "added_department",
			ObjectType:   "department",
			ObjectID:     "od_1",
			ObjectIDType: "department_id",
		},
		{
			EventType:    "delete_department",
			ObjectType:   "department",
			ObjectID:     "od_missing",
			ObjectIDType: "open_department_id",
		},
	})
	if err != nil {
		t.Fatalf("IncrementalSync() error = %v", err)
	}
	if len(data.Users) != 1 || data.Users[0].ExternalUserID != "ou_1" {
		t.Fatalf("unexpected users = %#v", data.Users)
	}
	if len(data.UserDeletions) != 1 ||
		data.UserDeletions[0].ObjectID != "ou_missing" ||
		data.UserDeletions[0].ObjectIDType != "open_id" {
		t.Fatalf("unexpected user deletions = %#v", data.UserDeletions)
	}
	if len(data.Departments) != 1 || data.Departments[0].ExternalDepartmentID != "od_1" {
		t.Fatalf("unexpected departments = %#v", data.Departments)
	}
	if len(data.DepartmentDeletions) != 1 ||
		data.DepartmentDeletions[0].ObjectID != "od_missing" ||
		data.DepartmentDeletions[0].ObjectIDType != "open_department_id" {
		t.Fatalf("unexpected department deletions = %#v", data.DepartmentDeletions)
	}
}

func TestIncrementalSyncKeepsTypedDeletionIdentifiers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/users/user_deleted", "/open-apis/contact/v3/users/open_deleted", "/open-apis/contact/v3/users/union_deleted":
			writeJSON(t, w, map[string]interface{}{"code": 60004, "msg": "user not found"})
		case "/open-apis/contact/v3/departments/open_department_deleted":
			writeJSON(t, w, map[string]interface{}{"code": 60005, "msg": "department not found"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	data, err := client.IncrementalSync(context.Background(), []idp.DirectorySyncEvent{
		{EventType: "delete_user", ObjectType: "user", ObjectID: "user_deleted", ObjectIDType: "user_id"},
		{EventType: "delete_user", ObjectType: "user", ObjectID: "open_deleted", ObjectIDType: "open_id"},
		{EventType: "delete_user", ObjectType: "user", ObjectID: "union_deleted", ObjectIDType: "union_id"},
		{EventType: "delete_department", ObjectType: "department", ObjectID: "open_department_deleted", ObjectIDType: "open_department_id"},
	})
	if err != nil {
		t.Fatalf("IncrementalSync() error = %v", err)
	}
	wantUserDeletions := []idp.DirectoryObjectDeletion{
		{ObjectID: "user_deleted", ObjectIDType: "user_id"},
		{ObjectID: "open_deleted", ObjectIDType: "open_id"},
		{ObjectID: "union_deleted", ObjectIDType: "union_id"},
	}
	if len(data.UserDeletions) != len(wantUserDeletions) {
		t.Fatalf("user deletions = %#v", data.UserDeletions)
	}
	for index, want := range wantUserDeletions {
		if data.UserDeletions[index] != want {
			t.Fatalf("user deletion[%d] = %#v, want %#v", index, data.UserDeletions[index], want)
		}
	}
	wantDepartmentDeletion := idp.DirectoryObjectDeletion{ObjectID: "open_department_deleted", ObjectIDType: "open_department_id"}
	if len(data.DepartmentDeletions) != 1 || data.DepartmentDeletions[0] != wantDepartmentDeletion {
		t.Fatalf("department deletions = %#v, want %#v", data.DepartmentDeletions, wantDepartmentDeletion)
	}
	if len(data.Users) != 0 || len(data.Departments) != 0 {
		t.Fatalf("delete webhooks produced tombstones: %#v", data)
	}
}

func TestIncrementalSyncDoesNotTreatInvalidUserIDAsDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/users/not-a-user-id":
			writeJSON(t, w, map[string]interface{}{"code": 41012, "msg": "user id invalid error"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.IncrementalSync(context.Background(), []idp.DirectorySyncEvent{{
		EventType:    "delete_user",
		ObjectType:   "user",
		ObjectID:     "not-a-user-id",
		ObjectIDType: "open_id",
	}})
	if err == nil {
		t.Fatal("IncrementalSync() error = nil, want invalid identifier error")
	}
	if !strings.Contains(err.Error(), "41012") {
		t.Fatalf("IncrementalSync() error = %q, want code 41012", err)
	}
}

func TestIncrementalSyncDoesNotTreatProviderFailureAsDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/users/ou_1":
			writeJSON(t, w, map[string]interface{}{"code": 40003, "msg": "internal error"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.IncrementalSync(context.Background(), []idp.DirectorySyncEvent{{
		EventType:    "delete_user",
		ObjectType:   "user",
		ObjectID:     "ou_1",
		ObjectIDType: "user_id",
	}})
	if err == nil {
		t.Fatal("IncrementalSync() error = nil, want provider error")
	}
	if !strings.Contains(err.Error(), "40003") {
		t.Fatalf("IncrementalSync() error = %q, want provider error code", err)
	}
}

func TestIncrementalSyncDoesNotTreatHTTPRouteNotFoundAsDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/users/ou_1":
			http.Error(w, `{"code":99991201,"msg":"request path not found"}`, http.StatusNotFound)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.IncrementalSync(context.Background(), []idp.DirectorySyncEvent{{
		EventType:    "delete_user",
		ObjectType:   "user",
		ObjectID:     "ou_1",
		ObjectIDType: "open_id",
	}})
	if err == nil {
		t.Fatal("IncrementalSync() error = nil, want route error")
	}
	if !strings.Contains(err.Error(), "99991201") {
		t.Fatalf("IncrementalSync() error = %q, want route error code", err)
	}
}

func TestFullSyncRejectsUserWithoutStableExternalID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case "/open-apis/contact/v3/departments/0/children":
			writeJSON(t, w, map[string]interface{}{"code": 0, "data": map[string]interface{}{"items": []map[string]interface{}{}}})
		case "/open-apis/contact/v3/users/find_by_department":
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{
					"items": []map[string]interface{}{{"name": "missing-id"}},
				},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.FullSync(context.Background())
	if err == nil {
		t.Fatal("FullSync() error = nil, want missing external id error")
	}
	if !strings.Contains(err.Error(), "user_id, open_id, and union_id") {
		t.Fatalf("FullSync() error = %q, want stable ID detail", err)
	}
}

func TestNewClientRejectsMissingCredentials(t *testing.T) {
	if _, err := NewClient(Config{AppID: "app-id"}, nil); err == nil {
		t.Fatal("NewClient() error = nil, want error")
	}
}

func TestNewClientUsesBoundedDefaultHTTPTimeout(t *testing.T) {
	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret"}, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client.httpClient.Timeout != 15*time.Second {
		t.Fatalf("default HTTP timeout = %s, want 15s", client.httpClient.Timeout)
	}
}

func TestIncrementalSyncEscapesProviderObjectIDPathSegment(t *testing.T) {
	var objectRequestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			writeJSON(t, w, map[string]interface{}{"code": 0, "tenant_access_token": "tenant-token"})
		case strings.HasPrefix(r.URL.Path, "/open-apis/contact/v3/users/"):
			objectRequestURI = r.RequestURI
			writeJSON(t, w, map[string]interface{}{
				"code": 0,
				"data": map[string]interface{}{"user": map[string]interface{}{
					"user_id": "user/with space", "name": "Escaped User",
				}},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.IncrementalSync(context.Background(), []idp.DirectorySyncEvent{{
		EventType: "updated_user", ObjectType: "user", ObjectID: "user/with space", ObjectIDType: "user_id",
	}})
	if err != nil {
		t.Fatalf("IncrementalSync() error = %v", err)
	}
	if !strings.Contains(objectRequestURI, "/users/user%2Fwith%20space?") {
		t.Fatalf("object request URI = %q, want escaped path segment", objectRequestURI)
	}
}

func TestDepartmentsRejectRepeatedPaginationToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(t, w, map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{"has_more": true, "next_page_token": "same-token", "items": []interface{}{}},
		})
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.departments(context.Background(), "tenant-token")
	if err == nil || !strings.Contains(err.Error(), "repeated page_token") {
		t.Fatalf("departments error = %v, want repeated page_token", err)
	}
	if requests != 2 {
		t.Fatalf("department page requests = %d, want 2 before stopping", requests)
	}
}

func TestUsersRejectRepeatedPaginationToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(t, w, map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{"has_more": true, "next_page_token": "same-token", "items": []interface{}{}},
		})
	}))
	defer server.Close()

	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.usersByDepartment(context.Background(), "tenant-token", "department")
	if err == nil || !strings.Contains(err.Error(), "repeated page_token") {
		t.Fatalf("users error = %v, want repeated page_token", err)
	}
	if requests != 2 {
		t.Fatalf("user page requests = %d, want 2 before stopping", requests)
	}
}

func TestDepartmentsRejectPaginationTokenCycle(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		next := map[string]string{"": "A", "A": "B", "B": "A"}[r.URL.Query().Get("page_token")]
		writeJSON(t, w, map[string]interface{}{
			"code": 0, "data": map[string]interface{}{"has_more": true, "next_page_token": next, "items": []interface{}{}},
		})
	}))
	defer server.Close()
	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.departments(context.Background(), "tenant-token")
	if err == nil || !strings.Contains(err.Error(), "repeated page_token") {
		t.Fatalf("departments error = %v, want cycle rejection", err)
	}
	if requests != 3 {
		t.Fatalf("department page requests = %d, want 3", requests)
	}
}

func TestUsersRejectPaginationTokenCycle(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		next := map[string]string{"": "A", "A": "B", "B": "A"}[r.URL.Query().Get("page_token")]
		writeJSON(t, w, map[string]interface{}{
			"code": 0, "data": map[string]interface{}{"has_more": true, "next_page_token": next, "items": []interface{}{}},
		})
	}))
	defer server.Close()
	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.usersByDepartment(context.Background(), "tenant-token", "department")
	if err == nil || !strings.Contains(err.Error(), "repeated page_token") {
		t.Fatalf("users error = %v, want cycle rejection", err)
	}
	if requests != 3 {
		t.Fatalf("user page requests = %d, want 3", requests)
	}
}

func TestDepartmentsEnforceMaximumPageBudget(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(t, w, map[string]interface{}{
			"code": 0, "data": map[string]interface{}{"has_more": true, "next_page_token": fmt.Sprintf("page-%d", requests), "items": []interface{}{}},
		})
	}))
	defer server.Close()
	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL, MaxPages: 2}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.departments(context.Background(), "tenant-token")
	if err == nil || !strings.Contains(err.Error(), "maximum page budget 2") {
		t.Fatalf("departments error = %v, want page-budget rejection", err)
	}
	if requests != 2 {
		t.Fatalf("department page requests = %d, want 2", requests)
	}
}

func TestUsersEnforceMaximumPageBudget(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(t, w, map[string]interface{}{
			"code": 0, "data": map[string]interface{}{"has_more": true, "next_page_token": fmt.Sprintf("page-%d", requests), "items": []interface{}{}},
		})
	}))
	defer server.Close()
	client, err := NewClient(Config{AppID: "app-id", AppSecret: "secret", BaseURL: server.URL, MaxPages: 2}, server.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.usersByDepartment(context.Background(), "tenant-token", "department")
	if err == nil || !strings.Contains(err.Error(), "maximum page budget 2") {
		t.Fatalf("users error = %v, want page-budget rejection", err)
	}
	if requests != 2 {
		t.Fatalf("user page requests = %d, want 2", requests)
	}
}

func TestMergeDirectoryUserPreservesAllRawProfileFieldsAcrossDepartmentListings(t *testing.T) {
	existing := idp.DirectoryUser{
		ExternalUserID: "ou_1",
		RawProfile: []byte(`{
			"user_id":"ou_1",
			"email":"first-listing@example.test",
			"department_ids":["dept-root"],
			"custom_from_root":{"badge":"A"},
			"status":{"is_activated":true}
		}`),
	}
	next := idp.DirectoryUser{
		ExternalUserID: "ou_1",
		RawProfile: []byte(`{
			"user_id":"ou_1",
			"email":"later-listing@example.test",
			"department_ids":["dept-research"],
			"en_name":"Zhang San",
			"custom_from_research":{"job_level":"P7"},
			"status":{"is_frozen":false}
		}`),
	}

	merged := mergeDirectoryUser(existing, next)
	var raw map[string]any
	if err := json.Unmarshal(merged.RawProfile, &raw); err != nil {
		t.Fatalf("decode merged raw profile: %v", err)
	}
	if raw["en_name"] != "Zhang San" {
		t.Fatalf("en_name = %#v, want Zhang San", raw["en_name"])
	}
	if raw["email"] != "first-listing@example.test" {
		t.Fatalf("email = %#v, want first provider value to remain lossless", raw["email"])
	}
	if _, ok := raw["custom_from_root"]; !ok {
		t.Fatalf("merged raw profile lost root-only field: %s", merged.RawProfile)
	}
	if _, ok := raw["custom_from_research"]; !ok {
		t.Fatalf("merged raw profile lost research-only field: %s", merged.RawProfile)
	}
	status, ok := raw["status"].(map[string]any)
	if !ok || status["is_activated"] != true || status["is_frozen"] != false {
		t.Fatalf("status = %#v, want both source fields", raw["status"])
	}
	departments, ok := raw["department_ids"].([]any)
	if !ok || len(departments) != 2 || departments[0] != "dept-root" || departments[1] != "dept-research" {
		t.Fatalf("department_ids = %#v, want stable union", raw["department_ids"])
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode json: %v", err)
	}
}
