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
an in-memory store and an HTTPS test server. They have not run against a real
RustFS yet (item NO3b-2, below). The stores do not report health yet (below).

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
  the operator never deletes objects.
- **BucketAccess** creates an IAM user with one policy for the bucket (and
  `prefix`), and one service account per credential generation. The binding
  Secret (type `servicebinding.io/s3`) holds the service account's keys.
  Rotation issues the next service account first and deletes the previous one
  after `gracePeriod`. A change of `access` or `prefix` updates the policy and
  the Secret's fields but keeps the key in use; its secret key is set again only
  when the Secret holds no valid one. The access waits (and is reconciled again
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

A deletion blocked this way keeps the finalizer, by design.

### Not done yet

- **NO3b-2:** a real RustFS lane (a TLS RustFS service in the devcontainer and
  in CI) to check the admin routes, the SigV4 signatures, the 404 versus 500
  answers for a missing object, the quota body (and whether a bucket without a
  quota answers 404, which the client reads as no quota), the canned policy list,
  tagging and lifecycle, and that a policy confines a key to its bucket and
  prefix. Policy document drift is not
  detected, only which policy is attached.
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
