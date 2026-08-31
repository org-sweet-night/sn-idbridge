// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/audit"
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/smices/open-idb/internal/db/generated"
)

// AdminService wraps *generated.Queries and provides data-access methods
// for all admin resource handlers. It satisfies the userService interface.
type AdminService struct {
	queries               *generated.Queries
	audit                 *auditWriter
	organizationTreeCache *OrganizationTreeCache
	txStarter             txStarter
}

type txStarter interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

var enabledIdentitySourceTypes = map[string]bool{
	"feishu": true,
}

var primaryIdentitySourceTypes = map[string]bool{
	"feishu":   true,
	"dingtalk": true,
	"wecom":    true,
	"ldap":     true,
}

func NewAdminService(queries *generated.Queries, auditLogger ...AuditLogger) (*AdminService, error) {
	if queries == nil {
		return nil, fmt.Errorf("queries are required")
	}
	svc := &AdminService{queries: queries}
	if len(auditLogger) > 0 && auditLogger[0] != nil {
		svc.audit = &auditWriter{logger: auditLogger[0]}
	}
	return svc, nil
}

func (s *AdminService) SetTxStarter(starter txStarter) {
	s.txStarter = starter
}

// --- Platform settings ---

func (s *AdminService) GetPlatformBranding(ctx context.Context) (PlatformBrandingResponse, error) {
	row, err := s.queries.GetPlatformSettings(ctx)
	if err != nil {
		return PlatformBrandingResponse{}, err
	}
	return platformBrandingFromRow(row), nil
}

func (s *AdminService) UpdatePlatformBranding(ctx context.Context, platformName, logoURL, faviconURL, titleSuffix string) (PlatformBrandingResponse, error) {
	row, err := s.queries.UpsertPlatformSettings(ctx, generated.UpsertPlatformSettingsParams{
		PlatformName: strings.TrimSpace(platformName),
		LogoUrl:      strings.TrimSpace(logoURL),
		FaviconUrl:   strings.TrimSpace(faviconURL),
		TitleSuffix:  strings.TrimSpace(titleSuffix),
	})
	if err != nil {
		return PlatformBrandingResponse{}, err
	}
	return platformBrandingFromRow(row), nil
}

// --- Entities ---

