// SPDX-License-Identifier: MIT

package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mozillazg/go-pinyin"
	"github.com/smices/open-idb/internal/idp"
)

const defaultBaseURL = "https://open.feishu.cn"
const defaultPageSize = 50
const defaultMaxPages = 1000
const defaultHTTPTimeout = 15 * time.Second

type Config struct {
	AppID     string
	AppSecret string
	BaseURL   string
	MaxPages  int
}

type Client struct {
	cfg        Config
	httpClient *http.Client
	maxPages   int
}

type feishuPaginatedResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Items         []json.RawMessage `json:"items"`
		HasMore       bool              `json:"has_more"`
		PageToken     string            `json:"page_token"`
		NextPageToken string            `json:"next_page_token"`
	} `json:"data"`
}

type feishuAPIError struct {
	HTTPStatus int
	Code       int
	Message    string
	Path       string
}

func (e *feishuAPIError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("feishu http status %d for %s: code=%d msg=%s", e.HTTPStatus, e.Path, e.Code, e.Message)
	}
	return fmt.Sprintf("feishu api error for %s: code=%d msg=%s", e.Path, e.Code, e.Message)
}

func isFeishuObjectNotFound(err error, objectType string) bool {
	var apiErr *feishuAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch objectType {
	case "user":
		return apiErr.Code == 60004
	case "department":
		return apiErr.Code == 60005
	}
	return false
}

