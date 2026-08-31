-- SPDX-License-Identifier: MIT

-- name: CreateApplication :one
INSERT INTO applications (entity_id, name, type, status, config)
VALUES ($1, $2, $3, COALESCE(sqlc.narg('status')::text, 'active'), COALESCE(sqlc.narg('config')::jsonb, '{}'::jsonb))
RETURNING id, entity_id, name, type, status, created_at, updated_at, config;

-- name: CreateOIDCClient :one
INSERT INTO oidc_clients (
    entity_id,
    application_id,
    client_id,
    client_secret_hash,
    redirect_uris,
    allowed_scopes,
    grant_types,
    response_types,
    pkce_required,
    workplace_provider,
    workplace_app_id,
    workplace_app_secret,
    secret_required,
    status
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, true, COALESCE(sqlc.narg('status')::text, 'active')
)
RETURNING id, entity_id, application_id, client_id, client_secret_hash, redirect_uris, allowed_scopes, grant_types, response_types, pkce_required, workplace_provider, workplace_app_id, workplace_app_secret, status, created_at, updated_at, secret_required;

-- name: GetOIDCClientByClientID :one
SELECT c.id, c.entity_id, c.application_id, c.client_id, c.client_secret_hash, c.redirect_uris, c.allowed_scopes, c.grant_types, c.response_types, c.pkce_required, c.workplace_provider, c.workplace_app_id, c.workplace_app_secret, c.status, c.created_at, c.updated_at, c.secret_required
FROM oidc_clients c
JOIN applications a ON a.entity_id = c.entity_id AND a.id = c.application_id
WHERE c.entity_id = $1 AND c.client_id = $2 AND a.status = 'active';

-- name: CreateAuthorizationCode :one
INSERT INTO oauth_authorization_codes (
    entity_id,
    client_id,
    user_id,
    code_hash,
    redirect_uri,
    scopes,
    code_challenge,
    code_challenge_method,
    nonce,
    expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, 'S256', $8, $9
)
RETURNING id, entity_id, client_id, user_id, code_hash, redirect_uri, scopes, code_challenge, code_challenge_method, nonce, used_at, expires_at, created_at;

-- name: GetAuthorizationCode :one
SELECT id, entity_id, client_id, user_id, code_hash, redirect_uri, scopes, code_challenge, code_challenge_method, nonce, used_at, expires_at, created_at
FROM oauth_authorization_codes
WHERE entity_id = $1 AND code_hash = $2;

-- name: GetAuthorizationCodeByHash :one
SELECT id, entity_id, client_id, user_id, code_hash, redirect_uri, scopes, code_challenge, code_challenge_method, nonce, used_at, expires_at, created_at
FROM oauth_authorization_codes code
WHERE code.code_hash = $1
  AND NOT EXISTS (
      SELECT 1
      FROM oauth_authorization_codes duplicate
      WHERE duplicate.code_hash = code.code_hash
        AND duplicate.entity_id <> code.entity_id
  );

-- name: MarkAuthorizationCodeUsed :one
UPDATE oauth_authorization_codes
SET used_at = now()
WHERE entity_id = $1
  AND code_hash = $2
  AND used_at IS NULL
  AND expires_at > now()
RETURNING id, entity_id, client_id, user_id, code_hash, redirect_uri, scopes, code_challenge, code_challenge_method, nonce, used_at, expires_at, created_at;

-- name: CreateOAuthToken :one
INSERT INTO oauth_tokens (entity_id, user_id, client_id, token_type, token_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, entity_id, user_id, client_id, token_type, token_hash, scopes, revoked_at, expires_at, created_at;

-- name: FinalizeAuthorizationCodeExchange :one
WITH consumed AS (
    UPDATE oauth_authorization_codes AS code
    SET used_at = now()
    WHERE code.entity_id = sqlc.arg('entity_id')
      AND code.code_hash = sqlc.arg('code_hash')
      AND code.used_at IS NULL
      AND code.expires_at > now()
      AND EXISTS (
          SELECT 1
          FROM oidc_clients client
          JOIN applications application
            ON application.entity_id = client.entity_id
           AND application.id = client.application_id
          WHERE client.entity_id = code.entity_id
            AND client.client_id = code.client_id
            AND client.status = 'active'
            AND application.status = 'active'
            AND (
                (
                    NOT client.secret_required
                    AND NOT sqlc.arg('client_secret_provided')::boolean
                )
                OR (
                    sqlc.arg('client_secret_provided')::boolean
                    AND client.client_secret_hash = sqlc.arg('client_secret_verifier')
                )
            )
      )
    RETURNING code.id, code.entity_id, code.client_id, code.user_id, code.code_hash, code.redirect_uri, code.scopes, code.code_challenge, code.code_challenge_method, code.nonce, code.used_at, code.expires_at, code.created_at
), access_token AS (
    INSERT INTO oauth_tokens (entity_id, user_id, client_id, token_type, token_hash, scopes, expires_at)
    SELECT entity_id, user_id, client_id, 'access', sqlc.arg('access_token_hash'), scopes, sqlc.arg('access_token_expires_at')
    FROM consumed
    RETURNING id
), id_token AS (
    INSERT INTO oauth_tokens (entity_id, user_id, client_id, token_type, token_hash, scopes, expires_at)
    SELECT entity_id, user_id, client_id, 'id', sqlc.arg('id_token_hash'), scopes, sqlc.arg('id_token_expires_at')
    FROM consumed
    RETURNING id
)
SELECT consumed.id, consumed.entity_id, consumed.client_id, consumed.user_id, consumed.code_hash, consumed.redirect_uri, consumed.scopes, consumed.code_challenge, consumed.code_challenge_method, consumed.nonce, consumed.used_at, consumed.expires_at, consumed.created_at
FROM consumed
JOIN access_token ON true
JOIN id_token ON true;

-- name: DeleteExpiredAuthorizationCodes :execrows
DELETE FROM oauth_authorization_codes
WHERE expires_at < now();

-- name: DeleteExpiredOAuthTokens :execrows
DELETE FROM oauth_tokens
WHERE expires_at < now();
