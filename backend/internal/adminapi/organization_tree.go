// SPDX-License-Identifier: MIT

package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/smices/open-idb/internal/db/generated"
	"github.com/smices/open-idb/internal/ephemeral"
	"github.com/smices/open-idb/internal/id"
)

const organizationTreeCacheTTL = 15 * time.Minute
const organizationTreeVersionTTL = 30 * 24 * time.Hour
const maxExternalReferences = 32
const maxExternalReferenceKeyLength = 64
const maxExternalReferenceValueLength = 256

type OrganizationTreeCache struct {
	store ephemeral.Store
}

func NewOrganizationTreeCache(store ephemeral.Store) *OrganizationTreeCache {
	if store == nil {
		return nil
	}
	return &OrganizationTreeCache{store: store}
}

func (c *OrganizationTreeCache) InvalidateOrganizationTree(ctx context.Context, entityID string) error {
	if c == nil || c.store == nil || entityID == "" {
		return nil
	}
	return c.store.Set(ctx, organizationTreeVersionKey(entityID), []byte(id.NewULID()), organizationTreeVersionTTL)
}

func (s *AdminService) SetOrganizationTreeCache(cache *OrganizationTreeCache) {
	s.organizationTreeCache = cache
}

func (s *AdminService) InvalidateOrganizationTree(ctx context.Context, entityID string) error {
	if s.organizationTreeCache == nil {
		return nil
	}
	return s.organizationTreeCache.InvalidateOrganizationTree(ctx, entityID)
}