func NewClient(cfg Config, httpClient *http.Client) (*Client, error) {
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, fmt.Errorf("feishu app id and app secret are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if _, err := url.ParseRequestURI(cfg.BaseURL); err != nil {
		return nil, fmt.Errorf("feishu base url is invalid: %w", err)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	maxPages := cfg.MaxPages
	if maxPages == 0 {
		maxPages = defaultMaxPages
	}
	if maxPages < 1 {
		return nil, fmt.Errorf("feishu max pages must be positive")
	}
	return &Client{cfg: cfg, httpClient: httpClient, maxPages: maxPages}, nil
}

func (c *Client) FullSync(ctx context.Context) (idp.FullSyncData, error) {
	token, err := c.entityAccessToken(ctx)
	if err != nil {
		return idp.FullSyncData{}, err
	}
	departments, err := c.departments(ctx, token)
	if err != nil {
		return idp.FullSyncData{}, err
	}
	users, err := c.users(ctx, token, departments)
	if err != nil {
		return idp.FullSyncData{}, err
	}
	return idp.FullSyncData{Departments: departments, Users: users}, nil
}

// IncrementalSync consumes a batch of webhook events and fetches changed users/departments.
// A confirmed not-found delete is kept as a typed provider identifier so the
// sync layer can resolve an existing object without inventing a canonical ID.
func (c *Client) IncrementalSync(ctx context.Context, events []idp.DirectorySyncEvent) (idp.FullSyncData, error) {
	if len(events) == 0 {
		return idp.FullSyncData{}, nil
	}

	token, err := c.entityAccessToken(ctx)
	if err != nil {
		return idp.FullSyncData{}, err
	}

	departments := make([]idp.DirectoryDepartment, 0)
	users := make([]idp.DirectoryUser, 0)
	departmentDeletions := make([]idp.DirectoryObjectDeletion, 0)
	userDeletions := make([]idp.DirectoryObjectDeletion, 0)
	departmentIDs := make(map[string]struct{})
	userIDs := make(map[string]struct{})
	departmentDeletionIDs := make(map[string]struct{})
	userDeletionIDs := make(map[string]struct{})

	for _, event := range events {
		eventType := strings.ToLower(strings.TrimSpace(event.EventType))
		if eventType == "" || !event.IsKnown() {
			continue
		}

		objectType := strings.ToLower(strings.TrimSpace(event.ObjectType))
		if objectType == "user" {
			if event.ObjectID == "" {
				continue
			}
			user, err := c.getUserByID(ctx, token, event.ObjectID, event.ObjectIDType)
			if err != nil {
				if event.IsDeleteEvent() && isFeishuObjectNotFound(err, "user") {
					key := strings.ToLower(strings.TrimSpace(event.ObjectIDType)) + "\x00" + event.ObjectID
					if _, ok := userDeletionIDs[key]; !ok {
						userDeletions = append(userDeletions, idp.DirectoryObjectDeletion{
							ObjectID:     event.ObjectID,
							ObjectIDType: event.ObjectIDType,
						})
						userDeletionIDs[key] = struct{}{}
					}
					continue
				}
				return idp.FullSyncData{}, err
			}
			if _, ok := userIDs[user.ExternalUserID]; !ok {
				users = append(users, user)
				userIDs[user.ExternalUserID] = struct{}{}
			}
			continue
		}

		if objectType == "department" {
			if event.ObjectID == "" {
				continue
			}
			department, err := c.getDepartmentByID(ctx, token, event.ObjectID, event.ObjectIDType)
			if err != nil {
				if event.IsDeleteEvent() && isFeishuObjectNotFound(err, "department") {
					key := strings.ToLower(strings.TrimSpace(event.ObjectIDType)) + "\x00" + event.ObjectID
					if _, ok := departmentDeletionIDs[key]; !ok {
						departmentDeletions = append(departmentDeletions, idp.DirectoryObjectDeletion{
							ObjectID:     event.ObjectID,
							ObjectIDType: event.ObjectIDType,
						})
						departmentDeletionIDs[key] = struct{}{}
					}
					continue
				}
				return idp.FullSyncData{}, err
			}
			if _, ok := departmentIDs[department.ExternalDepartmentID]; !ok {
				departments = append(departments, department)
				departmentIDs[department.ExternalDepartmentID] = struct{}{}
			}
			continue
		}
	}

	return idp.FullSyncData{
		Departments:         departments,
		Users:               users,
		DepartmentDeletions: departmentDeletions,
		UserDeletions:       userDeletions,
	}, nil
}

func (c *Client) entityAccessToken(ctx context.Context) (string, error) {
	payload := map[string]string{
		"app_id":     c.cfg.AppID,
		"app_secret": c.cfg.AppSecret,
	}
	var response struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", "", payload, &response); err != nil {
		return "", err
	}
	if response.Code != 0 {
		return "", fmt.Errorf("feishu entity token failed: code=%d msg=%s", response.Code, response.Msg)
	}
	if response.TenantAccessToken == "" {
		return "", fmt.Errorf("feishu entity token response missing token")
	}
	return response.TenantAccessToken, nil
}

