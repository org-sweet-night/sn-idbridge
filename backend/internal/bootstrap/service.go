// SPDX-License-Identifier: MIT

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/adminapi"
	"github.com/smices/open-idb/internal/audit"
	"github.com/smices/open-idb/internal/clientsecret"
	"github.com/smices/open-idb/internal/db/generated"
)

type transactionDB interface {
	generated.DBTX
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

type Service struct {
	db           transactionDB
	queries      *generated.Queries
	applications *adminapi.AdminService
}

type Result struct {
	// EntityID is a public OIDC tenant identifier. It is returned explicitly so
	// operators can promote the exact value to entity-bound consumers without
	// guessing from the slug or inspecting credential-bearing configuration.
	EntityID string
}

func NewService(db transactionDB) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("bootstrap database is required")
	}
	queries := generated.New(db)
	applicationService, err := adminapi.NewAdminService(queries, audit.NewService(queries))
	if err != nil {
		return nil, err
	}
	applicationService.SetTxStarter(db)
	return &Service{db: db, queries: queries, applications: applicationService}, nil
}

// Apply creates missing sandbox resources and validates existing ones. It
// never repairs security-sensitive drift, rotates credentials, changes a
// binding, or removes a role. A rerun after success performs no writes.
func (s *Service) Apply(ctx context.Context, cfg Config) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	var entityID string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		entityID, err = s.ensureFoundation(ctx, cfg)
		if err == nil {
			break
		}
		if !isRetryableTransactionError(err) || attempt == 2 {
			return Result{}, err
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		default:
		}
	}
	if err := s.ensureDirectoryReaderApplication(ctx, entityID, cfg); err != nil {
		return Result{}, fmt.Errorf("directory reader application: %w", err)
	}
	return Result{EntityID: entityID}, nil
}

func (s *Service) ensureFoundation(ctx context.Context, cfg Config) (string, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return "", fmt.Errorf("begin bootstrap transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "idbridge:sandbox:"+cfg.EntitySlug); err != nil {
		return "", fmt.Errorf("lock bootstrap: %w", err)
	}
	queries := generated.New(tx)
	auditor := audit.NewService(queries)

	entity, err := ensureEntity(ctx, queries, auditor, cfg)
	if err != nil {
		return "", err
	}
	roles, err := ensureRoles(ctx, queries, auditor, entity.ID)
	if err != nil {
		return "", err
	}
	if err := ensureAdmin(ctx, tx, auditor, entity.ID, cfg); err != nil {
		return "", err
	}
	source, err := ensureFixtureSource(ctx, queries, auditor, entity.ID, cfg.FixtureSourceName)
	if err != nil {
		return "", err
	}
	owner, err := ensureFixture(ctx, queries, auditor, entity.ID, source.ID, fixtureSpec{
		Username:    cfg.FixtureOwnerUsername,
		TROBSUserID: cfg.FixtureOwnerTROBSUserID,
		Owner:       true,
	}, cfg.DefaultLocale)
	if err != nil {
		return "", fmt.Errorf("owner fixture: %w", err)
	}
	unrelated, err := ensureFixture(ctx, queries, auditor, entity.ID, source.ID, fixtureSpec{
		Username: cfg.FixtureUnrelatedUsername,
	}, cfg.DefaultLocale)
	if err != nil {
		return "", fmt.Errorf("unrelated fixture: %w", err)
	}
	if err := ensureFixtureRoles(ctx, queries, auditor, entity.ID, owner.ID, roles, true, cfg.SandboxSingleDeveloper); err != nil {
		return "", fmt.Errorf("owner fixture roles: %w", err)
	}
	if err := ensureFixtureRoles(ctx, queries, auditor, entity.ID, unrelated.ID, roles, false, cfg.SandboxSingleDeveloper); err != nil {
		return "", fmt.Errorf("unrelated fixture roles: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit bootstrap transaction: %w", err)
	}
	return entity.ID, nil
}

