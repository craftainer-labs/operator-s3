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

Status: the `Bucket` and `BucketAccess` controllers are in place, tested against
an in-memory store and an HTTPS test server, and the RustFS client against a
real RustFS with TLS in the integration lane (see "Testing"). The stores do not
report health yet (below).

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
the operator log.

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

### Known limits

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

### Not done yet

- **NOX-probe:** the store kinds do not probe their server: `ServerStatus`
  (`serverVersion`, `lastProbeTime`, the `Ready` and `ServerReachable`
  conditions) stays empty until the probe lands in `lib-operator-go`.

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
  leaves it unset, pinned in `status.effectiveDeletionPolicy`): the bucket and
  its objects stay on the store, the finalizer is released. A later claim of the
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
