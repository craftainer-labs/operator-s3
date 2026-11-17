# 0005. Azure Blob backend (proposed)

## Status

Proposed, awaiting G-api. Nothing here is implemented, and no code, CRD or
chart changes with this ADR. It becomes Accepted when the implementation
(NKs3-3) merges, after the user has decided the points in "Decisions for the
user".

## Context

`lib-operator-go` ADR 0002 section 16 adds an optional `provider` to the store
kinds. It approves, for Azure: the `provider.azure` block (`tenantID`,
`subscriptionID` and `resourceGroup` required for s3, plus `endpointOverride`
on cluster kinds), `credentials.source` `WorkloadIdentity` or `Secret` with the
Azure Secret keys (`clientId`, `clientSecret`), the binding-key conventions
with `cloud: azure`, the ownership-marker rules, the provider transition rule
(`has(self.provider) == has(oldSelf.provider)`), and, for s3, the target field
`storageAccount` (3 to 24 lowercase letters or digits). Section 16.7 leaves
out, as "not drafted", `externalName`, `externalId` and the binding keys for
`Bucket` on Azure and for `BucketAccess` on Azure (SAS or Entra RBAC), and
says the operator's ADR 0005 specifies them before implementing and sends any
change to section 16 back to G-api. This ADR is that specification, in the
form [ADR 0004's GCP addendum](0004-cloud-backends-aws-store.md) used: a
proposal with the options and a lean for each open point.

Azure Blob Storage differs from S3 and GCS in ways that decide the design:

- **The API is not S3.** A consumer must speak the Blob REST API (or an Azure
  SDK). There is no S3-interoperability endpoint as on GCS, so a consumer of an
  Azure binding cannot be the S3 client of an AWS binding.
- **A bucket is a container inside a storage account.** The account is the
  billing, networking and security parent; names are unique only within an
  account. Account-level settings (blob public access, versioning, lifecycle
  management policy, soft delete) live on the account, not the container, and
  are reached through the Azure Resource Manager (ARM, control plane), not the
  Blob service (data plane).
- **Container names:** 3 to 63 characters; letters (lowercase only), digits and
  hyphens; start and end with a letter or digit; no consecutive hyphens.
- **Container metadata keys** must start with a letter or underscore and
  continue with letters, digits or underscores, in ASCII; values are ASCII.
  Keys are case-insensitive when set or read. `craftainer.dev/claim` cannot be
  a key (as with GCS labels).
- **Delete Container deletes the blobs in it.** Unlike S3 and GCS, there is no
  "bucket not empty" refusal.
- **Authorization** is Entra (RBAC) on the data plane, account keys, or SAS.
  A role assignment can take up to 10 minutes to take effect.
- **Public access** is two settings: the account's `allowBlobPublicAccess` and
  the container's public access level (`private` by default). The account
  setting overrides the container.

Sources: [naming and referencing containers and metadata][naming],
[anonymous access][anon], [assign a data role][rbac], [user delegation
SAS][uds] ("Stored access policies aren't supported for a user delegation SAS";
the key is valid for up to seven days), [delegating role assignments with
conditions][abac] (the built-in `Role Based Access Control Administrator` role
can be constrained to chosen roles and principal types), [Azurite][azurite].

## Decision

The decisions below are the proposal. Each point that is public API or a
trade-off is numbered `D1`...`D17` and listed again, with options, in
"Decisions for the user"; a point that the platform forces is marked
**forced**.

### 1. A bucket is a container in an existing account (D1)

A `Bucket` on an Azure store is a blob container in the storage account the
store names with `provider.azure.storageAccount`. **Under the lean for D1 the
operator never creates or changes a storage account**: the account, its network rules, encryption,
versioning, lifecycle policy and public-access setting belong to the platform
admin, the model ElastiCache user groups already follow. The blob endpoint is
`https://<storageAccount>.blob.core.windows.net`. `endpointOverride` (cluster
stores only, section 16.3) is test-only, as decided there: it is honoured only
with `EGRESS_MODE=allow-private`, and in `strict` mode a store that sets it is
`Ready=False` with reason `EndpointOverrideForbidden`. Here it points the
store at Azurite; sovereign clouds are out of `v1alpha1` (section 16.8,
decision 9). The container's ARM resource ID, used as `externalId` and as
the RBAC scope, is built from the provider block alone (`subscriptionID`,
`resourceGroup`, `storageAccount`, container name), with no ARM call.

The container name is derived as for the other backends (`naming.Tenant`,
`-`, `HashShort`, 63 characters), then sanitised because Azure allows only
lowercase letters, digits and hyphens (**forced**; D14). A Kubernetes object
name may contain `.`, which is mapped to `-`; doubled hyphens are collapsed;
the first and last characters are made alphanumeric; the 63-character limit is
applied after sanitising. The hash keeps the name unique (it is computed from
the unsanitised identity, so `a.b` and `a-b` differ) and satisfies the
three-character minimum. The exact rule is verified in NKs3-3, as it is for the
ElastiCache user id.

### 2. Ownership is container metadata, set in the create request (D3)

The marker is the metadata key `craftainer_claim` (**forced** by the key rules)
with the value `<namespace>/<name>/<uid>`, plain as on AWS, because ASCII
slashes are allowed in values (a GCS-style digest is not needed). It is sent
in the same `Create Container` request as the access level (`Access` unset,
which is `private`), so there is never an unmarked container to adopt, as on
GCS. Rules, as in ADR 0004:

- Only a container whose `craftainer_claim` equals this claim is read beyond
  its metadata, changed or deleted. A container with another or no marker is a
  `NameCollision` and is never adopted, not even when status recorded the
  create (the marker is written atomically, so an unmarked container is never
  the claim's).
- `409 ContainerAlreadyExists` on create, losing a race, is a `NameCollision`.
  `409 ContainerBeingDeleted` (a container of that name was deleted within
  roughly the last 30 seconds) is a `lifecycle.PendingError`, never a
  collision, so a claim deleted and recreated promptly settles.
- `404` on a read is absent. `403` is an authentication, authorization or
  network-rule failure, not a collision. The account is fixed by the store, so
  a `403` is never a name clash (unlike S3 and GCS), but it cannot tell a
  refused identity from an account firewall or private-endpoint rule
  (`AuthorizationFailure` covers both). The operator reports it without
  choosing between them, and the README says so.

### 3. Hardening (D4)

- **Container:** the operator creates it with public access `private` and
  reads `x-ms-blob-public-access` back on every reconcile; a claim's own
  container that drifted to `blob` or `container` is set private again. (A
  container's access level is data-plane readable and writable.)
- **Account:** `allowBlobPublicAccess` is an ARM property. The data plane does
  not return it, so with the Blob client alone the operator cannot check it.
  When it is `true`, a drifted container could still be re-opened by someone
  else; when `false`, the account overrides every container. The operator
  documents it as an admin prerequisite (the README and the NFR say: set
  `allowBlobPublicAccess=false`, which is also the secure default for new
  accounts). Reading it needs `armstorage` and the `Reader` role on the
  account; see D4.
- No object ACLs exist on Azure, so nothing corresponds to S3 ACL disabling.

### 4. Data plane against control plane (D2, D11)

| Call | Plane | Library | In go.mod |
| --- | --- | --- | --- |
| Create, get properties, set metadata, set access level, delete container; list blobs (emptiness check); list containers (probe) | data | `azblob` (`service`, `container`) | yes, v1.8.3 (direct, pinned in `clouddeps`) |
| Read `allowBlobPublicAccess`, versioning, soft delete; set versioning; management (lifecycle) policy; create a container with immutability | control | `armstorage` | **no** |
| Create and delete a role assignment (BucketAccess, option A) | control | `armauthorization/v3` | **no** |
| Get a user delegation key (option B) | data | `azblob` `service.GetUserDelegationCredential` | yes |
| Revoke user delegation keys (option B, whole account) | control | `armstorage` | **no** |

`Bucket` can be built on `azblob` alone, and that is the lean (D2). The cost:
`Bucket` fields that need the account (`versioning`, `lifecycleRules`,
`objectLock`) cannot be applied, and `quota` does not exist on Azure. Under the
lean the operator refuses them (`Synced=False`, reason `ReconcileError`,
nothing changed), as AWS refuses a quota and GCS a quota and object lock, and
a claim with `versioning: false` and no rules, the defaults, works. The
account-wide management policy is a single document shared by every container
and every tenant (up to 100 rules, rules matched by a `container/prefix`
filter): an operator that wrote `lifecycleRules` would read-modify-write an
admin-owned object, the Memorystore ACL policy problem again, so it is a
follow-up that needs its own decision, not part of v1. Versioning and
immutability would need `armstorage`.

New SDK modules come as a "dependencies alone" item before the code that uses
them, as `clouddeps` already does for `azblob`: `armauthorization/v3` for
option A, `azidentity` promoted from `// indirect` to a direct dependency (the
store uses it), plus `armstorage` only if D2 or D4 choose it.

### 5. The store, the probe and delete (D13, D15)

- **Store** `internal/azblob`, on the Azure SDK, implements the bucket methods
  of `controller.Store`. The HTTP transport comes from `cloud.Egress.Azure`
  (the credential comes from the store's `credentials`, `WorkloadIdentity` or
  `Secret` as section 16.2 defines them, resolved by `cloud.Egress.Azure`; no
  `DefaultAzureCredential` chain), so every
  request dials through the egress policy and the store's proxy. One attempt
  per call: the controller retries.
- **Probe:** `ListContainers` with one result, the smallest read that shows the
  account exists and the identity may use the data plane. It discovers
  `accountId` (the storage account name), `endpoint`, and no `region`: the
  location is an ARM property of the account, so `status.discovered.region`
  stays empty on the lean (D2) and the README says so.
- **Delete:** `Delete Container` removes every blob in it, so the operator
  never calls it on a container that holds anything. It lists one page of
  blobs, including versions and snapshots, and a non-empty container is a
  not-empty error and is left in place, as on S3 and GCS ("never emptied by
  the operator"). The check is racy (an object may arrive between the list and
  the delete; there is no `If-Match` on container delete other than a lease),
  so the residual risk is documented, and a platform admin can enable
  container soft delete on the account, which makes a lost container
  recoverable. See D15.
- **Settling:** a role assignment or a newly created container not yet visible
  is a `lifecycle.PendingError`, never a wait.

### 6. BucketAccess on Azure (D5 to D10)

Three credential models exist for a claim's access to one container. Rotation
is the lens `lib-operator-go` v0.6.0's rotation-ordering research (plan
Decision log, "Rotation-ordering research") adds: any option that mints a
secret has the write-ahead gap, because the lifecycle applies the external
change before the Secret and status are durable.

| | A. Entra RBAC role assignment | B. User delegation SAS | C. Service SAS from the account key |
| --- | --- | --- | --- |
| What the operator creates | one role assignment scoped to the container, for `iam.principal` | nothing in Azure; it signs a token locally | nothing in Azure |
| Secret in the binding | none | the SAS token (a bearer secret) | the SAS token |
| Operator needs | `Microsoft.Authorization/roleAssignments/write` and `delete` at the container scope (D10), `armauthorization`; plus `read` | the `generateUserDelegationKey` action on the account (`Storage Blob Delegator`, or Data Contributor) | the account key (`listKeys`, or a Secret holding it): full account authority |
| Lifetime | until the claim is deleted; propagation up to 10 minutes | at most 7 days (the delegation key limit), so it must be reissued before expiry | any, but a long SAS cannot be revoked except through the key or a stored access policy |
| Revocation | delete the assignment (up to 10 minutes) | revoke the account's user delegation keys, **all** SAS of the account at once, or remove the principal's role; no per-claim revoke, and stored access policies do not apply to a user delegation SAS | rotate the account key (breaks everyone) or a stored access policy (5 per container) |
| Rotation | not applicable (`RotationNotApplicable` Event, `NonRotator`), like the IAM modes in section 16.4 | reissue before expiry: a stateless re-sign, old SAS stay valid until they expire, so the write-ahead gap costs at most an early expiry; consumers must reload the Secret | as B, but a leak outlives the key rotation |
| Prefix scoping | by an ABAC condition on the assignment (D9) | not possible (a SAS scopes to a container or blob, a directory only with a hierarchical namespace) | same as B |
| Consumer must be | an Azure identity (managed or workload identity, a service principal) | anyone holding the token | anyone holding the token |
| Emulator | none (Azurite does not authorize by RBAC) | none for correctness (Azurite supports account and service SAS) | Azurite supports it |
| Microsoft's stance | recommended ("use Entra credentials when possible") | recommended over a key SAS | discouraged |

**Lean (D5): option A for v1**, as the only model with no secret, no rotation,
no 7-day clock and a per-claim, immediate-in-intent revoke, and the one that
matches the `IAM` mode the draft already defines for db and cache. Option B is
the follow-up for consumers outside Azure, as a second `authMode`. Option C is
not offered.

Under A the specification is:

- **API.** `BucketAccess.spec` gains `authMode` (`Password` default, `IAM`;
  immutable) and `iam.principal`, exactly the shape section 16.4 gives
  `DatabaseRole` and `CacheUser`, with the same CEL (`!has(self.iam) ||
  self.authMode == 'IAM'`, no `rotationPolicy` in `IAM` mode). On an Azure
  store `authMode: Password` is refused with `AuthModeUnsupported`: there is
  no shared secret model (B and C are not offered). On AWS, GCP (once NKs3-2b
  lands) and self-hosted stores `IAM` is refused with `AuthModeUnsupported`
  and `iam.principal` is not allowed for AWS (section 16.4). This is a public
  API change to `BucketAccess` and goes to G-api (D6).
- **The principal** is the Entra **object ID** of the service principal or
  managed identity (a GUID), not a name: an Azure role assignment takes an
  object ID, and resolving a name would need Microsoft Graph, another API, a
  second permission set and another egress target for the operator (D6). The
  controller rejects a value that is not a GUID with `IAMPrincipalRequired`
  (existing reason) with a message that says "object ID". The assignment sets
  `principalType: ServicePrincipal`, so a freshly created identity does not
  fail on Entra replication lag.
- **The assignment** is `Storage Blob Data Contributor` for `access:
  ReadWrite` and `Storage Blob Data Reader` for `ReadOnly` (built-in, so the
  role definition GUIDs are global constants; only the resource ID path that
  wraps them is subscription-scoped), scoped to the container's ARM ID.
  Contributor includes delete, as the AWS policy for ReadWrite does; a custom
  role without delete is a later option (D6).
- **Name and marker.** A role assignment name must be a GUID. It is derived
  deterministically from the claim's UID and the container (UUIDv5 over a
  fixed namespace), so a retry or a restart cannot create a second one, and
  `PUT` is idempotent. The marker is the assignment's `description`
  (`craftainer.dev/claim=<namespace>/<name>/<uid>`). Only an assignment with
  that name and that description is read, changed or deleted: another is a
  `NameCollision`, and one the operator did not make is never adopted. Azure
  also refuses a second assignment with the same principal, role and scope
  under a different name (`409 RoleAssignmentExists`). Two claims for the same
  principal, container and access level, or an admin-made identical
  assignment, therefore hit it; under the lean (D17) that is a `NameCollision`
  that names no principal, and no assignment is shared between claims, so
  deleting one claim never removes another's access. The
  scope (a container) is a second barrier: the operator touches no assignment
  at account or higher scope.
- **Order.** Create the container's assignment, read it back, and report
  `Ready` when it exists. The up-to-10-minute propagation cannot be verified
  by the operator (it holds no consumer token); the README says so and the
  condition message does not claim the consumer can already log in. Delete
  order: the assignment, then nothing (the container belongs to the `Bucket`).
- **Operator permission (D10).** The operator's identity holds `Role Based
  Access Control Administrator` on the storage account with an ABAC condition
  that allows it to assign and remove only the two data roles above (to
  service principals), so a compromised operator cannot grant itself or others
  `Owner`. The admin sets this up; the README documents the assignment and its
  condition, and the verification is manual because Azurite does not model
  RBAC. The operator's own data-plane rights are a separate matter (D16).
- **Prefix (D9).** `spec.prefix` is refused on Azure in v1
  (`Synced=False`, `ReconcileError`, nothing changed) rather than silently
  granting the whole container. An ABAC condition on the assignment can limit
  the blob path (`Microsoft.Storage/storageAccounts/blobServices/containers/blobs:path`
  with `StringStartsWith`, and the matching `prefix` request attribute for
  list), and is the follow-up once it can be tried against a real account.

### 7. Binding keys, `externalName`, `externalId` (D7, D8)

The binding keeps the type `servicebinding.io/s3` and the keys `type` and
`provider`, plus `cloud: azure` (section 16.5). Azure is not S3-compatible, so
`cloud` is what tells the consumer to use the Blob API.

`BucketAccess` × Azure, `IAM`:

| Key | Value |
| --- | --- |
| `cloud` | `azure` |
| `authMode` | `IAM` |
| `endpoint` | `https://<storageAccount>.blob.core.windows.net`, or the discovered value |
| `storageAccount` | the account name |
| `bucket` | the container name |
| `prefix` | empty (a prefix is refused, D9) |
| `username` | the principal's object ID (the section 16.5 convention: the consumer's own identity) |

