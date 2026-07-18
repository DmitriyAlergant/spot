# Site-Specific Maintainers

**Status:** Complete

## Implementation Checklist

- [x] Extend `_access.json` parsing, cloning, caching, and fail-closed staging
  policies with `maintainers`.
- [x] Add lifecycle and policy-transition schema, migrations, registry methods,
  and role-bearing management authorization.
- [x] Apply lifecycle and maintainer authorization to deploy, delete,
  Cloudflare, site traffic, capabilities, and background mutations.
- [x] Add the manageable-sites API, SDK method, `/spots` role/recovery UI, and
  maintainer policy editor.
- [x] Add unit, integration, migration, concurrency, and regression coverage.
- [x] Regenerate checked-in assets and pass all required validation and review.

## Overview

Spot currently assigns each site to the identity that completes its first
deploy. Later deploys, site deletion, Cloudflare publication management, and
owner-restricted platform capabilities are available only to that owner or a
configured platform admin.

That model does not fit a team project deployed by CI. The CI runner becomes
the permanent owner, while the people responsible for the project cannot
manage the site without receiving platform-wide admin access.

Add a `maintainers` field to the site's existing `_access.json` policy:

```json
{
  "allow": ["all-employees"],
  "maintainers": [
    "alice@example.com",
    "team-estimation"
  ],
  "ai": "owners",
  "slack": "owners"
}
```

The checked-in policy remains the single source of truth. Maintainers receive
complete site-management authority and may change the maintainer list through
later authorized deploys. The original owner remains an immutable recovery
principal, and platform admins retain their global override.

## Goals

- Let a site owner delegate management to exact emails or identity-provider
  groups without granting platform-wide admin access.
- Keep delegation version-controlled in `_access.json` alongside the site's
  existing access and capability policy.
- Give maintainers the same active-site management capabilities as the owner.
- Let maintainers add, remove, or replace maintainers, including removing
  themselves.
- Preserve the original owner and platform admins as recovery paths.
- Prevent a maintainer from turning delegated deletion into ownership transfer
  by deleting and reclaiming the site name.
- Prevent an unauthorized deploy from granting its own actor access.
- Preserve the previous management policy when a deploy fails before its
  policy change commits, and fail closed when an object-store result is
  ambiguous.
- Make sites visible and manageable from `/spots` for every authorized
  manager, not only the original owner.
- Keep existing sites and policies backward-compatible.

## Non-Goals

- Ownership transfer or removal of the original owner.
- Multiple maintainer roles or fine-grained permissions such as deploy-only
  versus delete-only.
- Moving policy state into SQLite or adding a separate policy-management API.
- Hiding `_access.json` from authenticated Spot users. Spot is a trusted
  internal environment, and transparent policy files are acceptable there.
- Publishing `_access.json` to Cloudflare Pages. Cloudflare snapshots continue
  to exclude it.
- Making management authorization imply visitor access. `allow` and
  `maintainers` remain separate capabilities.

Maintainer deletion has one deliberate distinction from owner deletion: it
purges the active site but preserves the owner's name claim. This is recovery
behavior, not a second maintainer role.

## Policy Semantics

`maintainers` is an optional array of strings. It uses the same identity
matching convention as `allow`:

- An entry containing `@` matches an identity email.
- Any other entry matches an identity-provider group.
- Email and group matching is case-insensitive.
- Whitespace around entries is ignored.
- Empty entries do not match.
- An omitted or empty `maintainers` array grants no additional management
  authority.
- A non-array value, `null`, or a case-variant duplicate field is invalid and
  causes deploy validation to fail.

The policy parser continues to reject unknown fields and malformed JSON. The
new slice must be cloned anywhere `AccessPolicy` is cloned so cached policies
cannot share mutable backing arrays.

`maintainers` does not change normal site visibility. A maintainer who is not
matched by `allow` may manage the site from the Spot apex but may not visit a
restricted site. Owner-restricted AI and Slack calls continue to require normal
visitor access first; once that gate is satisfied, a maintainer counts as a
site manager for the existing `"owners"` capability mode.

The policy file remains ordinary internal site content and remains present in
source ZIP downloads. It continues to be omitted from Cloudflare Pages
snapshots.

## Management Authorization

Every management operation must use one shared decision that returns the site
lifecycle state as well as the actor's role:

```text
CanManage(site, actor) =
    actor is the original owner
    OR actor is a platform admin
    OR the stored policy's maintainers match actor
```

