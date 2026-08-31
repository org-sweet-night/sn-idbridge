-- SPDX-License-Identifier: MIT

-- name: CreateEntity :one
INSERT INTO business_entities (name, slug, status, default_locale, brand_name, logo_url, login_message)
VALUES ($1, $2, 'active', $3, $4, $5, $6)
RETURNING id, name, slug, status, default_locale, brand_name, logo_url, login_message, created_at;

-- name: ListEntities :many
SELECT id, name, slug, status, default_locale, brand_name, logo_url, login_message, created_at
FROM business_entities
ORDER BY created_at DESC, name ASC
LIMIT $1 OFFSET $2;

-- name: CountEntities :one
SELECT count(*) FROM business_entities;

-- name: GetEntityByID :one
SELECT id, name, slug, status, default_locale, brand_name, logo_url, login_message, created_at
FROM business_entities
WHERE id = $1;

-- name: GetEntityBySlug :one
SELECT id, name, slug, status, default_locale, brand_name, logo_url, login_message, created_at
FROM business_entities
WHERE slug = $1;

-- name: UpdateEntity :one
UPDATE business_entities
SET
    name = COALESCE(sqlc.narg('name'), name),
    status = COALESCE(sqlc.narg('status'), status),
    default_locale = COALESCE(sqlc.narg('default_locale'), default_locale),
    brand_name = COALESCE(sqlc.narg('brand_name'), brand_name),
    logo_url = COALESCE(sqlc.narg('logo_url'), logo_url),
    login_message = COALESCE(sqlc.narg('login_message'), login_message)
WHERE id = sqlc.arg('id')
RETURNING id, name, slug, status, default_locale, brand_name, logo_url, login_message, created_at;

-- name: CreateIdentitySource :one
INSERT INTO identity_sources (entity_id, type, name, status, sync_enabled)
VALUES ($1, $2, $3, 'active', $4)
RETURNING id, entity_id, type, name, status, sync_enabled, created_at;

-- name: UpsertDirectoryUser :one
INSERT INTO directory_users (
    entity_id,
    source_id,
    external_user_id,
    external_union_id,
    external_open_id,
    name,
    english_name,
    employee_no,
    job_title,
    email,
    phone,
    avatar_url,
    status,
    raw_profile,
    last_synced_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now()
)
ON CONFLICT (entity_id, source_id, external_user_id)
DO UPDATE SET
    external_union_id = EXCLUDED.external_union_id,
    external_open_id = EXCLUDED.external_open_id,
    name = EXCLUDED.name,
    english_name = EXCLUDED.english_name,
    employee_no = EXCLUDED.employee_no,
    job_title = EXCLUDED.job_title,
    email = EXCLUDED.email,
    phone = EXCLUDED.phone,
    avatar_url = EXCLUDED.avatar_url,
    status = EXCLUDED.status,
    raw_profile = EXCLUDED.raw_profile,
    last_synced_at = now(),
    updated_at = now()
RETURNING id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at;