func (c *Client) departments(ctx context.Context, token string) ([]idp.DirectoryDepartment, error) {
	type rawDepartment struct {
		ExternalID string
		ParentID   string
		Name       string
		Raw        []byte
	}
	rawDepartments := make([]rawDepartment, 0)
	openIDToDepartmentID := make(map[string]string)
	pageToken := ""
	seenPageTokens := map[string]struct{}{"": {}}

	for page := 0; ; page++ {
		if page >= c.maxPages {
			return nil, fmt.Errorf("feishu departments exceeded maximum page budget %d", c.maxPages)
		}
		response, err := c.departmentsPage(ctx, token, pageToken)
		if err != nil {
			return nil, err
		}

		for _, raw := range response.Data.Items {
			var item struct {
				DepartmentID     string `json:"department_id"`
				OpenDepartmentID string `json:"open_department_id"`
				ParentID         string `json:"parent_department_id"`
				Name             string `json:"name"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			externalID := firstNonEmpty(item.DepartmentID, item.OpenDepartmentID)
			if item.OpenDepartmentID != "" && externalID != "" {
				openIDToDepartmentID[item.OpenDepartmentID] = externalID
			}
			rawDepartments = append(rawDepartments, rawDepartment{
				ExternalID: externalID,
				ParentID:   item.ParentID,
				Name:       item.Name,
				Raw:        cloneBytes(raw),
			})
		}

		if !response.Data.HasMore {
			break
		}
		nextPageToken := firstNonEmpty(response.Data.NextPageToken, response.Data.PageToken)
		if nextPageToken == "" {
			return nil, fmt.Errorf("feishu departments response missing page_token while has_more=true")
		}
		if _, seen := seenPageTokens[nextPageToken]; seen {
			return nil, fmt.Errorf("feishu departments response repeated page_token %q", nextPageToken)
		}
		seenPageTokens[nextPageToken] = struct{}{}
		pageToken = nextPageToken
	}

	out := make([]idp.DirectoryDepartment, 0, len(rawDepartments))
	for _, item := range rawDepartments {
		parentID := item.ParentID
		if mapped, ok := openIDToDepartmentID[parentID]; ok {
			parentID = mapped
		}
		out = append(out, idp.DirectoryDepartment{
			ExternalDepartmentID:       item.ExternalID,
			ParentExternalDepartmentID: parentID,
			Name:                       item.Name,
			RawProfile:                 cloneBytes(item.Raw),
		})
	}
	return out, nil
}

func (c *Client) departmentsPage(ctx context.Context, token string, pageToken string) (feishuPaginatedResponse, error) {
	var response feishuPaginatedResponse
	query := url.Values{}
	query.Set("fetch_child", "true")
	query.Set("department_id_type", "department_id")
	query.Set("page_size", strconv.Itoa(defaultPageSize))
	if pageToken != "" {
		query.Set("page_token", pageToken)
	}
	if err := c.doJSON(ctx, http.MethodGet, "/open-apis/contact/v3/departments/0/children?"+query.Encode(), token, nil, &response); err != nil {
		return feishuPaginatedResponse{}, err
	}
	if response.Code != 0 {
		return feishuPaginatedResponse{}, fmt.Errorf("feishu departments failed: code=%d msg=%s", response.Code, response.Msg)
	}
	return response, nil
}

func (c *Client) users(ctx context.Context, token string, departments []idp.DirectoryDepartment) ([]idp.DirectoryUser, error) {
	departmentIDs := []string{"0"}
	seenDepartments := map[string]struct{}{"0": {}}
	for _, department := range departments {
		departmentID := strings.TrimSpace(department.ExternalDepartmentID)
		if departmentID == "" {
			continue
		}
		if _, ok := seenDepartments[departmentID]; ok {
			continue
		}
		departmentIDs = append(departmentIDs, departmentID)
		seenDepartments[departmentID] = struct{}{}
	}

	out := make([]idp.DirectoryUser, 0)
	userIndexes := make(map[string]int)
	for _, departmentID := range departmentIDs {
		users, err := c.usersByDepartment(ctx, token, departmentID)
		if err != nil {
			return nil, err
		}
		for _, user := range users {
			userKey := firstNonEmpty(user.ExternalUserID, user.ExternalOpenID, user.ExternalUnionID)
			if userKey == "" {
				return nil, fmt.Errorf("feishu user is missing user_id, open_id, and union_id")
			}
			if index, ok := userIndexes[userKey]; ok {
				out[index] = mergeDirectoryUser(out[index], user)
				continue
			}
			out = append(out, user)
			userIndexes[userKey] = len(out) - 1
		}
	}

	return out, nil
}

func (c *Client) usersByDepartment(ctx context.Context, token string, departmentID string) ([]idp.DirectoryUser, error) {
	out := make([]idp.DirectoryUser, 0)
	pageToken := ""
	seenPageTokens := map[string]struct{}{"": {}}

	for page := 0; ; page++ {
		if page >= c.maxPages {
			return nil, fmt.Errorf("feishu users for department %q exceeded maximum page budget %d", departmentID, c.maxPages)
		}
		response, err := c.usersByDepartmentPage(ctx, token, departmentID, pageToken)
		if err != nil {
			return nil, err
		}

		for _, raw := range response.Data.Items {
			var item struct {
				UserID     string          `json:"user_id"`
				UnionID    string          `json:"union_id"`
				OpenID     string          `json:"open_id"`
				Name       string          `json:"name"`
				EnName     string          `json:"en_name"`
				English    string          `json:"english_name"`
				NameEN     string          `json:"name_en"`
				I18nName   json.RawMessage `json:"i18n_name"`
				Email      string          `json:"email"`
				Mobile     string          `json:"mobile"`
				AvatarURL  string          `json:"avatar_url"`
				EmployeeNo string          `json:"employee_no"`
				JobTitle   string          `json:"job_title"`
				Status     struct {
					IsActivated bool `json:"is_activated"`
					IsFrozen    bool `json:"is_frozen"`
					IsResigned  bool `json:"is_resigned"`
				} `json:"status"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			externalUserID := firstNonEmpty(item.UserID, item.OpenID, item.UnionID)
			if externalUserID == "" {
				return nil, fmt.Errorf("feishu user in department %q is missing user_id, open_id, and union_id", departmentID)
			}
			out = append(out, idp.DirectoryUser{
				ExternalUserID:  externalUserID,
				ExternalUnionID: item.UnionID,
				ExternalOpenID:  item.OpenID,
				Name:            item.Name,
				EnglishName:     englishNameForUser(item.Name, item.EnName, item.English, item.NameEN, item.I18nName),
				EmployeeNo:      item.EmployeeNo,
				JobTitle:        item.JobTitle,
				Email:           item.Email,
				Phone:           item.Mobile,
				AvatarURL:       item.AvatarURL,
				Status:          mapUserStatus(item.Status.IsActivated, item.Status.IsFrozen, item.Status.IsResigned),
				RawProfile:      cloneBytes(raw),
			})
		}

		if !response.Data.HasMore {
			break
		}
		nextPageToken := firstNonEmpty(response.Data.NextPageToken, response.Data.PageToken)
		if nextPageToken == "" {
			return nil, fmt.Errorf("feishu users response missing page_token while has_more=true")
		}
		if _, seen := seenPageTokens[nextPageToken]; seen {
			return nil, fmt.Errorf("feishu users response repeated page_token %q", nextPageToken)
		}
		seenPageTokens[nextPageToken] = struct{}{}
		pageToken = nextPageToken
	}

	return out, nil
}

func (c *Client) usersByDepartmentPage(ctx context.Context, token string, departmentID string, pageToken string) (feishuPaginatedResponse, error) {
	var response feishuPaginatedResponse
	query := url.Values{}
	query.Set("department_id", departmentID)
	query.Set("department_id_type", departmentIDType(departmentID))
	query.Set("user_id_type", "user_id")
	query.Set("page_size", strconv.Itoa(defaultPageSize))
	if pageToken != "" {
		query.Set("page_token", pageToken)
	}
	if err := c.doJSON(ctx, http.MethodGet, "/open-apis/contact/v3/users/find_by_department?"+query.Encode(), token, nil, &response); err != nil {
		return feishuPaginatedResponse{}, err
	}
	if response.Code != 0 {
		return feishuPaginatedResponse{}, fmt.Errorf("feishu users failed: code=%d msg=%s", response.Code, response.Msg)
	}
	return response, nil
}

func departmentIDType(departmentID string) string {
	if departmentID == "0" || strings.HasPrefix(departmentID, "od-") || strings.HasPrefix(departmentID, "od_") {
		return "open_department_id"
	}
	return "department_id"
}

func (c *Client) getUserByID(ctx context.Context, token string, userID string, idType string) (idp.DirectoryUser, error) {
	data, err := c.getObjectByID(ctx, token, "users", userID, "user_id_type", idType)
	if err != nil {
		return idp.DirectoryUser{}, err
	}
	var envelope struct {
		User json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return idp.DirectoryUser{}, err
	}
	rawUser := firstNonEmptyRaw(envelope.User)
	if len(rawUser) == 0 {
		rawUser = cloneBytes(data)
	}
	var item struct {
		UserID    string          `json:"user_id"`
		UnionID   string          `json:"union_id"`
		OpenID    string          `json:"open_id"`
		Name      string          `json:"name"`
		Email     string          `json:"email"`
		Mobile    string          `json:"mobile"`
		AvatarURL string          `json:"avatar_url"`
		Status    json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal(rawUser, &item); err != nil {
		return idp.DirectoryUser{}, fmt.Errorf("feishu user parse failed: %w", err)
	}
	rawPayload := rawUser
	if strings.TrimSpace(string(rawPayload)) == "" {
		rawPayload = data
	}
	externalUserID := firstNonEmpty(item.UserID, item.OpenID, item.UnionID)
	if externalUserID == "" {
		return idp.DirectoryUser{}, fmt.Errorf("feishu user is missing user_id, open_id, and union_id")
	}
	return idp.DirectoryUser{
		ExternalUserID:  externalUserID,
		ExternalUnionID: item.UnionID,
		ExternalOpenID:  item.OpenID,
		Name:            item.Name,
		Email:           item.Email,
		Phone:           item.Mobile,
		AvatarURL:       item.AvatarURL,
		Status:          mapFeishuUserStatus(item.Status),
		RawProfile:      cloneBytes(rawPayload),
	}, nil
}

func (c *Client) getDepartmentByID(ctx context.Context, token string, departmentID string, idType string) (idp.DirectoryDepartment, error) {
	data, err := c.getObjectByID(ctx, token, "departments", departmentID, "department_id_type", idType)
	if err != nil {
		return idp.DirectoryDepartment{}, err
	}
	var envelope struct {
		Department json.RawMessage `json:"department"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return idp.DirectoryDepartment{}, err
	}
	rawDepartment := firstNonEmptyRaw(envelope.Department)
	if len(rawDepartment) == 0 {
		rawDepartment = cloneBytes(data)
	}
	var item struct {
		DepartmentID     string `json:"department_id"`
		OpenDepartmentID string `json:"open_department_id"`
		ParentID         string `json:"parent_department_id"`
		Name             string `json:"name"`
	}
	if err := json.Unmarshal(rawDepartment, &item); err != nil {
		return idp.DirectoryDepartment{}, fmt.Errorf("feishu department parse failed: %w", err)
	}
	externalID := firstNonEmpty(item.DepartmentID, item.OpenDepartmentID)
	return idp.DirectoryDepartment{
		ExternalDepartmentID:       externalID,
		ParentExternalDepartmentID: item.ParentID,
		Name:                       item.Name,
		RawProfile:                 cloneBytes(rawDepartment),
	}, nil
}

func (c *Client) getObjectByID(ctx context.Context, token string, objectType string, id string, typeParam string, idType string) ([]byte, error) {
	query := url.Values{}
	if idType != "" {
		query.Set(typeParam, idType)
	}
	path := "/open-apis/contact/v3/" + objectType + "/" + url.PathEscape(id)
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var response struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, token, nil, &response); err != nil {
		return nil, err
	}
	if response.Code != 0 {
		return nil, &feishuAPIError{Code: response.Code, Message: response.Msg, Path: path}
	}
	if len(response.Data) == 0 {
		return nil, fmt.Errorf("feishu %s get missing data", objectType)
	}
	return response.Data, nil
}

func (c *Client) doJSON(ctx context.Context, method string, path string, token string, body interface{}, out interface{}) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		var apiBody struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(responseBody, &apiBody)
		message := strings.TrimSpace(apiBody.Msg)
		if message == "" {
			message = strings.TrimSpace(string(responseBody))
		}
		return &feishuAPIError{
			HTTPStatus: res.StatusCode,
			Code:       apiBody.Code,
			Message:    message,
			Path:       path,
		}
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func mapFeishuUserStatus(rawStatus json.RawMessage) string {
	if len(bytes.TrimSpace(rawStatus)) == 0 {
		return "unknown"
	}
	var boolStatus struct {
		IsActivated bool `json:"is_activated"`
		IsFrozen    bool `json:"is_frozen"`
		IsResigned  bool `json:"is_resigned"`
	}
	if err := json.Unmarshal(rawStatus, &boolStatus); err == nil {
		if boolStatus.IsResigned || boolStatus.IsFrozen {
			return "disabled"
		}
		if boolStatus.IsActivated {
			return "active"
		}
		return "unknown"
	}
	var codeStatus struct {
		Status int `json:"status"`
	}
	if err := json.Unmarshal(rawStatus, &codeStatus); err == nil {
		if codeStatus.Status == 3 {
			return "disabled"
		}
		if codeStatus.Status == 1 || codeStatus.Status == 2 {
			return "active"
		}
	}
	var statusNum int
	if err := json.Unmarshal(rawStatus, &statusNum); err == nil {
		if statusNum == 3 {
			return "disabled"
		}
		if statusNum == 1 || statusNum == 2 {
			return "active"
		}
	}
	return "unknown"
}

func mapUserStatus(activated bool, frozen bool, resigned bool) string {
	if resigned || frozen {
		return "disabled"
	}
	if activated {
		return "active"
	}
	return "unknown"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func mergeDirectoryUser(existing, next idp.DirectoryUser) idp.DirectoryUser {
	if existing.ExternalUnionID == "" {
		existing.ExternalUnionID = next.ExternalUnionID
	}
	if existing.ExternalOpenID == "" {
		existing.ExternalOpenID = next.ExternalOpenID
	}
	if existing.Name == "" {
		existing.Name = next.Name
	}
	// A prior department listing may only yield the pinyin fallback. Only
	// replace it when the later record carries an explicit provider English name.
	if english := explicitEnglishName(next.RawProfile); english != "" {
		existing.EnglishName = english
	} else if existing.EnglishName == "" {
		existing.EnglishName = next.EnglishName
	}
	if existing.EmployeeNo == "" {
		existing.EmployeeNo = next.EmployeeNo
	}
	if existing.JobTitle == "" {
		existing.JobTitle = next.JobTitle
	}
	if existing.Email == "" {
		existing.Email = next.Email
	}
	if existing.Phone == "" {
		existing.Phone = next.Phone
	}
	if existing.AvatarURL == "" {
		existing.AvatarURL = next.AvatarURL
	}
	if existing.Status == "" || existing.Status == "unknown" {
		existing.Status = next.Status
	}
	existing.RawProfile = mergeRawProfiles(existing.RawProfile, next.RawProfile)
	return existing
}

// mergeRawProfiles retains the complete provider payload when the same person
// appears in multiple department listings. The typed fields above are used by
// IdBridge, while RawProfile remains the lossless provider extension envelope.
func mergeRawProfiles(existing, next []byte) []byte {
	if len(bytes.TrimSpace(existing)) == 0 {
		return cloneBytes(next)
	}
	if len(bytes.TrimSpace(next)) == 0 {
		return cloneBytes(existing)
	}

	var existingValue any
	var nextValue any
	if json.Unmarshal(existing, &existingValue) != nil || json.Unmarshal(next, &nextValue) != nil {
		// Provider raw payloads should be JSON. If a malformed payload reaches
		// this boundary, preserve the established record rather than replacing it
		// with data that cannot be queried or rendered safely.
		return cloneBytes(existing)
	}
	merged, err := json.Marshal(mergeRawJSONValue(existingValue, nextValue))
	if err != nil {
		return cloneBytes(existing)
	}
	return merged
}

func mergeRawJSONValue(existing, next any) any {
	existingObject, existingIsObject := existing.(map[string]any)
	nextObject, nextIsObject := next.(map[string]any)
	if existingIsObject && nextIsObject {
		merged := make(map[string]any, len(existingObject)+len(nextObject))
		for key, value := range existingObject {
			merged[key] = value
		}
		for key, value := range nextObject {
			if previous, ok := merged[key]; ok {
				merged[key] = mergeRawJSONValue(previous, value)
			} else {
				merged[key] = value
			}
		}
		return merged
	}

	existingArray, existingIsArray := existing.([]any)
	nextArray, nextIsArray := next.([]any)
	if existingIsArray && nextIsArray {
		merged := append([]any{}, existingArray...)
		seen := make(map[string]struct{}, len(existingArray)+len(nextArray))
		for _, value := range existingArray {
			if encoded, err := json.Marshal(value); err == nil {
				seen[string(encoded)] = struct{}{}
			}
		}
		for _, value := range nextArray {
			encoded, err := json.Marshal(value)
			if err != nil {
				continue
			}
			if _, ok := seen[string(encoded)]; ok {
				continue
			}
			seen[string(encoded)] = struct{}{}
			merged = append(merged, value)
		}
		return merged
	}

	if isEmptyRawJSONValue(next) {
		return existing
	}
	if isEmptyRawJSONValue(existing) {
		return next
	}
	// The same provider person can be returned through several department
	// listings. Keep the first non-empty scalar verbatim so RawProfile remains
	// lossless; typed fields above decide which normalized value is preferred.
	return existing
}

func isEmptyRawJSONValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	}
	return false
}

func englishNameForUser(name string, values ...interface{}) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			parts = append(parts, typed)
		case json.RawMessage:
			parts = append(parts, englishNameFromI18n(typed))
		}
	}
	if english := normalizeLatinName(firstNonEmpty(parts...)); english != "" {
		return english
	}
	if !hasCJK(name) {
		return normalizeLatinName(name)
	}
	return pinyinDisplayName(name)
}

func explicitEnglishName(raw []byte) string {
	var profile struct {
		EnName   string          `json:"en_name"`
		English  string          `json:"english_name"`
		NameEN   string          `json:"name_en"`
		I18nName json.RawMessage `json:"i18n_name"`
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		return ""
	}
	return normalizeLatinName(firstNonEmpty(profile.EnName, profile.English, profile.NameEN, englishNameFromI18n(profile.I18nName)))
}

func englishNameFromI18n(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var names map[string]string
	if err := json.Unmarshal(raw, &names); err != nil {
		return ""
	}
	return firstNonEmpty(names["en_us"], names["en-US"], names["en"], names["en_US"])
}

func normalizeLatinName(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	for i, field := range fields {
		runes := []rune(field)
		if len(runes) == 0 {
			continue
		}
		runes[0] = unicode.ToUpper(runes[0])
		for j := 1; j < len(runes); j++ {
			runes[j] = unicode.ToLower(runes[j])
		}
		fields[i] = string(runes)
	}
	return strings.Join(fields, " ")
}

func hasCJK(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func pinyinDisplayName(value string) string {
	args := pinyin.NewArgs()
	args.Style = pinyin.Normal
	args.Fallback = func(r rune, _ pinyin.Args) []string {
		if unicode.IsSpace(r) {
			return nil
		}
		return []string{string(r)}
	}
	chunks := pinyin.Pinyin(value, args)
	parts := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if normalized := normalizeLatinName(chunk[0]); normalized != "" {
			parts = append(parts, normalized)
		}
	}
	return strings.Join(parts, "")
}

func firstNonEmptyRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(bytes.TrimSpace(value)) > 0 {
			out := make(json.RawMessage, len(value))
			copy(out, value)
			return out
		}
	}
	return nil
}

func cloneBytes(value []byte) []byte {
	out := make([]byte, len(value))
	copy(out, value)
	return out
}