The decision returns both an allow/deny result and the authorization role:

- `owner`
- `admin`
- `maintainer`

Role precedence is owner, then admin, then maintainer. A user who is both the
owner and a platform admin is reported as the owner. A user who is both an
admin and a policy maintainer is reported as an admin.

This decision replaces direct owner/admin checks for:

- deploy and redeploy;
- site deletion;
- Cloudflare status, publish, update, resolution, and unpublish operations;
- owner-restricted AI and Slack capability checks after normal visitor access;
- manageable-site discovery for `/spots`.

The original owner recorded in the `sites` table remains immutable. A
maintainer may empty or remove the `maintainers` field, but cannot remove or
transfer the owner. A missing policy grants only owner/admin management.
Maintainer matching applies only while the site record is active; provisional
first-deploy claims and deleted owner tombstones never grant policy-derived
management. A role match alone is not enough to authorize an operation: every
handler must also enforce the operation's allowed lifecycle state.

The operation matrix is:

| Operation | `active` | `provisioning` | `deleted` |
| --- | --- | --- | --- |
| Normal site-host traffic | Apply visitor policy | Deny | Deny |
| Deploy | Owner, admin, or maintainer | Owner/admin recovery only | Owner/admin recreate only |
| Delete | Owner, admin, or maintainer | Deny | Owner/admin release only |
| Cloudflare management | Owner, admin, or maintainer | Deny | Deny |
| Manageable listing | All matching roles | Exclude | Owner/admin recovery only |

A name with no registry row retains first-deployer ownership behavior. The
shared authorization API should therefore return a role-bearing decision plus
state, or accept an explicit operation and enforce this matrix centrally; a
bare reusable boolean such as `CanManageSite` is insufficient.

If a stored policy is malformed or unreadable, maintainer authorization fails
closed. Owners and platform admins bypass that policy lookup and can repair the
site. Policy I/O failures should be distinguishable from an ordinary mismatch:
the former returns an authorization-service error, while the latter returns
`403 Forbidden`.

## Component Boundaries

Management authorization should be centralized rather than reimplemented in
individual handlers. The authorization component composes:

- `SiteRegistry` ownership and platform-admin checks;
- the existing stored-policy resolver and cache;
- the actor identity supplied by the mesh or trusted forward-auth provider.

The component should expose a role-bearing management decision, not only a
boolean. `SiteRegistry` remains responsible for first-deploy claims, content
generation state, registry persistence, and audit writes. Raw registry
mutation methods must not become directly reachable from HTTP handlers in a
way that bypasses the composite authorization decision.

Deploy authorization has an additional transaction requirement: a new site
must still be claimed atomically, and an existing authorized deploy must still
advance its content generation. The implementation may wrap the registry in a
coordinating authorizer or inject a narrow stored-maintainer resolver into the
registry. It must not accept an unverified client-provided owner or maintainer
identity.

One storage-agnostic policy resolver remains the source for both local and
S3-backed storage. It owns a bounded, short-TTL cache of parsed policies,
negative lookups, parse/I/O errors, and the digest of the exact stored bytes.
Instantiate it in every storage mode; the current local-only `PolicyStore`
arrangement is insufficient. Deploy and delete set or invalidate entries only
at the matching storage transition, and concurrent cold reads for one site are
coalesced. Policy summaries and management authorization share the same cache
entry instead of opening `_access.json` twice.

No maintainer copy or lookup table is added to SQLite. The registry stores only
temporary policy-transition hashes used to fence an ambiguous object-store
write; these are integrity metadata, not another representation of the policy.
The `sites` table also gains an explicit lifecycle state so provisional claims
and owner-preserving deletion tombstones cannot be mistaken for active sites.

The lifecycle states are:

- `provisioning`: a new or owner-recreated site whose deploy has not completed;
- `active`: a successfully deployed site whose stored maintainers may grant
  management;
- `deleted`: content and dependent site data were purged by a maintainer, but
  the immutable owner claim remains.

Existing rows migrate to `active`. A successful create or recreate changes
`provisioning` to `active` through the generation-checked deploy completion
write. A storage failure during create removes its provisional row, while a
storage failure during recreation restores `deleted`, when the generation
still matches. A completion failure after storage has finished follows the
stricter recovery behavior defined below. New claims must explicitly insert
`provisioning`; the database default is `active` only so additive migration
treats existing rows correctly. Registry reads validate the state as this
three-value application enum.