func (s *AdminService) ListEntities(ctx context.Context, limit, offset int32) ([]EntityResponse, error) {
	rows, err := s.queries.ListEntities(ctx, generated.ListEntitiesParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	entities := make([]EntityResponse, 0, len(rows))
	for _, row := range rows {
		entities = append(entities, entityFromRow(row.ID, row.Name, row.Slug, row.Status, row.DefaultLocale, row.BrandName, row.LogoUrl, row.LoginMessage, row.CreatedAt))
	}
	return entities, nil
}

func (s *AdminService) CountEntities(ctx context.Context) (int64, error) {
	return s.queries.CountEntities(ctx)
}

func (s *AdminService) GetEntityByID(ctx context.Context, id string) (EntityResponse, error) {
	row, err := s.queries.GetEntityByID(ctx, id)
	if err != nil {
		return EntityResponse{}, err
	}
	return entityFromRow(row.ID, row.Name, row.Slug, row.Status, row.DefaultLocale, row.BrandName, row.LogoUrl, row.LoginMessage, row.CreatedAt), nil
}

func (s *AdminService) CreateEntity(ctx context.Context, name, slug, defaultLocale, brandName, logoURL, loginMessage string) (EntityResponse, error) {
	row, err := s.queries.CreateEntity(ctx, generated.CreateEntityParams{
		Name:          name,
		Slug:          slug,
		DefaultLocale: defaultLocale,
		BrandName:     brandName,
		LogoUrl:       logoURL,
		LoginMessage:  loginMessage,
	})
	if err != nil {
		return EntityResponse{}, err
	}
	return entityFromRow(row.ID, row.Name, row.Slug, row.Status, row.DefaultLocale, row.BrandName, row.LogoUrl, row.LoginMessage, row.CreatedAt), nil
}

func (s *AdminService) UpdateEntity(ctx context.Context, id string, name, status, defaultLocale, brandName, logoURL, loginMessage pgtype.Text) (EntityResponse, error) {
	row, err := s.queries.UpdateEntity(ctx, generated.UpdateEntityParams{
		ID:            id,
		Name:          name,
		Status:        status,
		DefaultLocale: defaultLocale,
		BrandName:     brandName,
		LogoUrl:       logoURL,
		LoginMessage:  loginMessage,
	})
	if err != nil {
		return EntityResponse{}, err
	}
	return entityFromRow(row.ID, row.Name, row.Slug, row.Status, row.DefaultLocale, row.BrandName, row.LogoUrl, row.LoginMessage, row.CreatedAt), nil
}

// --- Users ---

func (s *AdminService) ListUsers(ctx context.Context, entityID string, status pgtype.Text, limit, offset int32) ([]UserResponse, error) {
	rows, err := s.queries.ListUsers(ctx, generated.ListUsersParams{
		EntityID:        entityID,
		Limit:           limit,
		Offset:          offset,
		LifecycleStatus: status,
	})
	if err != nil {
		return nil, err
	}
	users := make([]UserResponse, 0, len(rows))
	for _, row := range rows {
		users = append(users, userFromRow(row))
	}
	return users, nil
}

func (s *AdminService) CountUsers(ctx context.Context, entityID string, status pgtype.Text) (int64, error) {
	return s.queries.CountUsers(ctx, generated.CountUsersParams{
		EntityID:        entityID,
		LifecycleStatus: status,
	})
}

func (s *AdminService) GetUserByID(ctx context.Context, entityID, id string) (UserResponse, error) {
	row, err := s.queries.GetUserByID(ctx, generated.GetUserByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return UserResponse{}, err
	}
	return userFromRow(row), nil
}

func (s *AdminService) ListArchivedUsers(ctx context.Context, entityID, username string, limit, offset int32) ([]ArchivedUserResponse, error) {
	rows, err := s.queries.ListArchivedUsers(ctx, generated.ListArchivedUsersParams{
		EntityID: entityID,
		Username: optionalText(username),
		Offset:   offset,
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}
	items := make([]ArchivedUserResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, archivedUserFromRow(row))
	}
	return items, nil
}

func (s *AdminService) CountArchivedUsers(ctx context.Context, entityID, username string) (int64, error) {
	return s.queries.CountArchivedUsers(ctx, generated.CountArchivedUsersParams{
		EntityID: entityID,
		Username: optionalText(username),
	})
}

func (s *AdminService) GetArchivedUser(ctx context.Context, entityID, id string) (ArchivedUserResponse, error) {
	row, err := s.queries.GetArchivedUserByID(ctx, generated.GetArchivedUserByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return ArchivedUserResponse{}, err
	}
	return archivedUserFromRow(row), nil
}

func (s *AdminService) DeleteArchivedUser(ctx context.Context, entityID, id string) (int64, error) {
	return s.queries.DeleteArchivedUser(ctx, generated.DeleteArchivedUserParams{
		EntityID: entityID,
		ID:       id,
	})
}

func (s *AdminService) ClearArchivedUsers(ctx context.Context, entityID string) (int64, error) {
	return s.queries.ClearArchivedUsers(ctx, entityID)
}

func (s *AdminService) ArchiveUser(ctx context.Context, entityID, userID, actorUserID, reason string) (ArchivedUserResponse, error) {
	before, err := s.GetUserByID(ctx, entityID, userID)
	if err != nil {
		return ArchivedUserResponse{}, err
	}
	if reason == "" {
		reason = "admin deleted user"
	}
	actor := optionalText(actorUserID)
	event := audit.Event{
		EntityID:     entityID,
		ActorUserID:  actorUserID,
		ActorType:    "user",
		Action:       audit.ActionUserArchived,
		ResourceType: "archived_user",
		Before:       before,
	}

	if s.txStarter == nil {
		return ArchivedUserResponse{}, fmt.Errorf("admin service transaction starter is not configured")
	}
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ArchivedUserResponse{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	txQueries := s.queries.WithTx(tx)
	archive, err := txQueries.ArchiveUser(ctx, generated.ArchiveUserParams{
		EntityID:         entityID,
		UserID:           userID,
		ArchivedByUserID: actor,
		ArchiveReason:    reason,
	})
	if err != nil {
		return ArchivedUserResponse{}, err
	}
	if err := txQueries.DeleteUserActiveDependents(ctx, generated.DeleteUserActiveDependentsParams{
		EntityID: entityID,
		UserID:   userID,
	}); err != nil {
		return ArchivedUserResponse{}, err
	}
	if err := txQueries.DeleteUserActiveRow(ctx, generated.DeleteUserActiveRowParams{
		EntityID: entityID,
		UserID:   userID,
	}); err != nil {
		return ArchivedUserResponse{}, err
	}
	resp := archivedUserFromRow(archive)
	event.ResourceID = resp.ID
	event.After = resp
	if auditService, ok := s.auditTxLogger(txQueries); ok {
		if err := auditService.Write(ctx, event); err != nil {
			return ArchivedUserResponse{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ArchivedUserResponse{}, err
	}
	committed = true

	if _, ok := s.auditTxLogger(txQueries); !ok {
		if err := s.audit.logAction(ctx, event); err != nil {
			return ArchivedUserResponse{}, err
		}
	}
	return resp, nil
}

func (s *AdminService) auditTxLogger(txQueries *generated.Queries) (*audit.Service, bool) {
	if s.audit == nil || s.audit.logger == nil {
		return nil, false
	}
	if _, ok := s.audit.logger.(*audit.Service); !ok {
		return nil, false
	}
	return audit.NewService(txQueries), true
}

func (s *AdminService) auditWriterForTx(txQueries *generated.Queries) *auditWriter {
	if s.audit == nil || s.audit.logger == nil {
		return nil
	}
	if _, ok := s.audit.logger.(*audit.Service); ok {
		return &auditWriter{logger: audit.NewService(txQueries)}
	}
	return s.audit
}

func (s *AdminService) UpdateUserLifecycle(ctx context.Context, entityID, id string, status string) (UserResponse, error) {
	before, _ := s.GetUserByID(ctx, entityID, id)
	row, err := s.queries.UpdateUserLifecycle(ctx, generated.UpdateUserLifecycleParams{
		EntityID:        entityID,
		ID:              id,
		LifecycleStatus: status,
	})
	if err != nil {
		return UserResponse{}, err
	}
	after := userFromRow(row)
	action := audit.ActionUserUpdated
	if status == "disabled" {
		action = audit.ActionUserDisabled
	}
	if err := s.audit.logAction(ctx, audit.Event{
		EntityID:     ulidString(entityID),
		ActorType:    "user",
		Action:       action,
		ResourceType: "user",
		ResourceID:   after.ID,
		Before:       before,
		After:        after,
	}); err != nil {
		return UserResponse{}, err
	}
	return after, nil
}

func (s *AdminService) UpdateUser(ctx context.Context, entityID, id string, displayName, email, phone, locale pgtype.Text) (UserResponse, error) {
	row, err := s.queries.UpdateUser(ctx, generated.UpdateUserParams{
		EntityID:    entityID,
		ID:          id,
		DisplayName: displayName,
		Email:       email,
		Phone:       phone,
		Locale:      locale,
	})
	if err != nil {
		return UserResponse{}, err
	}
	return userFromRow(row), nil
}

// --- Directory Users ---

func (s *AdminService) GetDirectoryUserByID(ctx context.Context, entityID, id string) (DirectoryUserResponse, error) {
	row, err := s.queries.GetDirectoryUserByID(ctx, generated.GetDirectoryUserByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return DirectoryUserResponse{}, err
	}
	return directoryUserFromRow(row), nil
}

// --- Applications ---

func (s *AdminService) ListApplications(ctx context.Context, entityID string, limit, offset int32) ([]ApplicationResponse, error) {
	rows, err := s.queries.ListApplications(ctx, generated.ListApplicationsParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	apps := make([]ApplicationResponse, 0, len(rows))
	for _, row := range rows {
		apps = append(apps, applicationFromListRow(row))
	}
	return apps, nil
}

func (s *AdminService) CountApplications(ctx context.Context, entityID string) (int64, error) {
	return s.queries.CountApplications(ctx, entityID)
}

func (s *AdminService) GetApplicationByID(ctx context.Context, entityID, id string) (ApplicationResponse, error) {
	row, err := s.queries.GetApplicationByID(ctx, generated.GetApplicationByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return ApplicationResponse{}, err
	}
	return applicationFromRow(row), nil
}

func (s *AdminService) GetApplicationDetail(ctx context.Context, entityID, id string) (ApplicationDetailResponse, error) {
	return applicationDetailFromQueries(ctx, s.queries, entityID, id)
}

func (s *AdminService) CreateApplication(ctx context.Context, entityID string, name, appType string) (ApplicationResponse, error) {
	if s.txStarter != nil {
		detail, err := s.createApplicationDetail(ctx, entityID, ApplicationWriteInput{Name: name, Type: appType}, false, nil)
		return detail.ApplicationResponse, err
	}
	row, err := s.queries.CreateApplication(ctx, generated.CreateApplicationParams{
		EntityID: entityID,
		Name:     name,
		Type:     appType,
	})
	if err != nil {
		return ApplicationResponse{}, err
	}
	resp := applicationFromRow(row)
	if err := s.audit.logCreate(ctx, ulidString(entityID), "", "application", resp.ID, applicationResponseForAudit(resp)); err != nil {
		return ApplicationResponse{}, err
	}
	return resp, nil
}

func (s *AdminService) UpdateApplication(ctx context.Context, entityID, id string, name, status pgtype.Text) (ApplicationResponse, error) {
	if s.txStarter != nil {
		detail, err := s.UpdateApplicationDetail(ctx, entityID, id, ApplicationWriteInput{
			Name:   textValue(name),
			Status: textValue(status),
		})
		return detail.ApplicationResponse, err
	}
	before, err := s.GetApplicationByID(ctx, entityID, id)
	if err != nil {
		return ApplicationResponse{}, err
	}
	row, err := s.queries.UpdateApplication(ctx, generated.UpdateApplicationParams{
		EntityID: entityID,
		ID:       id,
		Name:     name,
		Status:   status,
	})
	if err != nil {
		return ApplicationResponse{}, err
	}
	after := applicationFromRow(row)
	if err := s.audit.logUpdate(ctx, ulidString(entityID), "", "application", after.ID, applicationResponseForAudit(before), applicationResponseForAudit(after)); err != nil {
		return ApplicationResponse{}, err
	}
	return after, nil
}

func (s *AdminService) DeleteApplication(ctx context.Context, entityID, id string) error {
	if s.txStarter == nil {
		return fmt.Errorf("admin service transaction starter is not configured")
	}
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txQueries := s.queries.WithTx(tx)
	before, err := applicationDetailFromQueries(ctx, txQueries, entityID, id)
	if err != nil {
		return err
	}
	if err := txQueries.DeleteApplication(ctx, generated.DeleteApplicationParams{EntityID: entityID, ID: id}); err != nil {
		return err
	}
	if err := s.auditWriterForTx(txQueries).logDelete(ctx, ulidString(entityID), "", "application", ulidString(id), applicationDetailForAudit(before)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type apiClientApplicationConfig struct {
	ClientID      string   `json:"client_id"`
	ClientSecret  string   `json:"client_secret"`
	AllowedScopes []string `json:"allowed_scopes"`
}

type internalApplicationConfig struct {
	AppURL      string `json:"app_url"`
	CallbackURL string `json:"callback_url"`
	Description string `json:"description"`
}

func (s *AdminService) CreateApplicationDetail(ctx context.Context, entityID string, input ApplicationWriteInput) (ApplicationDetailResponse, error) {
	return s.createApplicationDetail(ctx, entityID, input, true, nil)
}

// CreateApplicationDetailWithClientSecret is an in-process bootstrap surface
// for provisioning a confidential OIDC client from a pre-projected secret. It
// is deliberately absent from the HTTP handler interface: ordinary callers
// retain generated secrets, while the application, client, role assignment,
// and redacted audit event remain one transaction in both paths.
func (s *AdminService) CreateApplicationDetailWithClientSecret(ctx context.Context, entityID string, input ApplicationWriteInput, clientSecret string) (ApplicationDetailResponse, error) {
	if input.Type != "oidc_client" || input.OIDCClient == nil {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "an oidc_client application is required"}
	}
	if clientSecret != strings.TrimSpace(clientSecret) || len(clientSecret) < 32 || len(clientSecret) > 4096 {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "client secret must be 32 to 4096 non-whitespace-surrounded characters"}
	}
	return s.createApplicationDetail(ctx, entityID, input, true, &clientSecret)
}

func (s *AdminService) createApplicationDetail(ctx context.Context, entityID string, input ApplicationWriteInput, requireComplete bool, clientSecret *string) (ApplicationDetailResponse, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.TrimSpace(input.Type)
	input.Status = strings.TrimSpace(input.Status)
	if input.Name == "" || !isValidApplicationType(input.Type) {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "name and a valid type are required"}
	}
	if input.Status == "" {
		input.Status = "active"
	}
	if !isValidApplicationStatus(input.Status) {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "status must be active or disabled"}
	}
	if requireComplete {
		if err := normalizeCompleteApplicationCreate(&input); err != nil {
			return ApplicationDetailResponse{}, err
		}
	}
	config := json.RawMessage(`{}`)
	var err error
	if applicationConfigProvided(input.Config) {
		config, err = normalizeApplicationConfig(input.Type, input.Config)
		if err != nil {
			return ApplicationDetailResponse{}, err
		}
	}
	if input.Type != "oidc_client" && input.OIDCClient != nil {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "oidc_client settings are only valid for oidc_client applications"}
	}
	if s.txStarter == nil {
		return ApplicationDetailResponse{}, fmt.Errorf("admin service transaction starter is not configured")
	}

	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txQueries := s.queries.WithTx(tx)
	row, err := txQueries.CreateApplication(ctx, generated.CreateApplicationParams{
		EntityID: entityID,
		Name:     input.Name,
		Type:     input.Type,
		Status:   optionalText(input.Status),
		Config:   config,
	})
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	detail := ApplicationDetailResponse{ApplicationResponse: applicationFromRow(row)}
	if input.OIDCClient != nil {
		client, err := createOIDCClientForApplication(ctx, txQueries, entityID, row.ID, row.Status, *input.OIDCClient, clientSecret)
		if err != nil {
			return ApplicationDetailResponse{}, err
		}
		detail.OIDCClient = client
	}
	if err := s.auditWriterForTx(txQueries).logCreate(ctx, ulidString(entityID), "", "application", detail.ID, applicationDetailForAudit(detail)); err != nil {
		return ApplicationDetailResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplicationDetailResponse{}, err
	}
	return detail, nil
}

func normalizeCompleteApplicationCreate(input *ApplicationWriteInput) error {
	switch input.Type {
	case "oidc_client":
		if input.OIDCClient == nil {
			return &applicationRequestError{message: "oidc_client settings are required"}
		}
		return normalizeOIDCClientCreate(input.OIDCClient)
	case "api_client", "internal_app":
		if !applicationConfigProvided(input.Config) {
			return &applicationRequestError{message: input.Type + " config is required"}
		}
	}
	return nil
}

// normalizeOIDCClientCreate distinguishes non-interactive confidential clients
// from browser-facing authorization-code clients. A client_credentials-only
// client has no redirect or response flow and therefore must not carry an
// unused callback or PKCE setting. Interactive clients retain the existing
// redirect and PKCE defaults.
func normalizeOIDCClientCreate(input *ApplicationOIDCClientInput) error {
	if len(input.GrantTypes) == 0 {
		input.GrantTypes = []string{"authorization_code"}
	}
	if isClientCredentialsOnly(input.GrantTypes) {
		if len(input.RedirectURIs) > 0 {
			return &applicationRequestError{message: "client_credentials clients must not declare redirect_uris"}
		}
		if len(input.ResponseTypes) > 0 {
			return &applicationRequestError{message: "client_credentials clients must not declare response_types"}
		}
		if input.PKCERequired != nil && *input.PKCERequired {
			return &applicationRequestError{message: "client_credentials clients must disable pkce_required"}
		}
		if len(input.AllowedScopes) == 0 {
			return &applicationRequestError{message: "client_credentials clients require explicit allowed_scopes"}
		}
		input.RedirectURIs = []string{}
		input.ResponseTypes = []string{}
		pkceRequired := false
		input.PKCERequired = &pkceRequired
		return nil
	}

	redirectURIs, err := normalizeOIDCRedirectURIs(input.RedirectURIs, true)
	if err != nil {
		return err
	}
	input.RedirectURIs = redirectURIs
	if len(input.AllowedScopes) == 0 {
		input.AllowedScopes = []string{"openid", "profile", "email"}
	}
	if len(input.ResponseTypes) == 0 {
		input.ResponseTypes = []string{"code"}
	}
	if input.PKCERequired == nil {
		pkceRequired := true
		input.PKCERequired = &pkceRequired
	}
	return nil
}

func isClientCredentialsOnly(grantTypes []string) bool {
	return len(grantTypes) == 1 && grantTypes[0] == "client_credentials"
}

func normalizeOIDCRedirectURIs(values []string, required bool) ([]string, error) {
	if values == nil {
		if !required {
			return nil, nil
		}
		return nil, &applicationRequestError{message: "oidc_client requires at least one redirect_uri"}
	}
	if len(values) == 0 {
		return nil, &applicationRequestError{message: "oidc_client requires at least one redirect_uri"}
	}
	normalized := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		redirectURI, err := url.ParseRequestURI(value)
		if err != nil || redirectURI.Host == "" || (redirectURI.Scheme != "https" && redirectURI.Scheme != "http") {
			return nil, &applicationRequestError{message: "redirect_uris must contain absolute http or https URLs"}
		}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

func (s *AdminService) UpdateApplicationDetail(ctx context.Context, entityID, id string, input ApplicationWriteInput) (ApplicationDetailResponse, error) {
	if s.txStarter == nil {
		return ApplicationDetailResponse{}, fmt.Errorf("admin service transaction starter is not configured")
	}
	tx, err := s.txStarter.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txQueries := s.queries.WithTx(tx)
	before, err := applicationDetailFromQueries(ctx, txQueries, entityID, id)
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	if input.Type != "" && input.Type != before.Type {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "application type cannot be changed"}
	}
	if input.OIDCClient != nil && before.Type != "oidc_client" {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "oidc_client settings are only valid for oidc_client applications"}
	}
	if input.Status != "" && !isValidApplicationStatus(input.Status) {
		return ApplicationDetailResponse{}, &applicationRequestError{message: "status must be active or disabled"}
	}
	var config []byte
	if applicationConfigProvided(input.Config) {
		config, err = normalizeApplicationConfig(before.Type, input.Config)
		if err != nil {
			return ApplicationDetailResponse{}, err
		}
	}
	row, err := txQueries.UpdateApplication(ctx, generated.UpdateApplicationParams{
		EntityID: entityID,
		ID:       id,
		Name:     optionalText(strings.TrimSpace(input.Name)),
		Status:   optionalText(strings.TrimSpace(input.Status)),
		Config:   config,
	})
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	after := ApplicationDetailResponse{ApplicationResponse: applicationFromRow(row), OIDCClient: before.OIDCClient}
	if input.OIDCClient != nil {
		if before.OIDCClient == nil {
			client, err := createOIDCClientForApplication(ctx, txQueries, entityID, id, row.Status, *input.OIDCClient, nil)
			if err != nil {
				return ApplicationDetailResponse{}, err
			}
			after.OIDCClient = client
		} else {
			client, err := updateOIDCClientForApplication(ctx, txQueries, entityID, row.Status, *before.OIDCClient, *input.OIDCClient)
			if err != nil {
				return ApplicationDetailResponse{}, err
			}
			after.OIDCClient = &client
		}
	}
	if err := s.auditWriterForTx(txQueries).logUpdate(ctx, ulidString(entityID), "", "application", after.ID, applicationDetailForAudit(before), applicationDetailForAudit(after)); err != nil {
		return ApplicationDetailResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ApplicationDetailResponse{}, err
	}
	return after, nil
}

func applicationDetailFromQueries(ctx context.Context, queries *generated.Queries, entityID, id string) (ApplicationDetailResponse, error) {
	row, err := queries.GetApplicationByID(ctx, generated.GetApplicationByIDParams{EntityID: entityID, ID: id})
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	detail := ApplicationDetailResponse{ApplicationResponse: applicationFromRow(row)}
	if row.Type != "oidc_client" {
		return detail, nil
	}
	clientRow, err := queries.GetOIDCClientByApplicationID(ctx, generated.GetOIDCClientByApplicationIDParams{
		EntityID: entityID, ApplicationID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return detail, nil
	}
	if err != nil {
		return ApplicationDetailResponse{}, err
	}
	client := oidcClientFromRow(clientRow)
	detail.OIDCClient = &client
	return detail, nil
}

func newOIDCClientParams(entityID, applicationID, status string, input ApplicationOIDCClientInput, suppliedSecret *string) (generated.CreateOIDCClientParams, string, error) {
	if err := normalizeOIDCClientCreate(&input); err != nil {
		return generated.CreateOIDCClientParams{}, "", err
	}
	clientID := strings.TrimSpace(input.ClientID)
	if clientID == "" {
		var err error
		clientID, err = generateOIDCClientID()
		if err != nil {
			return generated.CreateOIDCClientParams{}, "", err
		}
	}
	secret := ""
	if suppliedSecret != nil {
		secret = *suppliedSecret
	} else {
		var err error
		secret, err = generateRandomSecret(32)
		if err != nil {
			return generated.CreateOIDCClientParams{}, "", err
		}
	}
	pkceRequired := *input.PKCERequired
	provider, appID, appSecret, err := normalizeWorkplaceConfig(
		stringPointerValue(input.WorkplaceProvider),
		stringPointerValue(input.WorkplaceAppID),
		stringPointerValue(input.WorkplaceAppSecret),
	)
	if err != nil {
		return generated.CreateOIDCClientParams{}, "", &applicationRequestError{message: err.Error()}
	}
	return generated.CreateOIDCClientParams{
		EntityID:           entityID,
		ApplicationID:      applicationID,
		ClientID:           clientID,
		ClientSecretHash:   pgtype.Text{String: clientsecret.Hash(secret), Valid: true},
		RedirectUris:       input.RedirectURIs,
		AllowedScopes:      input.AllowedScopes,
		GrantTypes:         input.GrantTypes,
		ResponseTypes:      input.ResponseTypes,
		PkceRequired:       pkceRequired,
		WorkplaceProvider:  provider,
		WorkplaceAppID:     appID,
		WorkplaceAppSecret: appSecret,
		Status:             optionalText(status),
	}, secret, nil
}

func createOIDCClientForApplication(ctx context.Context, queries *generated.Queries, entityID, applicationID, status string, input ApplicationOIDCClientInput, suppliedSecret *string) (*OIDCClientResponse, error) {
	params, secret, err := newOIDCClientParams(entityID, applicationID, status, input, suppliedSecret)
	if err != nil {
		return nil, err
	}
	row, err := queries.CreateOIDCClient(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := queries.GrantApplicationAccessToRoleCode(ctx, generated.GrantApplicationAccessToRoleCodeParams{
		EntityID: entityID, ApplicationID: applicationID, Code: "employee",
	}); err != nil {
		return nil, err
	}
	client := oidcClientFromRow(row)
	// Ordinary application creation returns a generated secret exactly once.
	// Bootstrap supplies its own projected secret and must never receive it back.
	if suppliedSecret == nil {
		client.ClientSecret = secret
	}
	return &client, nil
}

func updateOIDCClientForApplication(ctx context.Context, queries *generated.Queries, entityID, status string, before OIDCClientResponse, input ApplicationOIDCClientInput) (OIDCClientResponse, error) {
	if input.ClientID != "" && strings.TrimSpace(input.ClientID) != before.ClientID {
		return OIDCClientResponse{}, &applicationRequestError{message: "client_id cannot be changed"}
	}
	if input.RedirectURIs != nil {
		redirectURIs, err := normalizeOIDCRedirectURIs(input.RedirectURIs, false)
		if err != nil {
			return OIDCClientResponse{}, err
		}
		input.RedirectURIs = redirectURIs
	}
	provider, appID, appSecret, err := normalizeApplicationWorkplaceUpdate(before, input)
	if err != nil {
		return OIDCClientResponse{}, &applicationRequestError{message: err.Error()}
	}
	var pkce pgtype.Bool
	if input.PKCERequired != nil {
		pkce = pgtype.Bool{Bool: *input.PKCERequired, Valid: true}
	}
	row, err := queries.UpdateOIDCClient(ctx, generated.UpdateOIDCClientParams{
		EntityID:           entityID,
		ID:                 before.ID,
		RedirectUris:       input.RedirectURIs,
		AllowedScopes:      input.AllowedScopes,
		GrantTypes:         input.GrantTypes,
		ResponseTypes:      input.ResponseTypes,
		PkceRequired:       pkce,
		WorkplaceProvider:  provider,
		WorkplaceAppID:     appID,
		WorkplaceAppSecret: appSecret,
		Status:             optionalText(status),
	})
	if err != nil {
		return OIDCClientResponse{}, err
	}
	return oidcClientFromRow(row), nil
}

func normalizeApplicationWorkplaceUpdate(before OIDCClientResponse, input ApplicationOIDCClientInput) (pgtype.Text, pgtype.Text, pgtype.Text, error) {
	provider := before.WorkplaceProvider
	appID := before.WorkplaceAppID
	appSecret := before.WorkplaceAppSecret
	if input.WorkplaceProvider != nil {
		provider = *input.WorkplaceProvider
	}
	if input.WorkplaceAppID != nil {
		appID = *input.WorkplaceAppID
	}
	if input.WorkplaceAppSecret != nil {
		appSecret = *input.WorkplaceAppSecret
	}

	provider, appID, appSecret, err := normalizeWorkplaceConfig(provider, appID, appSecret)
	if err != nil {
		return pgtype.Text{}, pgtype.Text{}, pgtype.Text{}, err
	}
	return pgtype.Text{String: provider, Valid: input.WorkplaceProvider != nil},
		pgtype.Text{String: appID, Valid: input.WorkplaceAppID != nil},
		pgtype.Text{String: appSecret, Valid: input.WorkplaceAppSecret != nil}, nil
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func normalizeApplicationConfig(appType string, raw json.RawMessage) (json.RawMessage, error) {
	switch appType {
	case "oidc_client":
		return json.RawMessage(`{}`), nil
	case "api_client":
		var config apiClientApplicationConfig
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, &applicationRequestError{message: "invalid api_client config"}
		}
		config.ClientID = strings.TrimSpace(config.ClientID)
		config.ClientSecret = strings.TrimSpace(config.ClientSecret)
		if config.ClientID == "" || config.ClientSecret == "" {
			return nil, &applicationRequestError{message: "api_client client_id and client_secret are required"}
		}
		if config.AllowedScopes == nil {
			config.AllowedScopes = []string{}
		}
		encoded, err := json.Marshal(config)
		return encoded, err
	case "internal_app":
		var config internalApplicationConfig
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, &applicationRequestError{message: "invalid internal_app config"}
		}
		config.AppURL = strings.TrimSpace(config.AppURL)
		config.CallbackURL = strings.TrimSpace(config.CallbackURL)
		config.Description = strings.TrimSpace(config.Description)
		encoded, err := json.Marshal(config)
		return encoded, err
	default:
		return nil, &applicationRequestError{message: "invalid application type"}
	}
}

func applicationConfigProvided(config json.RawMessage) bool {
	value := strings.TrimSpace(string(config))
	return value != "" && value != "null"
}

func isValidApplicationStatus(status string) bool {
	return status == "active" || status == "disabled"
}

func applicationResponseForAudit(application ApplicationResponse) ApplicationResponse {
	application.Config = sanitizedApplicationConfig(application.Config)
	return application
}

func applicationDetailForAudit(application ApplicationDetailResponse) ApplicationDetailResponse {
	application.ApplicationResponse = applicationResponseForAudit(application.ApplicationResponse)
	if application.OIDCClient != nil {
		client := oidcClientForAudit(*application.OIDCClient)
		application.OIDCClient = &client
	}
	return application
}

func sanitizedApplicationConfig(config json.RawMessage) json.RawMessage {
	var value map[string]interface{}
	if err := json.Unmarshal(config, &value); err != nil {
		return json.RawMessage(`{}`)
	}
	delete(value, "client_secret")
	delete(value, "workplace_app_secret")
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}

func (s *AdminService) ListApplicationRoleAssignments(ctx context.Context, entityID, applicationID string) ([]ApplicationRoleAssignmentResponse, error) {
	rows, err := s.queries.ListApplicationRoleAssignments(ctx, generated.ListApplicationRoleAssignmentsParams{
		EntityID:      entityID,
		ApplicationID: applicationID,
	})
	if err != nil {
		return nil, err
	}
	assignments := make([]ApplicationRoleAssignmentResponse, 0, len(rows))
	for _, row := range rows {
		assignments = append(assignments, applicationRoleAssignmentFromRow(row))
	}
	return assignments, nil
}

func (s *AdminService) SetApplicationRoleAssignments(ctx context.Context, entityID, applicationID string, roleIDs []string) error {
	before, _ := s.ListApplicationRoleAssignments(ctx, entityID, applicationID)
	if err := s.queries.SetApplicationRoleAssignments(ctx, generated.SetApplicationRoleAssignmentsParams{
		EntityID:      entityID,
		ApplicationID: applicationID,
		RoleIds:       roleIDs,
	}); err != nil {
		return err
	}
	after, _ := s.ListApplicationRoleAssignments(ctx, entityID, applicationID)
	return s.audit.logUpdate(ctx, entityID, "", "application", applicationID, before, after)
}

// --- Sync Jobs ---

func (s *AdminService) ListAllSyncJobs(ctx context.Context, entityID string, limit, offset int32) ([]SyncJobResponse, error) {
	rows, err := s.queries.ListAllSyncJobs(ctx, generated.ListAllSyncJobsParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	jobs := make([]SyncJobResponse, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, syncJobFromRow(row))
	}
	return jobs, nil
}

func (s *AdminService) CountAllSyncJobs(ctx context.Context, entityID string) (int64, error) {
	return s.queries.CountAllSyncJobs(ctx, entityID)
}

// --- Roles ---

func (s *AdminService) ListRoles(ctx context.Context, entityID string, limit, offset int32) ([]RoleResponse, error) {
	rows, err := s.queries.ListRoles(ctx, generated.ListRolesParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	roles := make([]RoleResponse, 0, len(rows))
	for _, row := range rows {
		roles = append(roles, roleFromRow(row))
	}
	return roles, nil
}

func (s *AdminService) CountRoles(ctx context.Context, entityID string) (int64, error) {
	return s.queries.CountRoles(ctx, entityID)
}

func (s *AdminService) GetRoleByID(ctx context.Context, entityID, id string) (RoleResponse, error) {
	row, err := s.queries.GetRoleByID(ctx, generated.GetRoleByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return RoleResponse{}, err
	}
	return roleFromRow(row), nil
}

// --- Permissions ---

func (s *AdminService) ListPermissions(ctx context.Context, entityID string, limit, offset int32) ([]PermissionResponse, error) {
	rows, err := s.queries.ListPermissions(ctx, generated.ListPermissionsParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	perms := make([]PermissionResponse, 0, len(rows))
	for _, row := range rows {
		perms = append(perms, permissionFromRow(row))
	}
	return perms, nil
}

func (s *AdminService) CountPermissions(ctx context.Context, entityID string) (int64, error) {
	return s.queries.CountPermissions(ctx, entityID)
}

func (s *AdminService) GetPermissionByID(ctx context.Context, entityID, id string) (PermissionResponse, error) {
	row, err := s.queries.GetPermissionByID(ctx, generated.GetPermissionByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return PermissionResponse{}, err
	}
	return permissionFromRow(row), nil
}

// --- Identity Sources ---

func (s *AdminService) ListIdentitySources(ctx context.Context, entityID string, limit, offset int32) ([]IdentitySourceResponse, error) {
	rows, err := s.queries.ListIdentitySources(ctx, generated.ListIdentitySourcesParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	sources := make([]IdentitySourceResponse, 0, len(rows))
	for _, row := range rows {
		sources = append(sources, identitySourceFromRow(row.ID, row.EntityID, row.Type, row.Name, row.Status, row.SyncEnabled, row.CreatedAt))
	}
	return sources, nil
}

func (s *AdminService) CountIdentitySources(ctx context.Context, entityID string) (int64, error) {
	return s.queries.CountIdentitySources(ctx, entityID)
}

func (s *AdminService) GetIdentitySourceByID(ctx context.Context, entityID, id string) (IdentitySourceResponse, error) {
	row, err := s.queries.GetIdentitySourceByID(ctx, generated.GetIdentitySourceByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return IdentitySourceResponse{}, err
	}
	return identitySourceFromRow(row.ID, row.EntityID, row.Type, row.Name, row.Status, row.SyncEnabled, row.CreatedAt), nil
}

func (s *AdminService) CreateIdentitySource(ctx context.Context, entityID string, sourceType, name string, syncEnabled bool) (IdentitySourceResponse, error) {
	sourceType = strings.TrimSpace(strings.ToLower(sourceType))
	name = strings.TrimSpace(name)
	if !enabledIdentitySourceTypes[sourceType] {
		return IdentitySourceResponse{}, fmt.Errorf("identity source type %q is not enabled yet", sourceType)
	}
	if primaryIdentitySourceTypes[sourceType] {
		existing, err := s.ListIdentitySources(ctx, entityID, 200, 0)
		if err != nil {
			return IdentitySourceResponse{}, err
		}
		for _, item := range existing {
			if item.ID != "" && primaryIdentitySourceTypes[item.Type] && item.Status == "active" {
				return IdentitySourceResponse{}, fmt.Errorf("only one active primary identity source is allowed")
			}
		}
	}
	row, err := s.queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID:    entityID,
		Type:        sourceType,
		Name:        name,
		SyncEnabled: syncEnabled,
	})
	if err != nil {
		return IdentitySourceResponse{}, err
	}
	return identitySourceFromRow(row.ID, row.EntityID, row.Type, row.Name, row.Status, row.SyncEnabled, row.CreatedAt), nil
}

func (s *AdminService) UpdateIdentitySource(ctx context.Context, entityID, id string, name, status pgtype.Text, syncEnabled pgtype.Bool) (IdentitySourceResponse, error) {
	if status.Valid && status.String == "active" {
		current, err := s.GetIdentitySourceByID(ctx, entityID, id)
		if err != nil {
			return IdentitySourceResponse{}, err
		}
		if primaryIdentitySourceTypes[current.Type] {
			existing, err := s.ListIdentitySources(ctx, entityID, 200, 0)
			if err != nil {
				return IdentitySourceResponse{}, err
			}
			for _, item := range existing {
				if item.ID != id && primaryIdentitySourceTypes[item.Type] && item.Status == "active" {
					return IdentitySourceResponse{}, fmt.Errorf("only one active primary identity source is allowed")
				}
			}
		}
	}
	row, err := s.queries.UpdateIdentitySource(ctx, generated.UpdateIdentitySourceParams{
		EntityID:    entityID,
		ID:          id,
		Name:        name,
		Status:      status,
		SyncEnabled: syncEnabled,
	})
	if err != nil {
		return IdentitySourceResponse{}, err
	}
	return identitySourceFromRow(row.ID, row.EntityID, row.Type, row.Name, row.Status, row.SyncEnabled, row.CreatedAt), nil
}

func (s *AdminService) DeleteIdentitySource(ctx context.Context, entityID, id string) error {
	return s.queries.DeleteIdentitySource(ctx, generated.DeleteIdentitySourceParams{
		EntityID: entityID,
		ID:       id,
	})
}

// --- Account Bindings ---

func (s *AdminService) ListUserBindings(ctx context.Context, entityID, userID string) ([]BindingResponse, error) {
	rows, err := s.queries.ListAccountBindingsByUserID(ctx, generated.ListAccountBindingsByUserIDParams{
		EntityID: entityID,
		UserID:   userID,
	})
	if err != nil {
		return nil, err
	}
	bindings := make([]BindingResponse, 0, len(rows))
	for _, row := range rows {
		bindings = append(bindings, bindingFromRow(
			row.ID, row.EntityID, row.UserID, row.SourceID, row.DirectoryUserID,
			row.ProviderUid, row.ProviderUnionID, row.IsPrimary, row.BoundAt,
			row.SourceType, row.SourceName,
		))
	}
	return bindings, nil
}

func (s *AdminService) GetBindingByID(ctx context.Context, entityID, id string) (BindingResponse, error) {
	row, err := s.queries.GetAccountBindingWithSource(ctx, generated.GetAccountBindingWithSourceParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return BindingResponse{}, err
	}
	return bindingFromRow(
		row.ID, row.EntityID, row.UserID, row.SourceID, row.DirectoryUserID,
		row.ProviderUid, row.ProviderUnionID, row.IsPrimary, row.BoundAt,
		row.SourceType, row.SourceName,
	), nil
}

func (s *AdminService) CreateUserBinding(ctx context.Context, entityID, userID string, sourceID, directoryUserID string, providerUID string, providerUnionID pgtype.Text, isPrimary bool) (BindingResponse, error) {
	created, err := s.queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
		EntityID:        entityID,
		UserID:          userID,
		SourceID:        sourceID,
		DirectoryUserID: directoryUserID,
		ProviderUid:     providerUID,
		ProviderUnionID: providerUnionID,
		IsPrimary:       isPrimary,
	})
	if err != nil {
		return BindingResponse{}, err
	}
	// Fetch back with source info via the JOIN query.
	resp, err := s.GetBindingByID(ctx, entityID, created.ID)
	if err != nil {
		return BindingResponse{}, err
	}
	if err := s.audit.logAction(ctx, audit.Event{
		EntityID:     ulidString(entityID),
		ActorType:    "user",
		Action:       audit.ActionUserBoundIdentity,
		ResourceType: "user",
		ResourceID:   ulidString(userID),
		After:        resp,
	}); err != nil {
		return BindingResponse{}, err
	}
	return resp, nil
}

func (s *AdminService) DeleteUserBinding(ctx context.Context, entityID, userID, id string) error {
	// Fetch the binding before deletion for audit and safety checks.
	before, err := s.GetBindingByID(ctx, entityID, id)
	if err != nil {
		return err
	}

	// Must not delete the last binding if user has no other auth method.
	count, err := s.queries.CountBindingsByUser(ctx, generated.CountBindingsByUserParams{
		EntityID: entityID,
		UserID:   userID,
	})
	if err != nil {
		return err
	}
	if count <= 1 {
		hasLocal, err := s.queries.HasLocalCredential(ctx, generated.HasLocalCredentialParams{
			EntityID: entityID,
			UserID:   userID,
		})
		if err != nil {
			return err
		}
		if !hasLocal {
			return fmt.Errorf("cannot delete last binding: user has no other authentication method")
		}
	}

	if err := s.queries.DeleteAccountBindingByID(ctx, generated.DeleteAccountBindingByIDParams{
		EntityID: entityID,
		ID:       id,
	}); err != nil {
		return err
	}
	if err := s.audit.logAction(ctx, audit.Event{
		EntityID:     ulidString(entityID),
		ActorType:    "user",
		Action:       audit.ActionUserUnboundIdentity,
		ResourceType: "user",
		ResourceID:   ulidString(userID),
		Before:       before,
	}); err != nil {
		return err
	}
	return nil
}

// --- OIDC Clients ---

func (s *AdminService) ListOIDCClients(ctx context.Context, entityID string, limit, offset int32) ([]OIDCClientResponse, error) {
	rows, err := s.queries.ListOIDCClients(ctx, generated.ListOIDCClientsParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	clients := make([]OIDCClientResponse, 0, len(rows))
	for _, row := range rows {
		clients = append(clients, oidcClientFromListRow(row))
	}
	return clients, nil
}

func (s *AdminService) CountOIDCClients(ctx context.Context, entityID string) (int64, error) {
	return s.queries.CountOIDCClients(ctx, entityID)
}

func (s *AdminService) GetOIDCClientByID(ctx context.Context, entityID, id string) (OIDCClientResponse, error) {
	row, err := s.queries.GetOIDCClientByID(ctx, generated.GetOIDCClientByIDParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return OIDCClientResponse{}, err
	}
	return oidcClientFromRow(row), nil
}

func (s *AdminService) CreateOIDCClient(ctx context.Context, params generated.CreateOIDCClientParams) (OIDCClientResponse, string, error) {
	// Generate a random client secret
	secret, err := generateRandomSecret(32)
	if err != nil {
		return OIDCClientResponse{}, "", err
	}
	params.ClientSecretHash = pgtype.Text{String: clientsecret.Hash(secret), Valid: true}

	row, err := s.queries.CreateOIDCClient(ctx, params)
	if err != nil {
		return OIDCClientResponse{}, "", err
	}
	if err := s.queries.GrantApplicationAccessToRoleCode(ctx, generated.GrantApplicationAccessToRoleCodeParams{
		EntityID:      params.EntityID,
		ApplicationID: params.ApplicationID,
		Code:          "employee",
	}); err != nil {
		return OIDCClientResponse{}, "", err
	}
	resp := oidcClientFromRow(row)
	if err := s.audit.logCreate(ctx, ulidString(params.EntityID), "", "oidc_client", resp.ID, oidcClientForAudit(resp)); err != nil {
		return OIDCClientResponse{}, "", err
	}
	return resp, secret, nil
}

func (s *AdminService) UpdateOIDCClient(ctx context.Context, params generated.UpdateOIDCClientParams) (OIDCClientResponse, error) {
	before, _ := s.GetOIDCClientByID(ctx, params.EntityID, params.ID)
	row, err := s.queries.UpdateOIDCClient(ctx, params)
	if err != nil {
		return OIDCClientResponse{}, err
	}
	after := oidcClientFromRow(row)
	if err := s.audit.logUpdate(ctx, ulidString(params.EntityID), "", "oidc_client", after.ID, oidcClientForAudit(before), oidcClientForAudit(after)); err != nil {
		return OIDCClientResponse{}, err
	}
	return after, nil
}

func (s *AdminService) DeleteOIDCClient(ctx context.Context, entityID, id string) error {
	before, _ := s.GetOIDCClientByID(ctx, entityID, id)
	err := s.queries.DeleteOIDCClient(ctx, generated.DeleteOIDCClientParams{
		EntityID: entityID,
		ID:       id,
	})
	if err != nil {
		return err
	}
	if err := s.audit.logDelete(ctx, ulidString(entityID), "", "oidc_client", ulidString(id), oidcClientForAudit(before)); err != nil {
		return err
	}
	return nil
}

func (s *AdminService) RotateOIDCClientSecret(ctx context.Context, entityID, id string) (OIDCClientResponse, string, error) {
	secret, err := generateRandomSecret(32)
	if err != nil {
		return OIDCClientResponse{}, "", err
	}
	row, err := s.queries.RotateOIDCClientSecret(ctx, generated.RotateOIDCClientSecretParams{
		EntityID:         entityID,
		ID:               id,
		ClientSecretHash: pgtype.Text{String: clientsecret.Hash(secret), Valid: true},
	})
	if err != nil {
		return OIDCClientResponse{}, "", err
	}
	resp := oidcClientFromRow(row)
	if err := s.audit.logAction(ctx, audit.Event{
		EntityID:     ulidString(entityID),
		ActorType:    "user",
		Action:       audit.ActionSecretRotated,
		ResourceType: "oidc_client",
		ResourceID:   resp.ID,
	}); err != nil {
		return OIDCClientResponse{}, "", err
	}
	return resp, secret, nil
}

func oidcClientForAudit(client OIDCClientResponse) OIDCClientResponse {
	client.ClientSecret = ""
	client.WorkplaceAppSecret = ""
	return client
}

func generateRandomSecret(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	for i := range b {
		var buf [1]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		b[i] = charset[int(buf[0])%len(charset)]
	}
	return string(b), nil
}

func generateOIDCClientID() (string, error) {
	value, err := generateRandomSecret(24)
	if err != nil {
		return "", err
	}
	return "idb_" + value, nil
}

func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