There is no `accessKeyId`, `secretAccessKey`, `pathStyle` or `region`. Nothing
in the Secret is secret, as in section 16.5's `IAM` mode, but it is still a
Secret, so the shape is the same; the principal may appear only here and in
status. The consumer logs in with its own workload identity and no key.

| Kind × cloud | `externalName` | `externalId` |
| --- | --- | --- |
| Bucket × Azure | the container name | the container's ARM resource ID (`/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Storage/storageAccounts/<account>/blobServices/default/containers/<name>`), the analog of `arn:aws:s3:::<bucket>` and `projects/_/buckets/<bucket>`; also the RBAC scope |
| BucketAccess × Azure (`IAM`) | the role assignment name (the derived GUID) | the role assignment's full resource ID (`<container scope>/providers/Microsoft.Authorization/roleAssignments/<guid>`); `rotation` stays empty |

If option B is added later, its `externalName`/`externalId` are empty (the SAS
has no Azure object), and its binding adds `sasToken` and `sasExpiry`; that
would be a G-api change of its own.

### 8. Emulator and what it cannot check (D12)

The integration job runs the store against Azurite (MIT, Microsoft's open
source emulator), pinned by image digest, like moto and fake-gcs-server.
Azurite 3.37.0 (GitHub release of 2026-08-26, MIT licence) is the latest release at the time
of writing; the pin (tag and
digest, with the Renovate comment) is chosen when the job is added, and is
checked against the `mcr.microsoft.com/azure-storage/azurite` tag list then.
It covers the container create/get/set-metadata/list/delete calls and the
public access level. It cannot check, and NFR-0007 (to be written with the
implementation, in the form of NFR-0005 and NFR-0006) states:

- **RBAC and role assignments.** Azurite documents OAuth as `--oauth basic`
  only, over HTTPS with a certificate: it checks the token's issuer, audience
  and expiry, not the signature or the permissions. A role assignment, an ABAC
  condition, the up-to-10-minute propagation and whether a role really limits
  a principal are not checked. The assignment calls get an httptest fake of the
  ARM authorization API, from Microsoft's reference, and the fake's limits are
  listed.
- **User delegation SAS** (only if option B is added): the key request and
  signing are not validated by Azurite.
- **The account's `allowBlobPublicAccess`** and the effect of the account
  setting over a container's level: Azurite has no ARM and no account
  settings. The container level is checked; the account setting is not.
- **Authentication and the `403` cases, account networking and firewalls,
  private endpoints, soft delete, immutability, versioning and lifecycle
  management.**

The store takes the resolved `azcore.TokenCredential` (what
`cloud.Egress.Azure` produces for either credential source); in CI the test passes a static
credential that returns a well-formed unsigned token, which `--oauth basic`
accepts, so the operator needs no test-only code path in production (D12).

## Decisions for the user

Each item is a public API shape, a security default or a genuine trade-off, so
the user decides; the lean is the executor's, labelled as one. Decisions marked
**G-api** change section 16 or a CRD and go back to the API review.