Normal traffic to a site hostname requires an `active` registry row before
visitor policy evaluation. Put this check in the shared site-access boundary
used by static serving, Caddy forward auth, document APIs, uploads/downloads,
realtime, and capability gates. Requests for missing, `provisioning`, or
`deleted` sites fail closed (normally as `404 Not Found`), so a tombstoned
hostname cannot accept new data or expose leftover storage.

## Deploy Authorization and Policy Transitions

An incoming `_access.json` is parsed before storage mutation so malformed
policies still fail with `400 Bad Request`. It must never authorize the request
that carries it. Existing-site authorization always evaluates the currently
stored policy.

Under the per-site mutation lock, an existing-site deploy proceeds as follows:

1. Resolve the actor.
2. Read the site record and determine owner/admin status.
3. If necessary, read the currently stored `_access.json` and evaluate
   `maintainers`.
4. Deny the request unless the actor is an owner, admin, or current
   maintainer.
5. Advance the registry's content generation.
6. Synchronize ordinary site files and metadata using the existing failure
   handling.
7. Commit the `_access.json` write or removal at the policy transition point.
8. Resolve and clear the durable policy-transition fence, then update or
   invalidate the policy cache from the verified storage result.
9. Record the successful deploy and its authorization role.

Any change to `maintainers`, including adding, removing, reordering, or
removing the entire policy file, is authorization-sensitive. For updates, the
final maintainer change is deferred until other required file operations have
succeeded. A final policy write that is verified not to have committed leaves
the previous maintainers authoritative so they can retry; an ambiguous result
uses the durable fence described below.

Deferring the whole file is not sufficient when an update narrows visitor
access. New sensitive content must not be served under the old broader visitor
policy. Before changing ordinary files, Spot writes a fail-closed staging
policy whenever the incoming policy may narrow `allow`, downloads, AI, or
Slack. This rule is independent of whether `maintainers` changes. The staging
policy:

- denies all visitors with `allow: []`;
- disables source downloads;
- keeps AI and Slack in owner mode;
- copies only the currently effective maintainer list.

Its serialized JSON must contain an explicit `"allow": []` and
`"download": false`; generic `omitempty` serialization must not drop the empty
allow array and accidentally make the site public. After the storage write is
verified, the policy cache is set to the same staging policy before ordinary
content mutation begins. Spot then synchronously disconnects existing document
and room WebSocket sessions for the site. Their authorization is otherwise
fixed at handshake time, so merely changing the cache would let an already
connected visitor continue to read or publish after revocation. The staging
policy prevents new sessions while disconnect runs.

After ordinary file and metadata operations succeed, Spot replaces the
staging policy with the complete incoming policy as the final authorization
commit. If an intermediate operation fails, the site may remain temporarily
unavailable to visitors, but it never exposes new content or new maintainer
authority. The previous maintainers and immutable owner/admin recovery paths
remain able to retry. Updates that only broaden visitor access keep the old
policy until the final write. An update that neither narrows visitor access nor
changes maintainers may continue to use the existing policy ordering.

Spot's deploy is not a general multi-object transaction. If the final policy
write fails, ordinary content may already have changed, consistent with the
existing partial-storage failure model. Local storage errors normally identify
the outcome, but an S3 `PutObject` or removal error can be ambiguous: the server
may receive a timeout after the object store committed the mutation. Spot must
not return failure while assuming that the previous maintainers remain stored.

Before every authorization-sensitive policy write or removal, persist a
generation-scoped transition fence on the site row containing the digest of
the previous bytes and the intended next bytes; use an explicit sentinel for
an absent object. After an error, synchronously read the object back:

- if it matches the intended digest, treat the storage operation as committed
  and continue;
- if it matches the previous digest, clear the fence, retain the previous
  cached policy, and return the storage error;
- if it matches neither value or cannot be read, retain the fence, cache a
  fail-closed error, and return a service error.

The same reconciliation runs after a process restart or before owner/admin
repair. While a fence is unresolved, normal site-host traffic and
maintainer-derived management are denied; only the immutable owner or a
platform admin may repair the policy. Clearing the fence uses the reserved
content generation so a stale request cannot clear a newer transition. Thus an
ambiguous response never activates unintended maintainers, even after cache
expiry or restart. A verified failure retains the previous policy; an
unresolved failure deliberately narrows recovery to owner/admin.

