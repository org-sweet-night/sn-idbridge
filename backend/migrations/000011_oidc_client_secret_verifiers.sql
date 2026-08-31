-- SPDX-License-Identifier: MIT

-- Client secrets are high-entropy credentials. Persist only deterministic
-- SHA-256 verifiers so token authentication can compare them without retaining
-- or re-exposing the original secret. This migration and the verifier-aware
-- binary are an inseparable promotion: an older binary cannot authenticate the
-- one-way values after this runs.
-- Goose runs this migration once in a transaction. Every non-empty legacy row
-- is hashed deliberately: a plaintext secret could itself look like the new
-- sha256-prefixed format, so pattern-based "already converted" detection would
-- be ambiguous and could both retain plaintext and break client authentication.
-- +goose Up
UPDATE oidc_clients
SET client_secret_hash = 'sha256:' || encode(digest(client_secret_hash, 'sha256'), 'hex')
WHERE client_secret_hash IS NOT NULL
  AND client_secret_hash <> '';

ALTER TABLE oidc_clients
    ADD CONSTRAINT oidc_clients_client_secret_verifier_format
    CHECK (
        client_secret_hash IS NULL
        OR client_secret_hash = ''
        OR client_secret_hash ~ '^sha256:[0-9a-f]{64}$'
    ) NOT VALID;

ALTER TABLE oidc_clients
    VALIDATE CONSTRAINT oidc_clients_client_secret_verifier_format;

-- +goose Down
ALTER TABLE oidc_clients
    DROP CONSTRAINT IF EXISTS oidc_clients_client_secret_verifier_format;

-- The data conversion is deliberately irreversible: a down migration may
-- remove the format constraint, but it cannot and must not recreate plaintext.