**D1. What a Bucket is.** (a) A container in an existing, admin-provided
storage account (the lean). (b) The operator also creates storage accounts
(a `StorageAccount` kind or a field), which brings account-level SKU,
networking, keys and cost into a tenant-facing API and needs `Contributor` on
the resource group. Lean (a): a tenant must not choose a billing and network
boundary, and (a) is what `storageAccount` in section 16.3 already says. This
is a proposal until the user decides; section 1 states the lean's consequence.

**D2. Bucket implementation scope.** (a) `azblob` only: bare private containers;
`versioning`, `lifecycleRules`, `objectLock` and `quota` refused with
`ReconcileError`; no region discovered (the lean). (b) Add `armstorage` now:
versioning read and verified, container immutability at create, region
discovered, the account's public-access setting read; lifecycle rules still
need the shared management policy (D11 and section 4). (a) ships a smaller,
safer surface without a new dependency and without writing the admin's
account; (b) gives parity with S3 for versioning and object lock. Lean (a),
with (b) as a follow-up.

**D3. Ownership marker (forced shape, one choice).** Metadata key
`craftainer_claim`, value `<namespace>/<name>/<uid>` plain. Options: (a) plain value, readable in the portal and by an admin debugging
a collision, but it exposes namespace, name and UID to anyone who can read
container metadata on the account (the same exposure as AWS tags); (b) a
digest as on GCS, which hides them but needs a lookup to explain a collision.
Lean (a). Adoption of a container without the marker stays refused either
way, as in ADR 0004.