Policy changes that affect visitor access must continue to obey the existing
fail-closed ordering. In particular, a newly created restricted site may need
to store `_access.json` before sensitive content. This does not activate its
maintainers prematurely: policy-derived management requires an `active` site
record, while the first deploy remains `provisioning`. A failed create removes
the claim, and the generation-checked successful deploy completion changes the
record to `active`. This protects management consumers that do not take the
site mutation lock, including AI/Slack capability checks and manageable-site
listing.

Policy preservation is valid only for an update to an existing `active` site.
A create or owner/admin recreate ignores `preserve_access`, removes and verifies
the removal of any stale `_access.json`, then uses only the policy supplied by
the new deploy. Activation cannot proceed while that cleanup or its transition
fence is unresolved. This prevents a failed deletion or permanent release from
granting former maintainers authority on a later claim.

Activation is not delegated to the current best-effort audit wrapper. After
the required storage and final policy operations succeed, create/recreate calls
a synchronous registry completion method with the reserved content generation.
It changes exactly one matching `provisioning` row to `active`, clears the dirty
marker, and requires no unresolved policy fence; zero affected rows is an
error. Only then may the handler return success. The audit event is written
separately and may retain today's best-effort logging behavior because audit
persistence no longer controls lifecycle state.

If synchronous activation fails, the request returns a server error and the
site remains fail-closed. A successful cancellation restores `deleted` for a
recreate; a completed create whose activation cannot be confirmed retains its
owner's `provisioning` claim rather than freeing a fully written name for
takeover. Owner/admin deploy recovery is allowed for such a row. Storage
failures earlier in a create/recreate keep the existing cancel behavior:
remove a new claim or restore the tombstone when the generation still matches.

## Deletion and Owner-Claim Recovery

An active-site delete is role-sensitive:

- An owner delete preserves today's behavior: after dependent resources and
  content are purged, the registry row is removed and the name becomes free.
- A platform admin acting on another owner's site has the same global override
  behavior as today and may remove the row.
- A maintainer must explicitly unpublish an existing Cloudflare publication
  before deleting, matching today's delete precondition. The delete then
  purges deployed files, uploads, documents, and the stored `_access.json`,
  clears presentation and content metadata, and changes the site row to
  `deleted` instead of removing it.

Deletion uses fail-closed ordering for every role. Under the site mutation
lock, Spot first authorizes the active-site delete and verifies that no
Cloudflare publication exists. It then stores and caches the same explicit
deny-all staging policy used by restrictive deploys, retaining the current
maintainer list for retry. After the staging policy is verified, it disconnects
all existing document and room WebSocket sessions for the site. Only after the
policy is effective and those sessions are closed may it purge ordinary site
files (leaving `_access.json` until last), uploads, and documents.
It then commits the role-specific registry deletion or tombstone transition
and finally removes `_access.json` and invalidates its cache entry.

If purge or registry mutation fails, the active site remains deny-all and its
previous managers can retry. If final policy cleanup fails after the registry
commit, the missing/non-active lifecycle row still blocks all site-host
traffic. Keep the cached staging policy; an owner/admin can retry cleanup for a
tombstone, while a later first deploy of a released name must purge and ignore
the stale policy before activation. No failure path may remove the access
policy while an active row and unpurged site data remain.

Because policy-derived management is disabled for a deleted row, its former
maintainers cannot recreate or release it even if failed cleanup left a stale
`_access.json`. The original owner or a platform admin may redeploy the known
name, which moves the tombstone through `provisioning` and back to `active`
without changing ownership. The owner/admin may instead issue delete against
the tombstone to release the name permanently. This prevents a maintainer from
implementing ownership transfer as delete followed by first deploy.

Deleted tombstones are excluded from gallery, public-site, and statistics
queries. Manageable-site results may expose them only to their owner or a
platform admin, with recovery actions instead of active-site controls. Direct
requests to normal Cloudflare, delete, or other active-site handlers must
reject a tombstone even when `CanManage` identifies the caller as owner/admin;
only the explicit recreate and permanent-release paths accept `deleted`.

## Concurrent Operations

All mutating management operations must reauthorize while holding the per-site
mutation lock. A preliminary authorization check may reject obvious failures
early, but it cannot replace the locked check. Read-only policy consumers such
as manageable-site discovery and AI/Slack capability checks do not acquire the
mutation lock; they rely on the lifecycle gate and the currently stored,
successfully cached policy.