func ensureEntity(ctx context.Context, queries *generated.Queries, auditor *audit.Service, cfg Config) (generated.BusinessEntity, error) {
	entity, err := queries.GetEntityBySlug(ctx, cfg.EntitySlug)
	if errors.Is(err, pgx.ErrNoRows) {
		entity, err = queries.CreateEntity(ctx, generated.CreateEntityParams{
			Name: cfg.EntityName, Slug: cfg.EntitySlug, DefaultLocale: cfg.DefaultLocale,
			BrandName: cfg.EntityName, LogoUrl: "", LoginMessage: "",
		})
		if err != nil {
			return generated.BusinessEntity{}, fmt.Errorf("create entity: %w", err)
		}
		if err := writeBootstrapAudit(ctx, auditor, entity.ID, "bootstrap.entity.created", "business_entity", entity.ID, map[string]any{
			"slug": entity.Slug, "name": entity.Name, "default_locale": entity.DefaultLocale,
		}); err != nil {
			return generated.BusinessEntity{}, err
		}
		return entity, nil
	}
	if err != nil {
		return generated.BusinessEntity{}, fmt.Errorf("read entity: %w", err)
	}
	if entity.Name != cfg.EntityName || entity.Status != "active" || entity.DefaultLocale != cfg.DefaultLocale {
		return generated.BusinessEntity{}, fmt.Errorf("entity %q drift: expected active name and locale", cfg.EntitySlug)
	}
	return entity, nil
}

type roleSpec struct {
	code        string
	name        string
	description string
}

func ensureRoles(ctx context.Context, queries *generated.Queries, auditor *audit.Service, entityID string) (map[string]generated.Role, error) {
	specs := []roleSpec{
		{code: EmployeeRoleCode, name: "员工", description: "默认员工角色；用于登录后访问已授权的业务应用。"},
		{code: PAMRequesterRoleCode, name: "ISA PAM Requester", description: "May request ISA privileged access."},
		{code: PAMApproverRoleCode, name: "ISA PAM Approver", description: "May approve ISA privileged access."},
	}
	roles := make(map[string]generated.Role, len(specs))
	for _, spec := range specs {
		role, err := queries.GetRoleByCode(ctx, generated.GetRoleByCodeParams{EntityID: entityID, Code: spec.code})
		if errors.Is(err, pgx.ErrNoRows) {
			role, err = queries.CreateRole(ctx, generated.CreateRoleParams{
				EntityID: entityID, Code: spec.code, Name: spec.name,
				Description: pgtype.Text{String: spec.description, Valid: true},
			})
			if err != nil {
				return nil, fmt.Errorf("create role %q: %w", spec.code, err)
			}
			if err := writeBootstrapAudit(ctx, auditor, entityID, "role.created", "role", role.ID, map[string]string{"code": role.Code, "name": role.Name}); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, fmt.Errorf("read role %q: %w", spec.code, err)
		}
		if role.Name != spec.name || !role.Description.Valid || role.Description.String != spec.description {
			return nil, fmt.Errorf("role %q metadata drift", spec.code)
		}
		roles[spec.code] = role
	}
	return roles, nil
}