**D4. Account public-access check.** (a) Document `allowBlobPublicAccess=false`
as an admin prerequisite, harden only the container (the lean). (b) Read it
through ARM in the probe and report a store whose account allows public access
as degraded (a new condition message, not a new condition type); needs
`armstorage` and `Reader` on the account. (a) keeps the operator data-plane
only; (b) turns a silent exposure into a visible one. Lean (a) now, (b) with
D2(b) if that is chosen.

**D5. BucketAccess credential model.** (A) Entra RBAC, no secret (the lean).
(B) User delegation SAS, short-lived, 7 days at most, needs rotation and a
reload by the consumer, revocation only per account. (C) Service SAS from the
account key (not recommended). Lean A for v1, B as a later opt-in for
non-Azure consumers; C is not proposed because it hands the operator the
account key (full account authority), and the user may still choose it. Rotation: A is `NonRotator`; B's rotation is a
stateless re-sign, so the write-ahead gap in the plan's rotation-ordering
research costs an early expiry, not an outage, but it still needs the NKlib-4
design to avoid a window with no valid token if a status write is lost.

**D6. New BucketAccess API (G-api).** `spec.authMode` (`Password` | `IAM`,
immutable, default `Password`) and `spec.iam.principal` on `BucketAccess`,
with section 16.4's CEL, extended to s3 (section 16.4 names only db and
cache). This departs from section 16.4 and 16.5 in
four places, each a G-api point: `iam.principal` carries an object ID GUID, not
a principal name, and needs a CEL pattern (16.4 has only `maxLength: 320`);
`username` in the binding is that GUID (16.5 says the name); `authMode` is a
key of an s3 binding (16.5 lists it for db and cache only); and, as
section 16.8 decision 6 decided no allow-list, a tenant can grant its bucket to
any object ID in the tenant (the same exposure decided for db and cache, and
for a data role only on its own container). Sub-choices: (i) the principal is an Entra **object ID** GUID (the
lean; a name would need Graph); (ii) `Password` is refused on Azure with
`AuthModeUnsupported` (the lean) versus defaulting to `IAM` on Azure stores
(the controller cannot see the server in CEL, so a default per cloud is not
expressible); (iii) the roles are the built-in `Storage Blob Data Contributor`
and `Reader` (the lean), versus a custom role without delete, which the
operator would have to create. No new condition type or reason is proposed, but reusing
existing reasons for new meanings is itself a G-api point: the proposal reuses `AuthModeUnsupported`, `IAMPrincipalRequired`, `NameCollision`,
`ReconcileError` and `RotationNotApplicable`.

