# operator-s3

> [!NOTE]
> This repository is vibe coded with [Claude](https://claude.com/product/claude-code).

A Kubernetes operator that provisions S3 buckets and per-bucket access keys
on a [RustFS](https://github.com/rustfs/rustfs) object store, in the API group
`s3.craftainer.dev`. It is an instance of
[`template-k8s-operator`](https://github.com/craftainer/template-k8s-operator)
(and so of `template-docker` and `template-base`) and follows the CRD
conventions of
[`lib-operator-go`](https://github.com/craftainer-labs/lib-operator-go).
See [`docs/TEMPLATE.md`](docs/TEMPLATE.md) for the conventions shared by every
instance.

Status: the `Bucket` and `BucketAccess` controllers are in place for RustFS
stores, tested against an in-memory store and an HTTPS test server, and the
RustFS client against a real RustFS with TLS in the integration lane (see
"Testing"). An AWS store serves `Bucket` and `BucketAccess` claims (see "AWS
stores").

## API

All kinds are `v1alpha1`. The field tables, defaults and validation are in
`lib-operator-go`'s
[`docs/crd-drafts.md`](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/crd-drafts.md)
and
[ADR 0002](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md).

| Kind | Scope | What it is |
| --- | --- | --- |
| `ClusterObjectStore` | cluster | A RustFS endpoint and its admin credentials, usable from the namespaces in `allowedNamespaces` |
| `ObjectStore` | namespaced | The same for one namespace |
| `Bucket` | namespaced | A bucket on a store; `deletionPolicy` defaults to the operator's `DEFAULT_DATA_DELETION_POLICY` (`Retain` when unset) |
| `BucketAccess` | namespaced | An access key on a `Bucket`, written to a binding Secret of type `servicebinding.io/s3` |

The admission webhooks check what the schema cannot: an `endpoint` that is a
well-formed `https` URL, a `proxyURL` with a `http`, `https` or `socks5`
scheme, and a `Bucket` quota of at least one byte. Defaults and every other
rule are in the CRD schema (OpenAPI and CEL).

### Provider and credentials

`ObjectStore` and `ClusterObjectStore` accept an optional `provider` (type
`AWS`, with an `aws` block) and `credentials` next to the self-hosted fields:

- A store has either `adminCredentialsSecretRef` and `endpoint` (self-hosted
  RustFS) or a `provider`. They exclude each other, and `credentials` is
  allowed only with a `provider`.
- A namespaced `ObjectStore` with a `provider` needs `credentials` with source
  `Secret` in its own namespace; it may not use the operator's own identity.
- With a `provider`, `region`, `pathStyle` and `backend` are forbidden:
  the provider names its region.
- `region`, `pathStyle` and `backend` no longer have schema defaults; the
  controller applies them (`us-east-1`, `true`, `RustFS`), so a self-hosted
  store behaves as before.

An AWS store is described under "AWS stores" below. `provider.aws.endpointOverride`
exists on `ClusterObjectStore` only: the type of a namespaced store's `aws` block
leaves it out by design, so the API server drops it from an `ObjectStore`
instead of rejecting it.

## Controllers

Both controllers run `lifecycle.Reconcile` of
[`lib-operator-go`](https://github.com/craftainer-labs/lib-operator-go), so
finalizers, `deletionPolicy`, `managementPolicies`, drift polling, the
`craftainer.dev/rotate`, `poll-interval` and `paused` annotations and the
`Ready`, `Synced` and `ServerReachable` conditions work as ADR 0002 says.

- **Bucket** creates the bucket (name derived from namespace and name, kept in
  `status.externalName`), a tag `craftainer.dev/claim` that marks it as the
  claim's, versioning (object lock implies it), lifecycle rules and the hard
  quota. Object lock is set only when the bucket is created. With
  `deletionPolicy: Delete` the bucket is deleted with the claim, if it is empty:
  the operator never deletes objects. Only a bucket the claim created is
  deleted; see "Deleting a claim".
- **BucketAccess** creates an IAM user with one policy for the bucket (and
  `prefix`), and one service account per credential generation. The binding
  Secret (type `servicebinding.io/s3`) holds the service account's keys.
  Rotation issues the next service account first and deletes the previous one
  after `gracePeriod`. A change of `access` or `prefix` updates the policy and
  the Secret's fields but keeps the key in use, and a key in use is never reset
  in place. If the binding Secret does not hold it (missing, or holding another
  key), the update applies the policy and reports "the binding Secret does not
  hold the active key"; the next reconcile then rotates to a new key with a
  grace period. A key that is gone from the store is issued again. The operator
  cannot ask the store for a secret key, so a hand-edited `secretAccessKey` that
  keeps the right `accessKeyId` is written back as it is; annotate the claim with
  `craftainer.dev/rotate` (a new value) to force a rotation and repair it. The access waits (and is reconciled again
  when the `Bucket` changes) until its `Bucket` is `Ready` and not being
  deleted. Deleting it removes the service accounts it derived (never another
  account under the user's name), the user, and the policies it named, even
  when the user is already gone.

Each store's RustFS client is kept between reconciles (connections reused, a
bounded number idle), keyed by the store and rebuilt when the store, its
credentials, its CA bundle or its proxy change; it is dropped when the store is
deleted or unused for ten minutes.

The admin API is RustFS's `/rustfs/admin/v3`, signed with SigV4; MinIO is not
supported. A store's `endpoint` is an `https` URL without a path.

### Settings

| Setting | Flag | Default | Meaning |
| --- | --- | --- | --- |
| `EGRESS_MODE` | `--egress-mode` | `strict` | `strict` refuses connections to loopback, link-local, cloud metadata and private ranges; `allow-private` refuses all but the private ranges, for stores on private addresses |
| `DEFAULT_DATA_DELETION_POLICY` | `--default-data-deletion-policy` | `Retain` | the `deletionPolicy` a new `Bucket` is pinned to when its spec leaves it unset |
| `POD_NAMESPACE` | `--operator-namespace` | the pod's namespace | where a `ClusterObjectStore`'s admin Secret and CA bundle are read from; startup fails when neither this nor the pod's service account names a namespace |

In the Helm chart the first two are the values `egressMode` and
`defaultDataDeletionPolicy` (defaults `strict` and `Retain`; `config/manager`
has the same defaults). Set `egressMode: allow-private` when the store, or a
proxy in front of it, is on a private address, which is the usual case for a
RustFS Service inside the cluster: with `strict` the operator cannot reach it.

Credentials in a URL (`https://user:pw@host`) are rejected in `endpoint` and
`proxyURL`; credentials come only from Secret references. Conditions and Events
carry fixed texts only (for example "authentication failed"); the details are in
the operator log, but only as far as they are safe: the log names the operation
and the failure category or S3 error code, at debug level
(`-zap-log-level=debug`), and never a host, key, secret or response body.

### A claim that stays `NameCollision`

The operator never adopts or deletes what it did not create.

- `Bucket`: `Synced=False`, reason `NameCollision`, with "bucket exists and is
  not managed by this operator" means a bucket of the derived name exists
  without the tag `craftainer.dev/claim`. Another message means the tag names
  another claim. Either the bucket is someone else's (rename the `Bucket`, or
  remove the bucket yourself if it is stale), or the operator stopped between
  creating it and tagging it: put the tag
  `craftainer.dev/claim=<namespace>/<name>/<uid>` on the bucket and the next
  reconcile takes it over.
- `BucketAccess`: the same reason means the IAM user of the derived name exists
  and the claim never recorded creating it. IAM users cannot carry a marker, so
  delete the stale user (and its policy and service accounts) with the RustFS
  admin tools, or rename the claim.

Deleting a claim in this state leaves the foreign object alone and releases the
finalizer (see "Deleting a claim" below).

### Deleting a claim

- A claim that never issued a create on the store (`status.externalId` is
  empty) is released without contacting the store, which may be gone: nothing
  of it is there. A claim that is stuck as `NameCollision` is such a claim.
- A claim that did issue one deletes only what is its own. A bucket whose
  marker names another claim, or that belongs to another account, is skipped and the finalizer is released;
  it is never deleted or changed, whatever the `deletionPolicy`. Its own
  bucket follows the `deletionPolicy` as before (`Retain` leaves it, `Delete`
  removes it when empty), and a store that cannot be reached still keeps the
  finalizer for a claim that did issue a create.
- "Issued a create" is `status.externalId`, written before the first create
  request (a `Bucket` writes `create-issued`, a `BucketAccess` the key of the
  active service account). A claim from before the record existed has none:
  deleting it releases the claim and leaves the object, which is the safe
  reading when the status does not say it is ours. A reconcile gives a `Bucket`
  whose bucket carries its marker the record, so only a claim deleted before
  its next reconcile is affected.

### Server health

`ClusterObjectStore` and `ObjectStore` get the `ServerStatus` their CRDs promise
from one `probe.Reconciler` of `lib-operator-go` (`pkg/probe`) per kind, wired
in `internal/app`:

- **What is probed:** `GET /rustfs/admin/v3/info` (the server info, which needs
  the admin credentials), signed like every other admin call. It answers for
  both things the status promises: that the store is reachable over verified
  TLS, and that it accepts the admin credentials. RustFS reports a version in
  that answer, so `status.serverVersion` is the version of the first server in
  it that names one (RustFS reports `<commit date>@<tag, short commit or
  package version>`; the part after the `@` is kept, at most 64 bytes). A store
  that answers but names no version is still `Ready` and reachable, with an
  empty `serverVersion`: reachability and accepted credentials are what the
  conditions say.
- `status.lastProbeTime`: the last probe that ran and whose result was recorded
  (a store that cannot be probed, `InvalidServer`, has none). It is rewritten
  when the result changes and otherwise every 10 minutes, so a healthy fleet
  does not write one status per store per minute.
- `Ready` and `ServerReachable` are `True` (reason `Available`) when the probe
  succeeded, and `False` with reason `ServerUnreachable` when it did not
  (unreachable, TLS failure, refused credentials, or a failed call). A store
  whose admin Secret is in a forbidden namespace is
  `AdminSecretNamespaceForbidden`; one whose connection settings are missing or
  invalid (an absent Secret or CA ConfigMap, a bad proxy URL, no `accessKey` and
  `secretKey`) is `Ready=False` with `ServerReachable=Unknown` and reason
  `InvalidServer`. The status never shows why a call failed; the operator log
  has the category (policy, TLS, authentication, timeout, other), once per
  change. Neither the status nor the log holds a key or a credential.
- A probe runs every `spec.healthCheckInterval` (default one minute, plus up to
  10% jitter), whether the last one succeeded or not; a store is also probed at
  once when it is created or its spec changes. A probe is bounded to 30 seconds.
- The probe connects exactly as the claims do: the egress policy, the store's
  TLS settings (its CA bundle, `tls.serverName`, minimum version) and proxy, as
  the admin user. It builds one client for the probe and closes its
  connections, not sharing the claims' client cache.
- RBAC: `get`, `list` and `watch` on Secrets and ConfigMaps, and `get`, `update`
  and `patch` on the stores' `status`, in the chart role.
- Caches: the manager's Secret and ConfigMap caches keep only the objects with
  the `managed-by` label (so the operator does not hold every Secret or ConfigMap
  in memory), and the probe reads the admin Secret and the CA bundle live, not
  from the cache, so every probe sees the current object. A change to an
  unlabelled admin Secret, CA Secret or CA ConfigMap does not trigger a probe at
  once; it is picked up at the next probe interval (one minute by default). Do
  not widen either cache for that: it costs memory and widens exposure for a
  latency gain of at most one interval.

### AWS stores

A store with `provider.type: AWS` serves `Bucket` claims on the S3 buckets of an
AWS account, through the AWS SDK for Go v2. Its region, credentials and role
come from `provider.aws` and `credentials` (WorkloadIdentity, the operator's own
identity, for a `ClusterObjectStore`; a Secret with the keys `accessKeyId` and
`secretAccessKey`, and `sessionToken` if there is one, for either kind). Every
request dials through the egress policy and the store's `proxyURL`, and
`tls.caBundle` is the set of roots the client trusts (`tls.serverName` and
`tls.minVersion` do not apply to the SDK).

- **Buckets** are created in the store's region with the same tag marker,
  versioning, object lock and lifecycle rules as on RustFS. A bucket of the
  derived name that exists in another region (`301`) or that another account
  made while the create runs (`BucketAlreadyExists`) is a `NameCollision`. A
  `403` on the existence check is an authentication failure, not a collision:
  S3 answers it both to refused credentials and to a bucket of another account,
  so the claim reports the store as unreachable. The operator keeps no lifecycle rule it cannot
  write itself (a tag or size filter, no expiration in days): it replaces the
  bucket's lifecycle configuration with the claim's rules.
- **A bucket quota is refused.** S3 has no quota, and the operator never
  compares one it cannot read back, which would update the bucket in a loop. A
  `Bucket` with `spec.quota` on an AWS store is `Synced=False` with reason
  `ReconcileError` and the message "an AWS store has no bucket quota: remove
  spec.quota". Nothing is created or changed on the store until it is removed.
- **`BucketAccess` is one IAM user per claim.** The user is named
  `<namespace>-<name>-<hash>` (at most 64 characters, `HashShort`), created under
  `provider.aws.iamPath` (default `/craftainer/`) with
  `provider.aws.permissionsBoundaryARN` if set, and tagged `craftainer.dev/claim`
  with the claim (`namespace/name/uid`). A user without that exact tag is never
  adopted, changed or deleted: the claim is a `NameCollision`. The access policy
  (the bucket's ARN, the claim's `access` and `prefix`) is an inline policy named
  after the document's digest, so a change attaches the new one before the old
  one is deleted. `status.externalName` is the user, `status.externalId` its ARN
  and `status.rotation.active` the access key id; the binding Secret is the
  usual `s3` binding plus `cloud: aws`. The path and the boundary apply when the
  user is created; changing them later does not move or re-bound an existing
  user. The policy's bucket ARN is in the
  partition of the store's region: `aws-cn` for `cn-` regions, `aws-us-gov` for
  `us-gov-` regions, `aws-iso`, `aws-iso-b`, `aws-iso-e` or `aws-iso-f` for the
  isolated regions, `aws` otherwise.
- **Rotation uses the user's two access keys.** IAM generates the key and shows
  the secret once. A rotation creates the second key, writes it to the binding
  Secret, and after the grace period deletes every key but the new one. Before
  creating a key, the operator deletes the keys that are neither the active
  one nor the one the binding Secret holds: a key whose secret was lost to a
  crash is never a reason to hit IAM's limit of two. A retry after a crash
  between writing the binding Secret and recording the key reuses the key the
  Secret holds, when the Secret is the claim's, a key is active, and the key
  is neither the active nor the previous one and is still on the user; with
  any other Secret nothing is reused. Delete order is keys, policies, user.
- **IAM is eventually consistent.** A user or key created a moment ago may not
  be visible to the next call, and a new key may not work at once. A call that
  finds an entity missing right after the operator made it, or a change in
  flight, makes the claim `Ready=False` and is retried shortly, never waited
  for. A consumer should retry on `InvalidClientTokenId` for a few seconds after
  a new key appears.
- **Health.** The probe calls STS `GetCallerIdentity` and S3 `ListBuckets`
  (the identity needs `s3:ListAllMyBuckets`), then writes
  `status.discovered.region` (the provider's region), `status.discovered.accountId`
  (from `GetCallerIdentity`) and `status.discovered.endpoint` (the override, or
  `https://s3.<region>.amazonaws.com`, with `.cn` for the China partition; scheme
  and host only, so an override's user name, password or path never reach the
  status). The identity is not read into the status, and `serverVersion` stays
  empty: S3 has none. The identity of the operator needs the IAM actions
  `CreateUser`, `GetUser`, `DeleteUser`, `PutUserPolicy`, `DeleteUserPolicy`,
  `ListUserPolicies`, `CreateAccessKey`, `ListAccessKeys`, `DeleteAccessKey` and
  `TagUser` (with a permissions boundary, also `iam:PermissionsBoundary`).
- **Known gap (a library limit, `lib-operator-go` v0.6.0).** An `endpointOverride`
  that the operator does not allow (it needs `EGRESS_MODE=allow-private`) and a
  target that is missing show as `Ready=False` with reason `ServerUnreachable`
  and a fixed text, not as `EndpointOverrideForbidden` or `TargetNotFound`. The
  operator log has the reason. A later library release (NKlib-3) carries a typed
  reason, and the operator will report it then.
- **What CI cannot check.** The integration lane runs the AWS store against
  moto server (see `test/integration`). moto does not enforce IAM and does not
  validate a web identity token, so IAM eventual consistency, a role's or a permissions boundary's actual limits,
  whether a key can really use the bucket and the workload identity credentials
  of a real cluster are not verified by any test here.

### Known limits

- On an AWS store, `provider.aws.iamPath` and `permissionsBoundaryARN` apply
  only when the IAM user is created; changing them later does not move or
  re-bound an existing user.
- An IAM user that carries an inline policy the operator did not name (a
  foreign policy) keeps the claim from being up to date: the operator only
  removes policies it named, so the claim stays not-up-to-date until that
  policy is removed by hand.
- `provider.aws.endpointOverride` is used for the S3 calls, but only its scheme
  and host reach the binding Secret's `endpoint` and `status.discovered.endpoint`:
  the override's path is dropped.
- Policy document drift is not detected, only which policy is attached.
- RustFS removes every service account of a user together with the user. The
  operator derives the service accounts it deletes by name, but it cannot spare
  an account somebody else added under the user's name once the claim is
  deleted.
- RustFS can answer a quota write with `503` until it has confirmed the quota
  capability across the cluster, which a node that has just started does a
  little later. The `Bucket` reconcile fails and retries until it is ready.
- A bucket created by somebody else between the operator's read and its create
  is refused as a name collision: RustFS would answer the create of an existing
  bucket with success for the admin, so the client checks first (a `403` on
  that check, with the admin credentials otherwise accepted, is a collision
  too, and so is the same refusal when the `Bucket` is read; a probe that fails
  is reported instead of a collision, and a bucket of your own whose HEAD is
  denied, by a bucket policy for example, is reported as a collision too). The window
  between that check and the create is not closed by RustFS: a create by the
  same admin identity inside it is adopted. The operator therefore needs
  dedicated admin credentials that no other creator shares; a create by a
  different account gets `BucketAlreadyExists`.

## Binding Secret

A `BucketAccess` writes its access key to the Secret named by
`spec.binding.secretName` (the claim's name when empty), in the claim's
namespace. The Secret has the type `servicebinding.io/s3`, a `type` key (`s3`)
and a `provider` key (`craftainer.dev`), as the Service Binding specification
asks, and these data keys:

| Key | Meaning |
| --- | --- |
| `endpoint` | The store's `spec.endpoint` (the URL of its S3 API). |
| `region` | The store's `spec.region`, or `us-east-1` when unset. |
| `bucket` | The name of the bucket on the store (the one the `Bucket` reports in `status.externalName`, not the claim's name). The Secret is written only once that `Bucket` is `Ready`, not being deleted and on the same store. |
| `accessKeyId` | The access key of the service account. It changes on each credential rotation, so read it together with `secretAccessKey`. During the grace period of a rotation the previous key still works in the store, while the Secret already holds the new pair. |
| `secretAccessKey` | The secret key of that service account. |
| `prefix` | The `spec.prefix` the access is limited to, or empty for the whole bucket. |
| `pathStyle` | `true` or `false`: whether the client should use path-style addressing. It is the store's `spec.pathStyle`, and `true` when unset. |
| `sslrootcert` | The PEM CA bundle that the store's `spec.tls.caBundle` refers to, for the client to pin. The key is present and empty when the store has no bundle. |

A binding Secret that is missing or does not hold the active key is replaced through a rotation (see "Controllers" and "Backup and restore"), so a consumer must read the Secret again, not cache it.

## Install

Helm and OLM v1 need cert-manager for the webhooks' serving certificate; OLM
v0 issues its own, so it needs none. The released chart, bundle and catalog are
on GHCR, signed and attested (`release-extras.yml`, see `docs/TEMPLATE.md`).

### Helm

```sh
helm install operator-s3 oci://ghcr.io/craftainer-labs/charts/operator-s3 \
  --version X.Y.Z --namespace operator-s3-system --create-namespace \
  --set egressMode=allow-private
```

A released chart pins the manager image by digest. Values worth knowing (the
schema, `charts/chart/values.schema.json`, rejects a misspelt enum or digest):

| Value | Default | Meaning |
| --- | --- | --- |
| `egressMode` | `strict` | `allow-private` is the usual choice when stores live on cluster or private addresses, such as a RustFS Service |
| `defaultDataDeletionPolicy` | `Retain` | the `deletionPolicy` a new `Bucket` is pinned to when it leaves it unset |
| `global.imageRegistry` | empty | pull the image from a mirror (air-gapped, EB-5) |
| `manager.image.digest` | set when released | image by digest; wins over `tag` |
| `manager.extraEnv` | `[]` | extra environment, rendered after the chart's own (not `POD_NAMESPACE`, `EGRESS_MODE` or `DEFAULT_DATA_DELETION_POLICY`: the chart sets them, and the schema rejects them here), e.g. `NO_PROXY` or `HTTPS_PROXY` for the Kubernetes API client, or `SSL_CERT_FILE` with a bundle mounted through `manager.extraVolumes` and `manager.extraVolumeMounts` |
| `manager.podSecurityContext`, `manager.securityContext` | restricted | non-root, seccomp `RuntimeDefault`, no privilege escalation, all capabilities dropped, read-only root filesystem (EB-10); the e2e installs into a `restricted` namespace as an arbitrary UID |
| `crd.keep` | `true` | keep the CRDs, and with them every claim, when the release is uninstalled |

The operator makes no outbound call except to the Kubernetes API and to the
RustFS stores named by the store kinds, and it sends no telemetry. A store's
proxy and CA bundle belong to the store object (`spec.proxyURL`,
`spec.tls.caBundle`), not to the chart.

The CSV therefore declares `features.operators.openshift.io/proxy-aware: "false"`:
the operator reads no `HTTP(S)_PROXY` (the RustFS client takes its proxy from the
store's `spec.proxyURL`), and the in-cluster Kubernetes API client needs no
proxy. `disconnected` is `"true"` and the fips, tls-profiles and token-auth
features are `"false"`.

### OLM

`make bundle` writes the bundle from `config/manifests` (`make bundle-validate`
checks it offline; `bundle/` is generated and not committed). It supports
`AllNamespaces` only, pins the manager image by digest and lists it in
`spec.relatedImages`. The released bundle and a single-bundle catalog are
`ghcr.io/craftainer-labs/operator-s3-bundle` and `...-catalog`; add the catalog
as a `CatalogSource` (OLM v0) or a `ClusterCatalog` (OLM v1) and subscribe or
create a `ClusterExtension`.

## Backup and restore

The operator holds no data of its own: everything it knows is in Kubernetes
objects and in RustFS.

| Kept in | What | Back it up with |
| --- | --- | --- |
| Kubernetes | the four kinds and their status, binding Secrets (`servicebinding.io/s3`), the admin Secrets and CA bundles that stores reference | your cluster backup (Velero, GitOps, etcd). Restore the stores and admin Secrets first, then the claims |
| RustFS | the buckets and their objects, versioning, lifecycle rules, quotas and object lock, the ownership markers (the bucket tag `craftainer.dev/claim`), and the IAM users, their policies and service accounts (the access keys) | RustFS's own tooling: replicate or copy the buckets (for example `mc mirror` or bucket replication) and keep a snapshot of the data volumes; the IAM state lives in the same store |

The operator never copies or restores data. What it does on a restore:

- Restoring `Bucket` claims into a cluster whose RustFS still has the buckets:
  a claim whose bucket carries its marker
  (`craftainer.dev/claim=<namespace>/<name>/<uid>`) is re-adopted. A restored
  claim has a new UID, so its old marker names another claim: it reports
  `NameCollision` and nothing is changed. Hand the bucket over by putting the
  tag `craftainer.dev/claim=<namespace>/<name>/<uid>` on it, with the restored
  claim's UID; the next reconcile takes it over. A bucket without the tag
  reports `NameCollision` the same way.
- Restoring a `BucketAccess`: an IAM user carries no marker, so one that exists
  while the claim never recorded creating it reports `NameCollision`. Delete the
  stale user (and its policy and service accounts) with RustFS's admin tools and
  the claim creates it again, or rename the claim. If your backup restores
  `status`, the claim keeps its record (`status.externalId`) and its rotation
  state and carries on.
- Binding Secrets are rewritten by the operator, so a restore of them is not
  required. The operator cannot read a secret key back from RustFS: a binding
  Secret that is missing or does not hold the active key is replaced by a new
  key issued through a rotation with a grace period, so consumers read the new
  Secret.
- Restoring RustFS from a snapshot brings its users and keys back as they were
  at the snapshot; keys issued or rotated since are gone, and the next reconcile
  issues what the claim needs again.

`deletionPolicy` (on `Bucket` only; a `BucketAccess` always removes the user,
policies and service accounts it derived, and never the bucket) decides what
deleting a claim does to the data:

- `Retain` (the default; `DEFAULT_DATA_DELETION_POLICY` sets it for a claim that
  leaves it unset, pinned in `status.effectiveDeletionPolicy`; a `spec.deletionPolicy`
  set later wins at deletion): the bucket and its objects stay on the store, the
  finalizer is released. A later claim of the
  same name needs the marker handed over (above).
- `Delete`: the bucket is deleted if it is empty. The operator never deletes
  objects, so a bucket that still holds objects (or versions or delete markers)
  keeps the finalizer and the claim reports the failure until you empty it.
  Take a backup first; there is no undo.
- Either way, a bucket whose marker names another claim is skipped and the
  finalizer released, and a claim that never issued a create touches nothing
  (see "Deleting a claim").
- Uninstalling the chart with `crd.keep=false`, or deleting a CRD, deletes every
  claim of that kind and applies each claim's policy.

Limit: a `BucketAccess` has no marker to heal from. A claim from before
`status.externalId` existed is treated as never having created its user, so
deleting it leaves the user in the store; remove it with RustFS's admin tools.
RustFS removes a user's service accounts together with the user, so any account
somebody else added under the user's name goes with it.

## Quick start

Open the repository in its devcontainer (the Go and Kubernetes tools are
installed there), then:

```sh
make test          # unit + envtest, race detector, coverage gate
make test-e2e      # kind cluster + Chainsaw (needs ~2 GB free memory and Docker)
make helm-lint     # lint the chart, check it matches config/
make bundle-validate   # OLM bundle (operator-sdk) and catalog (opm)
make operator-help # every operator target
prek run --all-files --hook-stage manual   # all checks
```

The unit and envtest suites run against an in-memory store and an HTTPS test
server and hold the 100% coverage gate. The RustFS client is also run against a
real RustFS 1.0.0-rc.5 with TLS (verified connections, random admin
credentials) by the integration tests in
[`test/integration`](test/integration/README.md) (build tag `integration`):
the `Integration` workflow in CI, the `rustfs` service of the devcontainer
locally.

```sh
go test -tags integration -count=1 ./test/integration/...
```

Releases go through `release.yml` as in `template-docker`; right after each
one, dispatch `release-extras.yml` to publish the chart, bundle and catalog
and to sign and attest them:

```sh
gh workflow run release-extras.yml -f tag=vX.Y.Z
```

It never publishes over an existing tag. If a run failed part-way (say the
chart and bundle were pushed and signing failed), finish it with
`-f resume=true`: artifacts whose tag exists are not pushed again, their
existing digests are signed (not again if they already verify) but never
attested, and missing ones are published and attested. SLSA provenance must
describe the run that built the artifact, so a resumed run does not claim it:
an existing artifact without provenance is named in a warning, and if
provenance is required for it, cut a new patch version instead.

## License

Licensed under the [GNU General Public License v3.0](LICENSE).