func ensureAdmin(ctx context.Context, tx pgx.Tx, auditor *audit.Service, entityID string, cfg Config) error {
	var admin struct {
		id, entityID, status, role                       string
		hasCredential, mustChange, weak, passwordMatches bool
	}
	err := tx.QueryRow(ctx, `
SELECT au.id,
       COALESCE(au.entity_id, ''),
       au.status,
       au.role,
       ac.admin_user_id IS NOT NULL,
       COALESCE(ac.must_change_password, false),
       COALESCE(ac.weak_password, false),
       COALESCE(ac.password_hash = crypt($2, ac.password_hash), false)
FROM admin_users au
LEFT JOIN admin_credentials ac ON ac.admin_user_id = au.id
WHERE au.username = $1`, cfg.AdminUsername, cfg.AdminPassword).Scan(
		&admin.id, &admin.entityID, &admin.status, &admin.role,
		&admin.hasCredential, &admin.mustChange, &admin.weak, &admin.passwordMatches,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		adminEntityID := any(nil)
		if cfg.AdminRole == "enterprise_admin" {
			adminEntityID = entityID
		}
		if err := tx.QueryRow(ctx, `
INSERT INTO admin_users (username, display_name, status, role, entity_id, locale)
VALUES ($1, $1, 'active', $2, $3, $4)
RETURNING id`, cfg.AdminUsername, cfg.AdminRole, adminEntityID, cfg.DefaultLocale).Scan(&admin.id); err != nil {
			return fmt.Errorf("create sandbox administrator: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO admin_credentials (admin_user_id, password_hash, must_change_password, weak_password)
VALUES ($1, crypt($2, gen_salt('bf')), false, false)`, admin.id, cfg.AdminPassword); err != nil {
			return fmt.Errorf("create sandbox administrator credential: %w", err)
		}
		return writeBootstrapAudit(ctx, auditor, entityID, "bootstrap.admin.created", "admin_user", admin.id, map[string]string{
			"username": cfg.AdminUsername, "role": cfg.AdminRole, "status": "active",
		})
	}
	if err != nil {
		return fmt.Errorf("read sandbox administrator: %w", err)
	}
	expectedEntityID := ""
	if cfg.AdminRole == "enterprise_admin" {
		expectedEntityID = entityID
	}
	if admin.status != "active" || admin.role != cfg.AdminRole || admin.entityID != expectedEntityID || !admin.hasCredential {
		return fmt.Errorf("sandbox administrator scope, role, status, or credential drift")
	}
	if admin.passwordMatches && !admin.mustChange && !admin.weak {
		return nil
	}
	// Only the migration's explicit weak first-run state may be hardened. A
	// credential that was already hardened is never rotated by bootstrap.
	if !admin.mustChange || !admin.weak {
		return fmt.Errorf("sandbox administrator credential drift; refusing rotation")
	}
	if admin.passwordMatches {
		if _, err := tx.Exec(ctx, `
UPDATE admin_credentials
SET must_change_password = false, weak_password = false, updated_at = now()
WHERE admin_user_id = $1 AND must_change_password = true AND weak_password = true`, admin.id); err != nil {
			return fmt.Errorf("harden sandbox administrator flags: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
UPDATE admin_credentials
SET password_hash = crypt($2, gen_salt('bf')),
    must_change_password = false,
    weak_password = false,
    password_updated_at = now(),
    updated_at = now()
WHERE admin_user_id = $1 AND must_change_password = true AND weak_password = true`, admin.id, cfg.AdminPassword); err != nil {
			return fmt.Errorf("initialize sandbox administrator credential: %w", err)
		}
	}
	return writeBootstrapAudit(ctx, auditor, entityID, "bootstrap.admin.credential_initialized", "admin_user", admin.id, map[string]any{
		"username": cfg.AdminUsername, "must_change_password": false, "weak_password": false,
	})
}

type identitySource struct {
	ID, Type, Name, Status string
	SyncEnabled            bool
}

func ensureFixtureSource(ctx context.Context, queries *generated.Queries, auditor *audit.Service, entityID, name string) (identitySource, error) {
	sources, err := queries.ListIdentitySources(ctx, generated.ListIdentitySourcesParams{EntityID: entityID, Limit: 1000, Offset: 0})
	if err != nil {
		return identitySource{}, fmt.Errorf("list identity sources: %w", err)
	}
	for _, source := range sources {
		if source.Type != "local" || source.Name != name {
			continue
		}
		if source.Status != "active" || source.SyncEnabled {
			return identitySource{}, fmt.Errorf("fixture identity source drift")
		}
		return identitySource{ID: source.ID, Type: source.Type, Name: source.Name, Status: source.Status, SyncEnabled: source.SyncEnabled}, nil
	}
	created, err := queries.CreateIdentitySource(ctx, generated.CreateIdentitySourceParams{
		EntityID: entityID, Type: "local", Name: name, SyncEnabled: false,
	})
	if err != nil {
		return identitySource{}, fmt.Errorf("create fixture identity source: %w", err)
	}
	if err := writeBootstrapAudit(ctx, auditor, entityID, "bootstrap.identity_source.created", "identity_source", created.ID, map[string]any{
		"type": created.Type, "name": created.Name, "sync_enabled": created.SyncEnabled,
	}); err != nil {
		return identitySource{}, err
	}
	return identitySource{ID: created.ID, Type: created.Type, Name: created.Name, Status: created.Status, SyncEnabled: created.SyncEnabled}, nil
}

type fixtureSpec struct {
	Username    string
	TROBSUserID string
	Owner       bool
}

func ensureFixture(ctx context.Context, queries *generated.Queries, auditor *audit.Service, entityID, sourceID string, spec fixtureSpec, locale string) (generated.User, error) {
	rawProfile := []byte(`{}`)
	if spec.Owner {
		encoded, err := json.Marshal(map[string]any{"external_references": map[string]string{"trobs_user_id": spec.TROBSUserID}})
		if err != nil {
			return generated.User{}, err
		}
		rawProfile = encoded
	}
	directoryUser, err := queries.GetDirectoryUserByExternalID(ctx, generated.GetDirectoryUserByExternalIDParams{
		EntityID: entityID, SourceID: sourceID, ExternalUserID: spec.Username,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		directoryUser, err = queries.UpsertDirectoryUser(ctx, generated.UpsertDirectoryUserParams{
			EntityID: entityID, SourceID: sourceID, ExternalUserID: spec.Username,
			Name: spec.Username, Status: "active", RawProfile: rawProfile,
		})
		if err != nil {
			return generated.User{}, fmt.Errorf("create directory user: %w", err)
		}
		if err := writeBootstrapAudit(ctx, auditor, entityID, "bootstrap.directory_user.created", "directory_user", directoryUser.ID, map[string]string{
			"external_user_id": spec.Username, "source_id": sourceID,
		}); err != nil {
			return generated.User{}, err
		}
	} else if err != nil {
		return generated.User{}, fmt.Errorf("read directory user: %w", err)
	} else if directoryUser.Status != "active" {
		return generated.User{}, fmt.Errorf("directory user lifecycle drift")
	}
	if err := validateFixtureProfile(directoryUser.RawProfile, spec); err != nil {
		return generated.User{}, err
	}

	user, err := queries.GetManagedUserByUsername(ctx, generated.GetManagedUserByUsernameParams{EntityID: entityID, Username: spec.Username})
	if errors.Is(err, pgx.ErrNoRows) {
		user, err = queries.CreateManagedUser(ctx, generated.CreateManagedUserParams{
			EntityID: entityID, Username: spec.Username, DisplayName: spec.Username,
			LifecycleStatus: "active", UserType: "employee",
			PrimarySourceID: pgtype.Text{String: sourceID, Valid: true},
			Locale:          pgtype.Text{String: locale, Valid: true},
		})
		if err != nil {
			return generated.User{}, fmt.Errorf("create managed user: %w", err)
		}
		if err := writeBootstrapAudit(ctx, auditor, entityID, "bootstrap.user.created", "user", user.ID, map[string]string{
			"username": user.Username, "lifecycle_status": user.LifecycleStatus, "user_type": user.UserType,
		}); err != nil {
			return generated.User{}, err
		}
	} else if err != nil {
		return generated.User{}, fmt.Errorf("read managed user: %w", err)
	} else if user.LifecycleStatus != "active" || user.UserType != "employee" || !user.PrimarySourceID.Valid || user.PrimarySourceID.String != sourceID {
		return generated.User{}, fmt.Errorf("managed user lifecycle, type, or source drift")
	}

	binding, err := queries.GetAccountBindingByDirectoryUserID(ctx, generated.GetAccountBindingByDirectoryUserIDParams{
		EntityID: entityID, SourceID: sourceID, DirectoryUserID: directoryUser.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		providerBinding, providerErr := queries.GetAccountBindingByProviderUID(ctx, generated.GetAccountBindingByProviderUIDParams{
			EntityID: entityID, SourceID: sourceID, ProviderUid: spec.Username,
		})
		if providerErr == nil {
			return generated.User{}, fmt.Errorf("provider identity is already bound to user %s", providerBinding.UserID)
		}
		if !errors.Is(providerErr, pgx.ErrNoRows) {
			return generated.User{}, fmt.Errorf("check provider binding: %w", providerErr)
		}
		binding, err = queries.CreateAccountBinding(ctx, generated.CreateAccountBindingParams{
			EntityID: entityID, UserID: user.ID, SourceID: sourceID,
			DirectoryUserID: directoryUser.ID, ProviderUid: spec.Username, IsPrimary: true,
		})
		if err != nil {
			return generated.User{}, fmt.Errorf("create account binding: %w", err)
		}
		if err := writeBootstrapAudit(ctx, auditor, entityID, "user.bound_identity", "user", user.ID, map[string]string{
			"binding_id": binding.ID, "directory_user_id": directoryUser.ID, "source_id": sourceID,
		}); err != nil {
			return generated.User{}, err
		}
	} else if err != nil {
		return generated.User{}, fmt.Errorf("read account binding: %w", err)
	} else if binding.UserID != user.ID || binding.ProviderUid != spec.Username || !binding.IsPrimary || binding.ProviderUnionID.Valid {
		return generated.User{}, fmt.Errorf("account binding drift; refusing rebind")
	}
	return user, nil
}

func validateFixtureProfile(raw []byte, spec fixtureSpec) error {
	var profile struct {
		ExternalReferences map[string]json.RawMessage `json:"external_references"`
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		return fmt.Errorf("fixture raw profile is invalid JSON")
	}
	value, present := profile.ExternalReferences["trobs_user_id"]
	if !spec.Owner {
		if present {
			return fmt.Errorf("unrelated fixture must not have a TROBS reference")
		}
		return nil
	}
	var trobsID string
	if !present || json.Unmarshal(value, &trobsID) != nil || trobsID != spec.TROBSUserID {
		return fmt.Errorf("owner fixture TROBS reference drift")
	}
	return nil
}

func ensureFixtureRoles(ctx context.Context, queries *generated.Queries, auditor *audit.Service, entityID, userID string, roles map[string]generated.Role, owner, singleDeveloper bool) error {
	assigned, err := queries.ListUserRoles(ctx, generated.ListUserRolesParams{EntityID: entityID, UserID: userID})
	if err != nil {
		return fmt.Errorf("list fixture roles: %w", err)
	}
	current := make(map[string]bool, len(assigned))
	for _, role := range assigned {
		current[role.Code] = true
	}
	expectedPAM := expectedPAMRoles(owner, singleDeveloper)
	expected := map[string]bool{EmployeeRoleCode: true}
	for code := range expectedPAM {
		expected[code] = true
	}
	for code := range current {
		if !expected[code] {
			return fmt.Errorf("unexpected authoritative role %q; refusing removal", code)
		}
	}
	codes := make([]string, 0, len(expected))
	for code := range expected {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		if current[code] {
			continue
		}
		role, ok := roles[code]
		if !ok {
			return fmt.Errorf("required role %q is unavailable", code)
		}
		if err := queries.AssignRoleToUser(ctx, generated.AssignRoleToUserParams{EntityID: entityID, UserID: userID, RoleID: role.ID}); err != nil {
			return fmt.Errorf("assign role %q: %w", code, err)
		}
		if err := writeBootstrapAudit(ctx, auditor, entityID, "bootstrap.role.assigned", "user", userID, map[string]string{"role": code}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ensureDirectoryReaderApplication(ctx context.Context, entityID string, cfg Config) error {
	var applicationID string
	err := s.db.QueryRow(ctx, `SELECT id FROM applications WHERE entity_id = $1 AND name = $2`, entityID, cfg.DirectoryReaderApplicationName).Scan(&applicationID)
	if errors.Is(err, pgx.ErrNoRows) {
		pkceRequired := false
		created, createErr := s.applications.CreateApplicationDetailWithClientSecret(ctx, entityID, adminapi.ApplicationWriteInput{
			Name: cfg.DirectoryReaderApplicationName, Type: "oidc_client", Status: "active",
			OIDCClient: &adminapi.ApplicationOIDCClientInput{
				ClientID: cfg.DirectoryReaderClientID, AllowedScopes: []string{DirectoryReadScope},
				GrantTypes: []string{"client_credentials"}, RedirectURIs: []string{},
				ResponseTypes: []string{}, PKCERequired: &pkceRequired,
			},
		}, cfg.DirectoryReaderClientSecret)
		if createErr == nil {
			applicationID = created.ID
			err = nil
		} else if isUniqueViolation(createErr) {
			// Another tokenless bootstrap may have committed the exact application
			// after our foundation transaction released its advisory lock. The
			// unique constraint is the serialization point; reread and validate
			// rather than treating an identical concurrent create as drift.
			if err := s.db.QueryRow(ctx, `SELECT id FROM applications WHERE entity_id = $1 AND name = $2`, entityID, cfg.DirectoryReaderApplicationName).Scan(&applicationID); err != nil {
				return fmt.Errorf("concurrent create readback: %w", err)
			}
			err = nil
		} else {
			return fmt.Errorf("create: %w", createErr)
		}
	}
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	detail, err := s.applications.GetApplicationDetail(ctx, entityID, applicationID)
	if err != nil {
		return fmt.Errorf("read detail: %w", err)
	}
	if detail.Name != cfg.DirectoryReaderApplicationName || detail.Type != "oidc_client" || detail.Status != "active" || detail.OIDCClient == nil {
		return fmt.Errorf("application type or status drift")
	}
	client := detail.OIDCClient
	clientRow, err := s.queries.GetOIDCClientByApplicationID(ctx, generated.GetOIDCClientByApplicationIDParams{
		EntityID: entityID, ApplicationID: applicationID,
	})
	if err != nil {
		return fmt.Errorf("read credential verifier: %w", err)
	}
	credentialMatches := clientRow.ClientSecretHash.Valid && clientsecret.Matches(clientRow.ClientSecretHash.String, cfg.DirectoryReaderClientSecret)
	if client.ClientID != cfg.DirectoryReaderClientID || !credentialMatches ||
		client.Status != "active" || !client.SecretRequired || client.PKCERequired ||
		!sameStrings(client.AllowedScopes, []string{DirectoryReadScope}) ||
		!sameStrings(client.GrantTypes, []string{"client_credentials"}) ||
		len(client.RedirectURIs) != 0 || len(client.ResponseTypes) != 0 ||
		client.WorkplaceProvider != "" || client.WorkplaceAppID != "" || client.WorkplaceAppSecret != "" {
		return fmt.Errorf("OIDC client contract or credential drift; refusing rotation")
	}
	var employeeGrant bool
	if err := s.db.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM application_assignments aa
  JOIN roles r ON r.entity_id = aa.entity_id AND r.id = aa.subject_id
  WHERE aa.entity_id = $1
    AND aa.application_id = $2
    AND aa.subject_type = 'role'
    AND aa.effect = 'allow'
    AND r.code = $3
)`, entityID, applicationID, EmployeeRoleCode).Scan(&employeeGrant); err != nil {
		return fmt.Errorf("read employee assignment: %w", err)
	}
	if !employeeGrant {
		return fmt.Errorf("employee application assignment drift")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isRetryableTransactionError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	a := append([]string(nil), left...)
	b := append([]string(nil), right...)
	sort.Strings(a)
	sort.Strings(b)
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func writeBootstrapAudit(ctx context.Context, auditor *audit.Service, entityID, action, resourceType, resourceID string, after any) error {
	if err := auditor.Write(ctx, audit.Event{
		EntityID: entityID, ActorType: "system", Action: action,
		ResourceType: resourceType, ResourceID: resourceID, After: after,
	}); err != nil {
		return fmt.Errorf("write bootstrap audit for %s: %w", resourceType, err)
	}
	return nil
}