**D7. Binding shape (G-api).** The keys above, with `type` still `s3`, and no
S3 keys. Alternative: a new binding type `azure-blob`, which would break the
"every cloud binding keeps the section 7 type" rule in section 16.5. Lean:
keep `s3`, with `cloud: azure` as the discriminator.

**D8. Status identifiers (G-api).** Bucket: name and the container's ARM ID.
BucketAccess: the role assignment's name and full ID. Alternatives: the blob
URL for the bucket's `externalId` (reachable, but not an ARM identity and not
the RBAC scope). Lean: the ARM ID.

**D9. Prefix.** (a) Refuse `prefix` on Azure in v1 (the lean). (b) Support it
with an ABAC condition on the role assignment, with the attribute names above.
(b) is correct on paper and unverifiable here (Azurite does no RBAC, so a
mistake would silently over- or under-grant). Lean (a), then (b) after a manual
check against a real account.

**D10. The operator's role-assignment permissions.** (a) `Role Based Access Control
Administrator` on the storage account, with an ABAC condition limiting the
roles and principal types it may assign (the lean). (b) `User Access
Administrator` or `Owner`: can grant itself anything. Needs the admin's setup;
the README documents it and says the condition is not verified in CI.

**D11. Dependencies.** A deps-alone item for `armauthorization/v3` before the
BucketAccess item (the lean), and `armstorage` only if D2(b) or D4(b) is
chosen. Alternative: call the ARM REST API with the egress-bound HTTP client
directly, to avoid the large ARM SDK modules; more code and less type safety,
not recommended.

**D12. Emulator credential.** (a) Azurite with `--oauth basic`, HTTPS and a
static unsigned token credential injected through the store's constructor
(the lean): no production test seam. (b) Azurite with a shared key, needing
the store to accept a key credential in production code (not wanted: an extra
credential type to maintain and to leak).

**D13. Probe and discovered fields.** (a) `ListContainers` with one result,
discovering `accountId` and `endpoint`, no region (the lean; data plane only,
the smallest read that proves the identity may use the account). (b) An ARM
read of the account (`armstorage`), which adds `region` and the public-access
setting but needs `Reader` and the extra dependency (it goes with D2(b) and
D4(b)). (c) A `GetAccountInfo` call: it returns the SKU and account kind but
no location, so it adds little over (a). Lean (a).

**D14. Container name sanitising.** The platform forces lowercase letters,
digits and single hyphens, so some rule is needed; its shape is a choice.
(a) Map `.` to `-`, collapse doubled hyphens, trim non-alphanumeric ends, cut
to 63 after sanitising, keep `HashShort` computed from the unsanitised
identity (the lean: readable, and unique through the hash). (b) Drop `.` and
`-` runs entirely: shorter, less readable. (c) Name the container from the hash
alone (`HashShort` plus a fixed prefix): always valid, but a person cannot
tell whose container it is from the account. Lean (a); the exact rule is
checked in NKs3-3.

**D15. Delete semantics.** (a) Delete the container only when a listing
(blobs, versions, snapshots) is empty, else report not-empty and leave it (the
lean; racy by one request). (b) Never delete containers; the claim's delete
leaves the container (safest, but a tenant cannot clean up and name reuse
breaks). (c) Delete unconditionally, as Azure does by default (data loss on a
mistaken claim delete; not recommended). Lean (a) with the soft-delete advice
in the README.

**D16. The operator's data-plane permissions.** Create, set-metadata,
set-access-level, list and delete on containers, and the probe's list, all
need data-plane or container-write rights on the account, which is separate
from D10's role-assignment right. (a) `Storage Blob Data Owner` or `Data
Contributor` on the account (the lean as the least work; the operator can then
read and write every tenant's blobs, which a compromise exposes). (b) A custom
role with only the container-level data actions (`containers/read`, `write`,
`delete`, and the blob list action) and none of the blob read/write ones;
narrower, but the admin must maintain it and the exact action set is verified
only against a real account. (c) Account keys: not proposed (full authority
and a secret to store). Lean (b) if the user accepts the manual verification,
else (a). The README documents whichever is chosen.

**D17. Duplicate role assignments.** Azure refuses two assignments with the
same principal, role and scope. (a) Report `409 RoleAssignmentExists` as a
`NameCollision` and share nothing (the lean: deleting one claim never removes
another's access, but two claims for the same principal and container cannot
coexist, and an admin's identical assignment blocks the claim). (b) Adopt the
existing assignment and reference-count it across claims: allows sharing, but
needs a marker per claim in status or a Secret and breaks the no-adoption
rule. (c) Refuse a second `BucketAccess` for the same principal and container
earlier, with a CEL or webhook rule. Lean (a).

## Consequences

- One claim model serves RustFS, AWS, GCS and Azure for `Bucket`; Azure
  `BucketAccess` is the first credential-free mode of the s3 operator, so
  `BucketAccess` gains `authMode` and `iam.principal` (a CRD, chart, webhook
  and generated-code change in NKs3-3, after G-api).
- With the lean, Azure `Bucket` is a bare private container: a tenant cannot
  ask for versioning, lifecycle rules, object lock or a quota. The
  `lifecycleRules` of a shared account-wide policy are a separate, later
  decision.
- A consumer of an Azure binding must use the Blob API with an Azure identity,
  and it may wait up to 10 minutes after `Ready` for the role to apply.
- The operator needs a role-assignment permission on the account. Narrowing it
  with an ABAC condition is a manual, admin-side setup that CI cannot verify.
- Nothing about role assignments, public access settings or RBAC is verified
  against Azure until a real subscription is used; a release that changes the
  Azure store is run once by hand against one, like the GCS store.
- A claim's delete never empties a container, but a last-moment write can
  still be lost to a race; container soft delete on the account is the
  mitigation the README recommends.

[naming]: https://learn.microsoft.com/en-us/rest/api/storageservices/naming-and-referencing-containers--blobs--and-metadata
[anon]: https://learn.microsoft.com/en-us/azure/storage/blobs/anonymous-read-access-prevent
[rbac]: https://learn.microsoft.com/en-us/azure/storage/blobs/assign-azure-role-data-access
[uds]: https://learn.microsoft.com/en-us/rest/api/storageservices/create-user-delegation-sas
[azurite]: https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azurite
[abac]: https://learn.microsoft.com/en-us/azure/role-based-access-control/delegate-role-assignments-overview