This is especially important for Cloudflare operations. A request may wait
behind a long-running publish while another manager changes `_access.json`.
Before the waiting request touches current publication state, it must acquire
the site lock and re-evaluate the current stored policy.

The existing lock order remains:

1. Cloudflare mutation lock, when applicable.
2. Site mutation lock for authorization, snapshot, or reservation work.

Deploy and delete require only the site mutation lock. The authorization
change must not invert this order or hold the site lock during remote
Cloudflare operations beyond the existing snapshot/reservation boundary.

Out-of-process and background writers must enforce lifecycle too. In
particular, `BeginExternalContentMutation` must atomically require `active`, and
gallery backfill and auto-tag metadata writes must re-check that state at their
locked or leased commit point. They may not recreate files or metadata after a
delete has moved the row to `deleted`.

## Manageable Sites API and UI

Keep `GET /api/sites/mine` and `spot.sites.mine()` ownership-only for backward
compatibility. Add:

```http
GET /api/sites/manageable
```

and:

```js
const sites = await spot.sites.manageable();
```

The new endpoint returns sites for which the caller is owner, platform admin,
or maintainer. It is registered behind the same `sameOriginOnly` middleware,
apex-host check, identity requirement, and database rate limit as
`/api/sites/mine`. Each active result contains the existing owned-site summary
plus enough attribution for the UI:

```json
{
  "name": "estimation-builder",
  "owner": "gitlab-runner",
  "management_role": "maintainer",
  "state": "active"
}
```

The `/spots` page switches to the manageable endpoint and exposes the same
deploy, delete, and Cloudflare controls for every role on active sites.
Maintainer-managed cards can display a quiet label such as
`Maintainer · owned by gitlab-runner`.

For the initial implementation, add a registry query that returns all active
sites with the same deploy-size and content-hash summary used by
`SitesOwnedBy`; `AllSites` alone is insufficient for the existing `/spots`
cards. Filter those candidates through the shared authorization component.
Owner/admin matches bypass policy reads. For the remaining candidates, use the
storage-agnostic cache described above and bounded concurrency on cold reads.
A candidate policy I/O or parse error is logged and that candidate is omitted;
it must not fail the entire list because one unrelated broken site would make
`/spots` unavailable to every non-admin user. Direct operations on that site
continue to return the authorization-service error. A database policy index is
deferred until observed platform scale requires it.

`/api/sites/mine` remains ownership-only and active-only. The manageable
endpoint additionally returns `deleted` tombstones to their owner or a
platform admin; it never returns `provisioning` rows. A tombstone response has
`state: "deleted"`, its immutable owner, and zeroed content summary fields.
The `/spots` card offers the owner/admin two recovery actions: redeploy to the
same name, or permanently release the name. It does not show visitor,
download, or Cloudflare controls for a tombstone.

The browser deployer's access editor adds a separate Maintainers picker backed
by the existing identity/group suggestion endpoint. Its parser and serializer
must round-trip `allow`, `maintainers`, `ai`, `slack`, and `download`; changing
one picker must not silently discard fields the current UI does not edit. It
must emit `_access.json` when maintainers or any other policy setting exists,
even if `allow` is omitted or empty. CLI deploys need no new flags: a checked-in
`_access.json` is the configuration interface. `--preserve-access` preserves
all policy fields together only for an existing `active` site. Create and
owner/admin recreate ignore that flag, remove any stale `_access.json`, and use
only the policy supplied by the new deploy. This prevents a failed deletion or
permanent release from granting former maintainers authority on a later claim.

## Auditing

Add an `authorized_as` text column to `site_deploy_audit`. Values written by
new events are `owner`, `admin`, or `maintainer`; historical rows retain the
empty default. Denied events may leave it empty. Successful recreation may use
the existing `create` deploy action or a new `recreate` action. Lifecycle
activation is a separate synchronous registry completion write, not a side
effect of inserting this audit row.

Deploy and delete events record the role returned by the shared authorization
decision. Cloudflare operations currently use their publication state and
server logs rather than `site_deploy_audit`; this design does not introduce a
new general-purpose audit subsystem for them.

The deploy audit's existing content hash intentionally excludes
`_access.json`, because private mesh-policy changes must not make an otherwise
unchanged Cloudflare publication stale. The temporary transition digests are
not audit history and are cleared once the storage outcome is resolved. This
design does not store full historical maintainer lists in SQLite. Repository
history remains the intended source for reviewing declarative policy changes.

The schema change is additive:

- add `sites.state text NOT NULL DEFAULT 'active'`,
  `sites.policy_transition_generation integer NOT NULL DEFAULT 0`,
  `sites.policy_previous_hash text NOT NULL DEFAULT ''`,
  `sites.policy_next_hash text NOT NULL DEFAULT ''`, and
  `site_deploy_audit.authorized_as text NOT NULL DEFAULT ''` to
  `server/schema.sql` for new databases; generation zero and empty hashes mean
  no transition is pending;
- add idempotent `ensureColumn` migrations in `server/db.go` for existing
  databases, so every pre-feature site starts active and historical audit rows
  retain an empty role, with no policy transition pending;
- update all registry scans and queries to carry or explicitly filter the
  lifecycle state;
- preserve empty values for old audit rows.

No migration is required for existing `_access.json` files.

## Error Behavior

- Invalid incoming `maintainers`: `400 Bad Request`, naming `_access.json` and
  the invalid field.
- Existing site not found: `404 Not Found`.
- Identified actor that matches no management rule: `403 Forbidden`, with an
  error referring to owner, maintainer, or platform-admin access.
- Stored policy I/O or parse failure for a non-owner/non-admin actor: fail
  closed with a service error rather than treating the actor as a definite
  non-maintainer.
- Stored policy failure for an owner/admin actor: allow management so the
  policy can be repaired.
- Failed storage mutation before a staging-policy write: retain the previous
  effective policy and record the deploy failure through the existing audit
  path.
- Failed storage mutation after a staging-policy write: leave visitors denied
  and retain the prior maintainer list so an existing manager can retry.
- Ambiguous policy write/removal: retain the durable transition fence and deny
  site-host and maintainer-derived access until read-back or owner/admin repair
  resolves it.
- Missing, `provisioning`, or `deleted` site-host request: fail closed, normally
  as `404 Not Found`, before reading or mutating site-scoped data.
- Active-only management operation against `provisioning` or `deleted`:
  reject regardless of owner/admin role; only the lifecycle recovery actions
  in the operation matrix are allowed.
- Create/recreate activation failure: return a server error and retain a
  non-active owner claim or restore the prior tombstone; never report success
  based only on a logged audit error.
- Deploy to a `deleted` tombstone by its former maintainer or an unrelated
  actor: `403 Forbidden`; owner/admin recreation is allowed.
- Delete a `deleted` tombstone by its owner/admin: release the name; all other
  actors receive `403 Forbidden`.

## Testing Strategy

### Policy parsing and matching

- Parse email and group maintainers.
- Match emails and groups case-insensitively.
- Cover omitted, empty, `null`, non-array, empty-entry, and case-variant
  duplicate values.
- Confirm unknown fields remain rejected.
- Confirm policy clones and cache entries do not alias the maintainer slice.
- Confirm `_access.json` preservation retains maintainers.
- Confirm staging-policy serialization preserves explicit `allow: []` and
  `download: false` instead of applying `omitempty` defaults.

### Authorization matrix

Exercise owner, platform admin, email maintainer, group maintainer, and
unrelated actor across:

- create, deploy, and redeploy;
- site deletion;
- every Cloudflare management route;
- owner-restricted AI and Slack after normal visitor authorization;
- manageable-site discovery.

Tests must prove that an unrelated actor cannot authorize itself with an
incoming policy, a successful first deploy activates its maintainers, a
maintainer can delegate and revoke access, a self-removing maintainer loses
access on the next operation, and owner/admin repair remains possible when the
stored policy is broken. They must also prove that `provisioning` and `deleted`
rows never activate stored maintainer authority for deploy, AI/Slack, or
listing decisions, and that every active-only handler rejects those states for
owners/admins as well.

### Failure injection and concurrency

- Fail ordinary file writes/removals before the policy transition and verify
  old maintainers remain effective.
- Combine a maintainer change with narrower visitor access and verify the
  deny-all staging policy is stored before content mutation. Inject a later
  failure and verify visitors stay denied while old maintainers retain repair
  access.
- Fail an `_access.json` write/removal and verify the policy cache does not
  expose the attempted list.
- Simulate S3 success followed by a client-visible timeout for policy put and
  removal. Verify read-back resolves exact previous/next digests, an unknown or
  unreadable result stays durably fenced across restart, and no incoming
  maintainer is activated by a reported failure.
- Narrow visitor access without changing maintainers and verify staging still
  precedes every ordinary file mutation.