-- name: CreateManagedUser :one
INSERT INTO users (
    entity_id,
    username,
    display_name,
    english_name,
    employee_no,
    job_title,
    email,
    phone,
    avatar_url,
    lifecycle_status,
    user_type,
    primary_source_id,
    locale
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
RETURNING id, entity_id, username, display_name, english_name, employee_no, job_title, email, phone, avatar_url, lifecycle_status, user_type, primary_source_id, locale, created_at, updated_at;

-- name: CreateAccountBinding :one
INSERT INTO account_bindings (
    entity_id,
    user_id,
    source_id,
    directory_user_id,
    provider_uid,
    provider_union_id,
    is_primary
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING id, entity_id, user_id, source_id, directory_user_id, provider_uid, provider_union_id, is_primary, bound_at;

-- name: UpsertDirectoryDepartment :one
INSERT INTO directory_departments (
    entity_id,
    source_id,
    external_department_id,
    parent_external_department_id,
    name,
    raw_profile,
    last_synced_at
) VALUES (
    $1, $2, $3, $4, $5, $6, now()
)
ON CONFLICT (entity_id, source_id, external_department_id)
DO UPDATE SET
    parent_external_department_id = EXCLUDED.parent_external_department_id,
    name = EXCLUDED.name,
    raw_profile = EXCLUDED.raw_profile,
    last_synced_at = now(),
    updated_at = now()
RETURNING id, entity_id, source_id, external_department_id, parent_external_department_id, name, raw_profile, last_synced_at, created_at, updated_at;

-- name: DeleteMissingDirectoryDepartments :many
DELETE FROM directory_departments
WHERE entity_id = $1
  AND source_id = $2
  AND NOT (external_department_id = ANY(sqlc.arg('present_external_department_ids')::text[]))
RETURNING id, entity_id, source_id, external_department_id, parent_external_department_id, name, raw_profile, last_synced_at, created_at, updated_at;

-- name: GetAccountBindingByProviderUID :one
SELECT id, entity_id, user_id, source_id, directory_user_id, provider_uid, provider_union_id, is_primary, bound_at
FROM account_bindings
WHERE entity_id = $1 AND source_id = $2 AND provider_uid = $3;

-- name: GetAccountBindingByProviderUnionID :one
SELECT id, entity_id, user_id, source_id, directory_user_id, provider_uid, provider_union_id, is_primary, bound_at
FROM account_bindings
WHERE entity_id = $1 AND source_id = $2 AND provider_union_id = $3 AND provider_union_id <> ''
LIMIT 1;

-- name: ListAccountBindingsByProviderUnionID :many
SELECT id, entity_id, user_id, source_id, directory_user_id, provider_uid, provider_union_id, is_primary, bound_at
FROM account_bindings
WHERE entity_id = $1 AND source_id = $2 AND provider_union_id = $3 AND provider_union_id <> ''
ORDER BY bound_at ASC, id ASC;

-- name: UpdateAccountBindingFromDirectory :one
UPDATE account_bindings
SET directory_user_id = $4,
    provider_uid = $5,
    provider_union_id = $6,
    is_primary = true
WHERE entity_id = $1 AND source_id = $2 AND id = $3
RETURNING id, entity_id, user_id, source_id, directory_user_id, provider_uid, provider_union_id, is_primary, bound_at;

-- name: GetDirectoryUserByExternalID :one
SELECT id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at
FROM directory_users
WHERE entity_id = $1 AND source_id = $2 AND external_user_id = $3;

-- name: GetDirectoryUserByProviderIdentifier :one
SELECT id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at
FROM directory_users
WHERE entity_id = sqlc.arg('entity_id')
  AND source_id = sqlc.arg('source_id')
  AND sqlc.arg('identifier')::text <> ''
  AND CASE lower(sqlc.arg('identifier_type')::text)
    WHEN 'user_id' THEN
      external_user_id = sqlc.arg('identifier')::text
      OR raw_profile->>'user_id' = sqlc.arg('identifier')::text
    WHEN 'open_id' THEN
      external_open_id = sqlc.arg('identifier')::text
      OR external_user_id = sqlc.arg('identifier')::text
      OR raw_profile->>'open_id' = sqlc.arg('identifier')::text
    WHEN 'union_id' THEN
      external_union_id = sqlc.arg('identifier')::text
      OR external_user_id = sqlc.arg('identifier')::text
      OR raw_profile->>'union_id' = sqlc.arg('identifier')::text
    ELSE
	  external_user_id = sqlc.arg('identifier')::text
	  OR external_open_id = sqlc.arg('identifier')::text
	  OR external_union_id = sqlc.arg('identifier')::text
	  OR raw_profile->>'user_id' = sqlc.arg('identifier')::text
	  OR raw_profile->>'open_id' = sqlc.arg('identifier')::text
	  OR raw_profile->>'union_id' = sqlc.arg('identifier')::text
  END
ORDER BY (external_user_id = sqlc.arg('identifier')::text) DESC, created_at ASC
LIMIT 1;

-- name: ListDirectoryUsersByProviderIdentifier :many
SELECT id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at
FROM directory_users
WHERE entity_id = sqlc.arg('entity_id')
  AND source_id = sqlc.arg('source_id')
  AND sqlc.arg('identifier')::text <> ''
  AND CASE lower(sqlc.arg('identifier_type')::text)
    WHEN 'user_id' THEN
      external_user_id = sqlc.arg('identifier')::text
      OR raw_profile->>'user_id' = sqlc.arg('identifier')::text
    WHEN 'open_id' THEN
      external_open_id = sqlc.arg('identifier')::text
      OR external_user_id = sqlc.arg('identifier')::text
      OR raw_profile->>'open_id' = sqlc.arg('identifier')::text
    WHEN 'union_id' THEN
      external_union_id = sqlc.arg('identifier')::text
      OR external_user_id = sqlc.arg('identifier')::text
      OR raw_profile->>'union_id' = sqlc.arg('identifier')::text
    ELSE
      false
  END
ORDER BY created_at ASC, id ASC;

-- name: UpdateDirectoryUserByID :one
UPDATE directory_users
SET external_user_id = sqlc.arg('external_user_id'),
    external_union_id = sqlc.narg('external_union_id'),
    external_open_id = sqlc.narg('external_open_id'),
    name = sqlc.arg('name'),
    english_name = sqlc.arg('english_name'),
    employee_no = sqlc.arg('employee_no'),
    job_title = sqlc.arg('job_title'),
    email = sqlc.narg('email'),
    phone = sqlc.narg('phone'),
    avatar_url = sqlc.narg('avatar_url'),
    status = sqlc.arg('status'),
    raw_profile = sqlc.arg('raw_profile'),
    last_synced_at = now(),
    updated_at = now()
WHERE entity_id = sqlc.arg('entity_id')
  AND source_id = sqlc.arg('source_id')
  AND id = sqlc.arg('id')
RETURNING id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at;

-- name: MarkDirectoryUserDeletedByID :one
UPDATE directory_users
SET status = 'deleted',
    last_synced_at = now(),
    updated_at = now()
WHERE entity_id = $1 AND source_id = $2 AND id = $3
RETURNING id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at;

-- name: GetAccountBindingByDirectoryUserID :one
SELECT id, entity_id, user_id, source_id, directory_user_id, provider_uid, provider_union_id, is_primary, bound_at
FROM account_bindings
WHERE entity_id = $1 AND source_id = $2 AND directory_user_id = $3;

-- name: GetManagedUserForDeletedDirectoryUser :one
SELECT u.id, u.entity_id, u.username, u.display_name, u.english_name, u.employee_no, u.job_title, u.email, u.phone, u.avatar_url, u.lifecycle_status, u.user_type, u.primary_source_id, u.locale, u.created_at, u.updated_at
FROM users u
JOIN account_bindings deleted_binding
  ON deleted_binding.entity_id = u.entity_id
 AND deleted_binding.user_id = u.id
JOIN directory_users deleted_user
  ON deleted_user.entity_id = deleted_binding.entity_id
 AND deleted_user.source_id = deleted_binding.source_id
 AND deleted_user.id = deleted_binding.directory_user_id
WHERE deleted_binding.entity_id = $1
  AND deleted_binding.source_id = $2
  AND deleted_binding.directory_user_id = $3
  AND deleted_user.status = 'deleted'
  AND NOT EXISTS (
    SELECT 1
    FROM account_bindings active_binding
    JOIN directory_users active_user
      ON active_user.entity_id = active_binding.entity_id
     AND active_user.source_id = active_binding.source_id
     AND active_user.id = active_binding.directory_user_id
    WHERE active_binding.entity_id = u.entity_id
      AND active_binding.user_id = u.id
      AND active_user.status <> 'deleted'
  )
LIMIT 1;

-- name: GetDirectoryDepartmentByProviderIdentifier :one
SELECT id, entity_id, source_id, external_department_id, parent_external_department_id, name, raw_profile, last_synced_at, created_at, updated_at
FROM directory_departments
WHERE entity_id = sqlc.arg('entity_id')
  AND source_id = sqlc.arg('source_id')
  AND sqlc.arg('identifier')::text <> ''
  AND CASE lower(sqlc.arg('identifier_type')::text)
    WHEN 'department_id' THEN
      external_department_id = sqlc.arg('identifier')::text
      OR raw_profile->>'department_id' = sqlc.arg('identifier')::text
    WHEN 'open_department_id' THEN
      external_department_id = sqlc.arg('identifier')::text
      OR raw_profile->>'open_department_id' = sqlc.arg('identifier')::text
    ELSE
	  external_department_id = sqlc.arg('identifier')::text
	  OR raw_profile->>'department_id' = sqlc.arg('identifier')::text
	  OR raw_profile->>'open_department_id' = sqlc.arg('identifier')::text
  END
ORDER BY (external_department_id = sqlc.arg('identifier')::text) DESC, created_at ASC
LIMIT 1;

-- name: ListDirectoryDepartmentsByProviderIdentifier :many
SELECT id, entity_id, source_id, external_department_id, parent_external_department_id, name, raw_profile, last_synced_at, created_at, updated_at
FROM directory_departments
WHERE entity_id = sqlc.arg('entity_id')
  AND source_id = sqlc.arg('source_id')
  AND sqlc.arg('identifier')::text <> ''
  AND CASE lower(sqlc.arg('identifier_type')::text)
    WHEN 'department_id' THEN
      external_department_id = sqlc.arg('identifier')::text
      OR raw_profile->>'department_id' = sqlc.arg('identifier')::text
    WHEN 'open_department_id' THEN
      external_department_id = sqlc.arg('identifier')::text
      OR raw_profile->>'open_department_id' = sqlc.arg('identifier')::text
    ELSE
      external_department_id = sqlc.arg('identifier')::text
      OR raw_profile->>'department_id' = sqlc.arg('identifier')::text
      OR raw_profile->>'open_department_id' = sqlc.arg('identifier')::text
  END
ORDER BY created_at ASC, id ASC;

-- name: DeleteDirectoryDepartmentByID :one
DELETE FROM directory_departments
WHERE entity_id = $1 AND source_id = $2 AND id = $3
RETURNING id, entity_id, source_id, external_department_id, parent_external_department_id, name, raw_profile, last_synced_at, created_at, updated_at;

-- name: GetManagedUserByUsername :one
SELECT id, entity_id, username, display_name, english_name, employee_no, job_title, email, phone, avatar_url, lifecycle_status, user_type, primary_source_id, locale, created_at, updated_at
FROM users
WHERE entity_id = $1 AND username = $2
LIMIT 1;

-- name: GetActiveDirectoryUserByManagedUserID :one
SELECT du.id, du.entity_id, du.source_id, du.external_user_id, du.external_union_id,
       du.external_open_id, du.name, du.english_name, du.employee_no,
       du.job_title, du.email, du.phone, du.avatar_url, du.status,
       du.raw_profile, du.last_synced_at, du.created_at, du.updated_at
FROM users managed
JOIN account_bindings binding
  ON binding.entity_id = managed.entity_id
 AND binding.user_id = managed.id
JOIN directory_users du
  ON du.entity_id = binding.entity_id
 AND du.source_id = binding.source_id
 AND du.id = binding.directory_user_id
WHERE managed.entity_id = $1
  AND managed.id = $2
  AND managed.lifecycle_status = 'active'
  AND du.status = 'active'
ORDER BY binding.is_primary DESC, binding.bound_at ASC, du.id ASC
LIMIT 1;

-- name: CountCurrentDirectoryUsersBySource :one
SELECT count(*)::bigint
FROM directory_users
WHERE entity_id = $1 AND source_id = $2 AND status <> 'deleted';

-- name: CountCurrentDirectoryDepartmentsBySource :one
SELECT count(*)::bigint
FROM directory_departments
WHERE entity_id = $1 AND source_id = $2;

-- name: GetManagedUserByBinding :one
SELECT u.id, u.entity_id, u.username, u.display_name, u.english_name, u.employee_no, u.job_title, u.email, u.phone, u.avatar_url, u.lifecycle_status, u.user_type, u.primary_source_id, u.locale, u.created_at, u.updated_at
FROM users u
JOIN account_bindings ab ON ab.entity_id = u.entity_id AND ab.user_id = u.id
WHERE ab.entity_id = $1 AND ab.source_id = $2 AND ab.provider_uid = $3;

-- name: UpdateManagedUserFromDirectory :one
UPDATE users
SET username = COALESCE(sqlc.narg('username'), username),
    display_name = sqlc.arg('display_name'),
    english_name = sqlc.arg('english_name'),
    employee_no = sqlc.arg('employee_no'),
    job_title = sqlc.arg('job_title'),
    email = sqlc.arg('email'),
    phone = sqlc.arg('phone'),
    avatar_url = sqlc.arg('avatar_url'),
    lifecycle_status = sqlc.arg('lifecycle_status'),
    primary_source_id = sqlc.arg('primary_source_id'),
    updated_at = now()
WHERE entity_id = sqlc.arg('entity_id') AND id = sqlc.arg('id')
RETURNING id, entity_id, username, display_name, english_name, employee_no, job_title, email, phone, avatar_url, lifecycle_status, user_type, primary_source_id, locale, created_at, updated_at;

-- name: MarkMissingDirectoryUsersDeleted :many
UPDATE directory_users
SET status = 'deleted',
    last_synced_at = now(),
    updated_at = now()
WHERE entity_id = $1
  AND source_id = $2
  AND status <> 'deleted'
  AND NOT (external_user_id = ANY(sqlc.arg('present_external_user_ids')::text[]))
RETURNING id, entity_id, source_id, external_user_id, external_union_id, external_open_id, name, english_name, employee_no, job_title, email, phone, avatar_url, status, raw_profile, last_synced_at, created_at, updated_at;

-- name: ListManagedUsersForDeletedDirectoryUsers :many
SELECT DISTINCT u.id, u.entity_id, u.username, u.display_name, u.english_name, u.employee_no, u.job_title, u.email, u.phone, u.avatar_url, u.lifecycle_status, u.user_type, u.primary_source_id, u.locale, u.created_at, u.updated_at
FROM users u
JOIN account_bindings ab
  ON ab.entity_id = u.entity_id
 AND ab.user_id = u.id
JOIN directory_users du
  ON du.entity_id = ab.entity_id
 AND du.source_id = ab.source_id
 AND du.id = ab.directory_user_id
WHERE u.entity_id = $1
  AND ab.entity_id = $1
  AND ab.source_id = $2
  AND du.status = 'deleted'
  AND NOT EXISTS (
    SELECT 1
    FROM account_bindings active_ab
    JOIN directory_users active_du
      ON active_du.entity_id = active_ab.entity_id
     AND active_du.source_id = active_ab.source_id
     AND active_du.id = active_ab.directory_user_id
    WHERE active_ab.entity_id = u.entity_id
      AND active_ab.user_id = u.id
      AND active_du.status <> 'deleted'
  )
;