func (s *AdminService) ResolveOrganizationTreeEntityID(ctx context.Context, candidate string) (string, error) {
	if candidate != "" {
		if err := id.ValidateULID(candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}
	entities, err := s.queries.ListEntities(ctx, generated.ListEntitiesParams{
		Limit:  1,
		Offset: 0,
	})
	if err != nil {
		return "", err
	}
	if len(entities) == 0 {
		return "", fmt.Errorf("no entity is available")
	}
	return entities[0].ID, nil
}

// GetDirectorySubject returns one current managed subject without consulting
// the organization-tree cache. Authorization consumers use this exact lookup
// so lifecycle offboarding and role removals are visible on the next request.
// Inactive, deleted, unbound, or directory-inactive subjects are not found.
func (s *AdminService) GetDirectorySubject(ctx context.Context, entityID, subjectID string) (OrganizationTreeNode, error) {
	row, err := s.queries.GetActiveDirectoryUserByManagedUserID(ctx, generated.GetActiveDirectoryUserByManagedUserIDParams{
		EntityID: entityID,
		ID:       subjectID,
	})
	if err != nil {
		return OrganizationTreeNode{}, err
	}
	roles, err := s.queries.ListUserRoles(ctx, generated.ListUserRolesParams{EntityID: entityID, UserID: subjectID})
	if err != nil {
		return OrganizationTreeNode{}, err
	}
	node := directoryUserTreeNode(row, subjectID)
	node.Roles = organizationTreeRoleCodes(roles)
	node.Version = directoryAuthorizationVersion(entityID, subjectID, "active", row, node.Roles)
	return node, nil
}

type OrganizationTreeNodeKind string

const (
	organizationTreeKindCompany      OrganizationTreeNodeKind = "company"
	organizationTreeKindOrganization OrganizationTreeNodeKind = "organization"
	organizationTreeKindDepartment   OrganizationTreeNodeKind = "department"
	organizationTreeKindUser         OrganizationTreeNodeKind = "user"
)

type OrganizationTreeNode struct {
	ID                   string                   `json:"id"`
	Kind                 OrganizationTreeNodeKind `json:"kind"`
	Name                 string                   `json:"name"`
	ParentID             string                   `json:"parent_id,omitempty"`
	OrganizationID       string                   `json:"organization_id,omitempty"`
	SourceID             string                   `json:"source_id,omitempty"`
	ExternalDepartmentID string                   `json:"external_department_id,omitempty"`
	EnglishName          string                   `json:"english_name,omitempty"`
	EmployeeNo           string                   `json:"employee_no,omitempty"`
	JobTitle             string                   `json:"job_title,omitempty"`
	Email                string                   `json:"email,omitempty"`
	Phone                string                   `json:"phone,omitempty"`
	ExternalReferences   map[string]string        `json:"external_references,omitempty"`
	Roles                []string                 `json:"roles,omitempty"`
	Version              string                   `json:"version,omitempty"`
	Status               string                   `json:"status,omitempty"`
	HasChildren          bool                     `json:"has_children"`
	UpdatedAt            time.Time                `json:"updated_at,omitempty"`
}

type OrganizationTreeRootResponse struct {
	Root     OrganizationTreeNode   `json:"root"`
	Children []OrganizationTreeNode `json:"children"`
	Limit    int                    `json:"limit"`
	Offset   int                    `json:"offset"`
}

type OrganizationTreeSearchResponse struct {
	Items  []OrganizationTreeNode `json:"items"`
	Total  int64                  `json:"total"`
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

func (s *AdminService) GetOrganizationTreeRoot(ctx context.Context, entityID string, limit, offset int32) (OrganizationTreeRootResponse, error) {
	if limit <= 0 {
		limit = 100
	}
	return cachedOrganizationTreeValue(ctx, s.organizationTreeCache, entityID, "root", "", "", limit, offset, func() (OrganizationTreeRootResponse, error) {
		entity, err := s.queries.GetEntityByID(ctx, entityID)
		if err != nil {
			return OrganizationTreeRootResponse{}, err
		}
		rootOrg, orgErr := s.queries.GetFirstOrganization(ctx, entityID)
		if orgErr != nil && !errors.Is(orgErr, pgx.ErrNoRows) {
			return OrganizationTreeRootResponse{}, orgErr
		}

		root := OrganizationTreeNode{
			ID:   entity.ID,
			Kind: organizationTreeKindCompany,
			Name: entity.Name,
		}
		if entity.BrandName != "" {
			root.Name = entity.BrandName
		}
		displayName := root.Name
		var children []OrganizationTreeNode
		if orgErr == nil {
			root.Name = displayName
			root.OrganizationID = rootOrg.ID
			root.UpdatedAt = rootOrg.UpdatedAt.Time
			children, err = s.listOrganizationRootDepartments(ctx, entityID, rootOrg.ID, limit, offset)
		} else {
			children, err = s.listRootTreeChildren(ctx, entityID, limit, offset)
		}
		if err != nil {
			return OrganizationTreeRootResponse{}, err
		}
		rootUsers, err := s.listRootDirectoryUsers(ctx, entityID, limit, 0)
		if err != nil {
			return OrganizationTreeRootResponse{}, err
		}
		children = append(children, rootUsers...)
		root.HasChildren = len(children) > 0

		return OrganizationTreeRootResponse{
			Root:     root,
			Children: children,
			Limit:    int(limit),
			Offset:   int(offset),
		}, nil
	})
}

func (s *AdminService) ListOrganizationTreeChildren(ctx context.Context, entityID string, kind OrganizationTreeNodeKind, parentID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	if limit <= 0 {
		limit = 100
	}
	return cachedOrganizationTreeValue(ctx, s.organizationTreeCache, entityID, "children", string(kind), parentID, limit, offset, func() ([]OrganizationTreeNode, error) {
		switch kind {
		case organizationTreeKindCompany:
			return s.listCompanyTreeChildren(ctx, entityID, parentID, limit, offset)
		case organizationTreeKindOrganization:
			return s.listOrganizationTreeChildren(ctx, entityID, parentID, limit, offset)
		case organizationTreeKindDepartment:
			return s.listDepartmentTreeChildren(ctx, entityID, parentID, limit, offset)
		default:
			return nil, fmt.Errorf("unsupported organization tree node kind: %s", kind)
		}
	})
}

func (s *AdminService) listCompanyTreeChildren(ctx context.Context, entityID string, companyID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	if companyID == entityID {
		rootOrg, err := s.queries.GetFirstOrganization(ctx, entityID)
		if err == nil {
			return s.listOrganizationRootDepartments(ctx, entityID, rootOrg.ID, limit, offset)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return s.listRootTreeChildren(ctx, entityID, limit, offset)
	}
	if companyID != "" {
		return s.listOrganizationRootDepartments(ctx, entityID, companyID, limit, offset)
	}
	return s.listRootTreeChildren(ctx, entityID, limit, offset)
}

func (s *AdminService) listRootTreeChildren(ctx context.Context, entityID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	deptRows, err := s.queries.ListRootDepartments(ctx, generated.ListRootDepartmentsParams{
		EntityID:       entityID,
		Limit:          limit,
		Offset:         offset,
		OrganizationID: pgtype.Text{},
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]OrganizationTreeNode, 0, len(deptRows))
	for _, row := range deptRows {
		nodes = append(nodes, s.departmentTreeNode(ctx, row))
	}
	return nodes, nil
}

func (s *AdminService) listOrganizationRootDepartments(ctx context.Context, entityID string, organizationID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	deptRows, err := s.queries.ListRootDepartments(ctx, generated.ListRootDepartmentsParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
		OrganizationID: pgtype.Text{
			String: organizationID,
			Valid:  true,
		},
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]OrganizationTreeNode, 0, len(deptRows))
	for _, row := range deptRows {
		nodes = append(nodes, s.departmentTreeNode(ctx, row))
	}
	return nodes, nil
}

func (s *AdminService) listOrganizationTreeChildren(ctx context.Context, entityID string, organizationID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	orgRows, err := s.queries.ListChildOrganizations(ctx, generated.ListChildOrganizationsParams{
		EntityID: entityID,
		ParentID: pgtype.Text{
			String: organizationID,
			Valid:  true,
		},
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]OrganizationTreeNode, 0, len(orgRows)+16)
	for _, row := range orgRows {
		nodes = append(nodes, s.organizationTreeNode(ctx, row))
	}
	deptRows, err := s.queries.ListRootDepartments(ctx, generated.ListRootDepartmentsParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   0,
		OrganizationID: pgtype.Text{
			String: organizationID,
			Valid:  true,
		},
	})
	if err != nil {
		return nil, err
	}
	for _, row := range deptRows {
		nodes = append(nodes, s.departmentTreeNode(ctx, row))
	}
	return nodes, nil
}

func (s *AdminService) listDepartmentTreeChildren(ctx context.Context, entityID string, departmentID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	department, err := s.queries.GetDepartmentByID(ctx, generated.GetDepartmentByIDParams{
		EntityID: entityID,
		ID:       departmentID,
	})
	if err != nil {
		return nil, err
	}
	deptRows, err := s.queries.ListChildDepartments(ctx, generated.ListChildDepartmentsParams{
		EntityID: entityID,
		ParentID: pgtype.Text{
			String: departmentID,
			Valid:  true,
		},
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]OrganizationTreeNode, 0, len(deptRows)+32)
	for _, row := range deptRows {
		nodes = append(nodes, s.departmentTreeNode(ctx, row))
	}
	if !department.SourceID.Valid || !department.ExternalDepartmentID.Valid {
		return nodes, nil
	}
	userRows, err := s.queries.ListDirectoryUsersByDepartmentExternalID(ctx, generated.ListDirectoryUsersByDepartmentExternalIDParams{
		EntityID: entityID,
		SourceID: department.SourceID.String,
		Column3:  department.ExternalDepartmentID.String,
		Limit:    limit,
		Offset:   0,
	})
	if err != nil {
		return nil, err
	}
	userNodes, err := s.boundDirectoryUserTreeNodes(ctx, entityID, userRows)
	if err != nil {
		return nil, err
	}
	nodes = append(nodes, userNodes...)
	return nodes, nil
}

func (s *AdminService) organizationTreeNode(ctx context.Context, row generated.Organization) OrganizationTreeNode {
	childCount, _ := s.queries.CountChildOrganizations(ctx, generated.CountChildOrganizationsParams{
		EntityID: row.EntityID,
		ParentID: pgtype.Text{
			String: row.ID,
			Valid:  true,
		},
	})
	if childCount == 0 {
		departments, _ := s.queries.ListRootDepartments(ctx, generated.ListRootDepartmentsParams{
			EntityID: row.EntityID,
			Limit:    1,
			Offset:   0,
			OrganizationID: pgtype.Text{
				String: row.ID,
				Valid:  true,
			},
		})
		childCount = int64(len(departments))
	}
	parentID := ""
	if row.ParentID.Valid {
		parentID = row.ParentID.String
	}
	return OrganizationTreeNode{
		ID:          row.ID,
		Kind:        organizationTreeKindOrganization,
		Name:        row.Name,
		ParentID:    parentID,
		HasChildren: childCount > 0,
		UpdatedAt:   row.UpdatedAt.Time,
	}
}

func (s *AdminService) departmentTreeNode(ctx context.Context, row generated.Department) OrganizationTreeNode {
	childCount, _ := s.queries.CountChildDepartments(ctx, generated.CountChildDepartmentsParams{
		EntityID: row.EntityID,
		ParentID: pgtype.Text{
			String: row.ID,
			Valid:  true,
		},
	})
	if childCount == 0 && row.SourceID.Valid && row.ExternalDepartmentID.Valid {
		childCount, _ = s.queries.CountDirectoryUsersByDepartmentExternalID(ctx, generated.CountDirectoryUsersByDepartmentExternalIDParams{
			EntityID: row.EntityID,
			SourceID: row.SourceID.String,
			Column3:  row.ExternalDepartmentID.String,
		})
	}
	parentID := ""
	if row.ParentID.Valid {
		parentID = row.ParentID.String
	}
	return OrganizationTreeNode{
		ID:                   row.ID,
		Kind:                 organizationTreeKindDepartment,
		Name:                 row.Name,
		ParentID:             parentID,
		OrganizationID:       row.OrganizationID,
		SourceID:             textString(row.SourceID),
		ExternalDepartmentID: textString(row.ExternalDepartmentID),
		HasChildren:          childCount > 0,
		UpdatedAt:            row.UpdatedAt.Time,
	}
}

func directoryUserTreeNode(row generated.DirectoryUser, subjectID string) OrganizationTreeNode {
	return OrganizationTreeNode{
		ID:                 subjectID,
		Kind:               organizationTreeKindUser,
		Name:               row.Name,
		SourceID:           row.SourceID,
		EnglishName:        row.EnglishName,
		EmployeeNo:         row.EmployeeNo,
		JobTitle:           row.JobTitle,
		Email:              textString(row.Email),
		Phone:              textString(row.Phone),
		ExternalReferences: directoryUserExternalReferences(row.RawProfile),
		Status:             row.Status,
		HasChildren:        false,
		UpdatedAt:          row.UpdatedAt.Time,
	}
}

// boundDirectoryUserTreeNode emits only users with one managed IdBridge
// subject. OIDC tokens use the managed user's ID as `sub`; exposing the raw
// directory-row ID here would make the assertion-to-directory join ambiguous.
// An unbound source user is intentionally not eligible for the OAuth directory
// projection, and a database failure aborts the response rather than guessing.
func (s *AdminService) boundDirectoryUserTreeNode(ctx context.Context, entityID string, row generated.DirectoryUser) (OrganizationTreeNode, bool, error) {
	nodes, err := s.boundDirectoryUserTreeNodes(ctx, entityID, []generated.DirectoryUser{row})
	if err != nil {
		return OrganizationTreeNode{}, false, err
	}
	if len(nodes) == 0 {
		return OrganizationTreeNode{}, false, nil
	}
	return nodes[0], true, nil
}

// boundDirectoryUserTreeNodes resolves all bindings, lifecycle states, and
// authoritative role codes for one response page in a single database query.
// The directory list endpoints cap pages at 100 rows; keeping this projection
// batched prevents a cache miss from turning into hundreds of serial queries.
func (s *AdminService) boundDirectoryUserTreeNodes(ctx context.Context, entityID string, rows []generated.DirectoryUser) ([]OrganizationTreeNode, error) {
	if len(rows) == 0 {
		return []OrganizationTreeNode{}, nil
	}
	ids := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		ids = append(ids, row.ID)
	}
	projections, err := s.queries.ListBoundDirectoryUserTreeNodes(ctx, generated.ListBoundDirectoryUserTreeNodesParams{
		EntityID: entityID, DirectoryUserIds: ids,
	})
	if err != nil {
		return nil, err
	}
	type projection struct {
		userID, lifecycle string
		roles             map[string]struct{}
	}
	byDirectoryID := make(map[string]*projection, len(projections))
	for _, item := range projections {
		current := byDirectoryID[item.ID]
		if current == nil {
			current = &projection{userID: item.UserID, lifecycle: item.LifecycleStatus, roles: make(map[string]struct{})}
			byDirectoryID[item.ID] = current
		}
		if item.RoleCode.Valid {
			current.roles[item.RoleCode.String] = struct{}{}
		}
	}
	nodes := make([]OrganizationTreeNode, 0, len(rows))
	for _, row := range rows {
		item := byDirectoryID[row.ID]
		if item == nil || item.userID == "" {
			continue
		}
		roles := make([]string, 0, len(item.roles))
		for role := range item.roles {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		node := directoryUserTreeNode(row, item.userID)
		node.Roles = roles
		if item.lifecycle != "active" {
			node.Status = item.lifecycle
		}
		node.Version = directoryAuthorizationVersion(entityID, item.userID, item.lifecycle, row, roles)
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// organizationTreeRoleCodes projects only IdBridge's authoritative role
// assignments. Directory raw_profile fields (including TROBS references) are
// intentionally not an input to authorization.
func organizationTreeRoleCodes(roles []generated.Role) []string {
	if len(roles) == 0 {
		return nil
	}
	codes := make([]string, 0, len(roles))
	for _, role := range roles {
		codes = append(codes, role.Code)
	}
	sort.Strings(codes)
	return codes
}

// directoryAuthorizationVersion is an opaque, state-derived revision for IAM
// membership proofs. It excludes volatile sync/display timestamps and includes
// only the active managed lifecycle, selected binding target, directory state,
// and the canonical authoritative role set.
func directoryAuthorizationVersion(entityID, subjectID, lifecycleStatus string, row generated.DirectoryUser, roles []string) string {
	canonicalRoles := append([]string(nil), roles...)
	sort.Strings(canonicalRoles)
	payload, err := json.Marshal(struct {
		Schema          string   `json:"schema"`
		EntityID        string   `json:"entity_id"`
		SubjectID       string   `json:"subject_id"`
		LifecycleStatus string   `json:"lifecycle_status"`
		SourceID        string   `json:"binding_source_id"`
		DirectoryUserID string   `json:"binding_directory_user_id"`
		DirectoryStatus string   `json:"directory_status"`
		Roles           []string `json:"roles"`
	}{
		Schema:          "idbridge-directory-authorization/v1",
		EntityID:        entityID,
		SubjectID:       subjectID,
		LifecycleStatus: lifecycleStatus,
		SourceID:        row.SourceID,
		DirectoryUserID: row.ID,
		DirectoryStatus: row.Status,
		Roles:           canonicalRoles,
	})
	if err != nil {
		panic("marshal directory authorization version: " + err.Error())
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("sha256:%x", digest)
}

// directoryUserExternalReferences projects only a small, opaque, string-only
// subset of source metadata. It gives an enterprise directory a stable join
// point for customer business identifiers without leaking a source RawProfile
// or allowing arbitrary nested provider data onto the directory API.
func directoryUserExternalReferences(rawProfile []byte) map[string]string {
	var profile struct {
		ExternalReferences map[string]json.RawMessage `json:"external_references"`
	}
	if len(rawProfile) == 0 || json.Unmarshal(rawProfile, &profile) != nil {
		return nil
	}
	if len(profile.ExternalReferences) > maxExternalReferences {
		return nil
	}
	references := make(map[string]string, len(profile.ExternalReferences))
	for key, rawValue := range profile.ExternalReferences {
		if !validExternalReferenceKey(key) {
			continue
		}
		var value string
		if json.Unmarshal(rawValue, &value) != nil || !validExternalReferenceValue(value) {
			continue
		}
		references[key] = value
	}
	if len(references) == 0 {
		return nil
	}
	return references
}

func validExternalReferenceKey(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= maxExternalReferenceKeyLength
}

func validExternalReferenceValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= maxExternalReferenceValueLength
}

func (s *AdminService) listRootDirectoryUsers(ctx context.Context, entityID string, limit, offset int32) ([]OrganizationTreeNode, error) {
	userRows, err := s.queries.ListRootDirectoryUsers(ctx, generated.ListRootDirectoryUsersParams{
		EntityID: entityID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]OrganizationTreeNode, 0, len(userRows))
	boundNodes, err := s.boundDirectoryUserTreeNodes(ctx, entityID, userRows)
	if err != nil {
		return nil, err
	}
	nodes = append(nodes, boundNodes...)
	return nodes, nil
}

func (s *AdminService) SearchOrganizationTree(ctx context.Context, entityID, query string, limit, offset int32) (OrganizationTreeSearchResponse, error) {
	if limit <= 0 {
		limit = 100
	}
	if query == "" {
		return OrganizationTreeSearchResponse{Items: []OrganizationTreeNode{}, Limit: int(limit), Offset: int(offset)}, nil
	}
	deptRows, err := s.queries.SearchOrganizationTreeDepartments(ctx, generated.SearchOrganizationTreeDepartmentsParams{
		EntityID: entityID,
		Column2:  query,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return OrganizationTreeSearchResponse{}, err
	}
	userRows, err := s.queries.SearchOrganizationTreeUsers(ctx, generated.SearchOrganizationTreeUsersParams{
		EntityID: entityID,
		Column2:  query,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return OrganizationTreeSearchResponse{}, err
	}
	nodes := make([]OrganizationTreeNode, 0, len(deptRows)+len(userRows))
	for _, row := range deptRows {
		nodes = append(nodes, s.departmentTreeNode(ctx, row))
	}
	boundNodes, err := s.boundDirectoryUserTreeNodes(ctx, entityID, userRows)
	if err != nil {
		return OrganizationTreeSearchResponse{}, err
	}
	nodes = append(nodes, boundNodes...)
	return OrganizationTreeSearchResponse{
		Items:  nodes,
		Total:  int64(len(nodes)),
		Limit:  int(limit),
		Offset: int(offset),
	}, nil
}

func organizationTreeVersionKey(entityID string) string {
	return "orgtree:version:" + entityID
}

func organizationTreeCacheVersion(ctx context.Context, cache *OrganizationTreeCache, entityID string) string {
	if cache == nil || cache.store == nil {
		return ""
	}
	value, ok, err := cache.store.Get(ctx, organizationTreeVersionKey(entityID))
	if err != nil || !ok || len(value) == 0 {
		return "0"
	}
	return string(value)
}

func cachedOrganizationTreeValue[T any](
	ctx context.Context,
	cache *OrganizationTreeCache,
	entityID string,
	scope string,
	kind string,
	parentID string,
	limit int32,
	offset int32,
	load func() (T, error),
) (T, error) {
	var zero T
	if cache == nil || cache.store == nil {
		return load()
	}
	version := organizationTreeCacheVersion(ctx, cache, entityID)
	key := ephemeral.Key("orgtree", entityID, version, scope, kind, parentID, fmt.Sprintf("%d", limit), fmt.Sprintf("%d", offset))
	if raw, ok, err := cache.store.Get(ctx, key); err == nil && ok {
		var cached T
		if err := json.Unmarshal(raw, &cached); err == nil {
			return cached, nil
		}
	}
	value, err := load()
	if err != nil {
		return zero, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return zero, err
	}
	_ = cache.store.Set(ctx, key, raw, organizationTreeCacheTTL)
	return value, nil
}