- Inject failures throughout deletion and verify no remaining content,
  document, upload, realtime, or capability path becomes public; verify old
  managers retain retry access until the lifecycle transition commits.
- Keep document and room WebSockets open while visitor access narrows or delete
  begins; verify staging blocks new handshakes and existing sessions are closed
  before content mutation or purge.
- Verify a successful policy change is visible immediately without waiting for
  cache expiry.
- Hold a Cloudflare request behind its operation lock, revoke the actor through
  a deploy, and verify the locked reauthorization prevents the waiting action.
- Cover both allowed and denied paths with local storage and the S3-compatible
  policy resolver where behavior differs.

### API, UI, migration, and regression coverage

- Verify `/api/sites/mine` remains ownership-only.
- Verify `/api/sites/manageable` is same-origin and apex-only and returns the
  correct full site summaries, owners, roles, and lifecycle states.
- Verify manageable listing uses the shared cache in local and S3 modes, does
  not fetch a candidate policy twice, and logs/omits one broken candidate
  without hiding healthy owner/admin or maintainer results.
- Verify `/spots` exposes full active-site controls to maintainers, identifies
  their role, and renders owner/admin recovery actions for tombstones.
- Verify the SDK adds `spot.sites.manageable()` and generated assets remain in
  sync.
- Verify maintainer deletion requires prior Cloudflare unpublish, purges site
  data, leaves an owner tombstone, and prevents the former maintainer from
  claiming the name. Verify owner recreation preserves ownership and
  owner/admin tombstone deletion releases the name.
- Verify successful create/recreate is the activation point and failed
  create/recreate removes, retains, or restores the correct lifecycle state.
- Verify `--preserve-access` works for active updates but is ignored for create
  and recreate, including when stale storage contains former maintainers.
- Fail or stale the synchronous generation-checked activation and verify the
  deploy does not return success, the site stays unavailable, and owner/admin
  recovery remains possible. Separately fail audit insertion and verify it
  does not roll back an already confirmed activation.
- Verify the browser editor round-trips every existing policy field and emits
  a maintainer-only policy.
- Verify audit rows persist `authorized_as` and old databases receive both
  additive columns with active sites and empty historical roles.
- Retain the existing owner/admin, access-policy, deploy-failure, and
  Cloudflare lock-order tests as regressions.
- Verify gallery backfill, external content leases, and delayed auto-tag writes
  cannot mutate `provisioning` or `deleted` sites.

Run `just test`, `just test-integration`, `just check-generate`, and the
relevant SDK smoke/e2e coverage before completion.

## Alternatives Considered

### Materialize maintainers in SQLite

Copy the list from `_access.json` into a `site_maintainers` table after each
successful deploy. This would make manageable-site queries cheaper but creates
two policy representations that can diverge during partial failures or manual
storage changes. It is unnecessary at Spot's current scale.

### Manage maintainers only through an API or UI

Store delegation separately from deployed content. This gives transactional
database updates but removes repository-based review and makes CI-managed
projects depend on additional out-of-band configuration.

### Use a `publish` field

`publish` is ambiguous because Spot already uses that term for Cloudflare
Pages publication. `maintainers` describes the people and groups receiving the
complete management capability without conflating internal deploys with
external publication.

## Acceptance Criteria

- A CI-owned site can declare a team email or group in `_access.json` and every
  matching actor can fully manage the site without becoming a platform admin.
- A maintainer can deploy a new policy that adds or removes maintainers.
- No request can use its incoming policy to authorize itself.
- Failed deploys do not prematurely grant maintainer authority or expose new
  content under an older, broader visitor policy; verified failures retain the
  existing maintainer repair path and ambiguous policy outcomes fail closed to
  owner/admin recovery.
- Missing, provisioning, and deleted sites cannot serve site-host traffic or
  accept new site-scoped data, and active-only management routes reject them.
- A create/recreate is not reported successful until its generation-checked
  activation is confirmed independently of audit logging.
- The immutable owner and platform admins can always repair delegation.
- A maintainer can delete an active site after unpublishing it but cannot use
  delete-and-reclaim to become its owner; the owner/admin can recreate or
  release the preserved name claim.
- Maintainers appear in `/spots` with full management controls and an explicit
  role, while one unrelated broken policy cannot fail the whole listing.
- Existing sites without `maintainers` behave exactly as before.
- `_access.json` remains available inside trusted Spot and excluded from
  Cloudflare Pages.
