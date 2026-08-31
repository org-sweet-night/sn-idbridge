# IdBridge Feishu identity and TROBS reference source contract

- Contract ID: `idbridge-source-feishu-trobs-v1`
- Version: `1.0.0`
- Status: active
- Owner: SN IdBridge
- Scope: Feishu directory identity/lifecycle ingestion and opaque TROBS person references

## Boundaries and provenance

Feishu is an identity source. Full sync reads tenant-scoped departments from
`contact/v3/departments/0/children` and users from
`contact/v3/users/find_by_department` with an app-derived tenant access token.
Incremental sync accepts authenticated/deduplicated Feishu events, then reads
the referenced user or department from Feishu before applying it. IdBridge's
`identity_sources` entity/source pair is the provenance boundary; records from
different entities or sources never merge merely because a display field
matches.

TROBS is not an identity or authorization source. A source adapter may place a
bounded string `trobs_user_id` under
`directory_users.raw_profile.external_references`. It is an opaque person-join
reference only. TROBS responsibilities, store ownership, titles, groups, and
permissions must not be ingested as IdBridge roles. In particular, neither
`isa:pam:requester` nor `isa:pam:approver` may be inferred from TROBS data.

## Bounded canonical fields

Department records map only canonical `department_id`/`open_department_id`,
parent department ID, and name into IdBridge columns. User records map
`user_id` (falling back to `open_id`, then `union_id`), union/open IDs, name,
English name, employee number, job title, email, mobile, avatar URL, and
lifecycle status. Provider payloads may be retained in source-owned
`raw_profile` for traceability, but directory APIs expose only canonical fields
and the bounded string-only `external_references` projection (maximum 32 keys,
64-character keys, 256-character values). Nested or oversized references are
discarded from the API projection.

Feishu app secrets, tenant access tokens, Authorization headers, IdBridge
credentials, OIDC client secrets, webhook verification secrets, and TROBS
credentials are never canonical fields, raw-profile fields, audit payloads, or
log attributes. Plaintext OIDC client secrets exist only in runtime secret
configuration or their one-time create/rotate response; IdBridge persists only
a one-way verifier.

## Lifecycle semantics

Feishu activated maps to directory/managed `active`. Frozen or resigned maps
to directory `disabled` and managed `disabled`. Unrecognized/absent status maps
to directory `unknown` and managed `locked`. A confirmed incremental delete
marks the exact provider object deleted and can archive its managed user when
no active binding remains; this path is not subject to full-snapshot shrink
heuristics.

A full sync is authoritative only after every page is fetched and validated.
It is applied in one database transaction. Missing users are marked deleted
and eligible managed users archived only after the destructive snapshot guard
passes. Provider-returned explicit deleted users retain their exact identity
and lifecycle treatment.

## Pagination, watermark, and replay

Both department pagination and each per-department user pagination maintain a
seen-token set, reject missing tokens and any token cycle (including A→B→A),
and stop at an explicit default budget of 1,000 pages per listing. Exceeding
the budget fails the sync before identity reconciliation. Page size is 50.

Feishu list APIs provide no stable cross-resource snapshot watermark in this
integration. Therefore IdBridge does not claim point-in-time consistency
across the department and user listings. The sync job ULID/trace ID and start
time are processing provenance, not an upstream watermark. Incremental event
IDs are deduplicated and replay-safe; object state is reread from Feishu before
mutation.

## Destructive snapshot protection

Full sync fails closed before its mutation transaction when an existing source
has a zero-row user or department snapshot. It also rejects any shrink when a
source has fewer than 20 current rows, and a shrink of at least 50 percent for
larger populations. There is no environment-variable or request-body bypass. Legitimate
offboarding should arrive as confirmed incremental deletes; an exceptional
large reorganization requires reviewed source evidence and a code/config
change with tests, never an ad-hoc runtime flag.

On rejection, current directory rows, bindings, managed users, roles, and
departments remain unchanged. The failed sync job and redacted failure audit
remain as operational evidence.

## Authorization separation

IdBridge `user_roles` is the only role authority exposed by directory APIs.
The uncached exact-subject endpoint reads current active lifecycle, binding,
directory status, and role assignments directly. `GET
/api/directory/users/{subject_id}` returns a top-level opaque `version` in
`sha256:<lowercase-hex>` form. The version is derived from the active managed
lifecycle, selected source/directory-user binding, directory status, and sorted
current IdBridge role codes. Display fields and volatile sync timestamps are
excluded, so an equivalent full sync does not create membership drift; any
authorization-relevant state change does. External references are returned only
as join metadata and have no role or permission semantics.

The authorization-relevant response shape is:

```json
{
  "id": "<managed IdBridge OIDC subject ULID>",
  "kind": "user",
  "source_id": "<selected identity source ULID>",
  "roles": ["isa:pam:approver", "isa:pam:requester"],
  "version": "sha256:<64 lowercase hex characters>",
  "status": "active",
  "has_children": false,
  "updated_at": "<display/sync timestamp; not a membership revision>"
}
```

Other canonical profile fields may be present, but IAM authorization must bind
to `id`, `roles`, and `version` within the token's entity boundary.
