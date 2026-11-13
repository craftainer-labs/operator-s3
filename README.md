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
