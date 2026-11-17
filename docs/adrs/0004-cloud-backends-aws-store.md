# 0004. Serve claims on AWS through an AWS store, with IAM users per BucketAccess

## Status

Accepted

## Context

The operator began as a RustFS operator. `lib-operator-go` ADR 0002 section 16
adds an optional `provider` to the store kinds, so a `ClusterObjectStore` or
`ObjectStore` can name an AWS account instead of a RustFS endpoint. The same
`Bucket` and `BucketAccess` claims then have to work on S3 and IAM, whose
semantics differ from RustFS's in ways that matter to ownership and rotation:

- S3 has no bucket quota, and IAM users carry tags but RustFS users do not.
- IAM generates an access key and shows its secret once. RustFS lets the
  operator choose the key id, so a retry can reissue the same key; IAM cannot.
- IAM allows two access keys per user.
- IAM is eventually consistent.
- S3 answers a `HEAD` on a bucket of another account with `403`, and one in
  another region with `301`.
- A region belongs to a partition (`aws`, `aws-cn`, `aws-us-gov`) that sets the
  ARN prefix and, for China, the endpoint domain.

## Decision

- **An AWS store** (`internal/awss3`) implements the same store interfaces as
  the RustFS client, on the AWS SDK for Go v2. Every request dials through the
  egress policy and the store's proxy; `tls.caBundle` is the set of trusted
  roots. Its health probe is STS `GetCallerIdentity` plus S3 `ListBuckets`.
- **One IAM user per `BucketAccess`**, named `<namespace>-<name>-<hash>`,
  created under `provider.aws.iamPath` with `provider.aws.permissionsBoundaryARN`
  if set. The path and the boundary apply at creation only. The access is one
  inline policy named after the document's digest, attached before the old one
  is removed. Deleting removes keys, policies, then the user.
- **Ownership is a tag.** A user and a bucket carry `craftainer.dev/claim`
  (`namespace/name/uid`). Only an entity with that exact tag is adopted,
  changed or deleted; anything else is a `NameCollision`. A user whose tag
  names this claim is adopted even when status lost the record of the create.
- **Key rotation uses the user's two keys.** The first key is made at create.
  A rotation makes the second, writes it to the binding Secret and, after the
  grace period, deletes every key but the new one. Because IAM's secret is
  shown once and its id cannot be derived, a retry of a crashed rotation
  cannot reissue the same key. Instead `RotateBegin` keeps the key the binding
  Secret already holds and reuses it, when the Secret is controlled by the
  claim, a key is active, and the held key has a secret, exists on the user and
  is neither `status.rotation.active` nor `status.rotation.previous`. Every
  other key, the previous one included, is a leftover whose secret nobody holds
  and is deleted first: a rotation begins only after the last one finished, so
  the previous key is out of its grace period, and IAM's two keys leave no room
  to keep it. A crash between the Secret write and the status write therefore
  neither deletes a key in use nor leaves a third key to hit the limit. Only
  reissuing a lost active key (not a rotation) keeps
  `status.rotation.previous`, still in its grace period.
- **A bucket quota is refused.** A `Bucket` with `spec.quota` on an AWS store is
  `Synced=False`, reason `ReconcileError`, and nothing is changed. A quota
  cannot be read back, so an operator that applied one would update in a loop.
- **HEAD semantics.** On the existence check of a bucket, `404` is absent,
  `200` is present (its tag decides ownership), `301` (another region) is a
  `NameCollision`, and `403` is an authentication failure, not a collision: S3
  gives it both to refused credentials and to a bucket of another account, so
  the claim reports the store unreachable rather than guess. A create that
  loses a race answers `BucketAlreadyExists`, a `NameCollision`.
- **The partition follows the region.** `cn-` is `aws-cn`, `us-gov-` is
  `aws-us-gov`, `us-iso-`, `us-isob-`, `eu-isoe-` and `us-isof-` are `aws-iso`,
  `aws-iso-b`, `aws-iso-e` and `aws-iso-f`, `eusc-` is `aws-eusc`, anything else
  `aws`. Only the China regions have a default endpoint domain; an isolated or
  sovereign region needs `endpointOverride`. The region is
  `provider.aws.region`, so an endpoint override does not change it. It sets the ARN of the access policy and
  the China endpoint domain; an endpoint override is used as given.
- **IAM settling is retried, never waited for.** A missing entity right after
  the operator made it, or a change in flight, is a `lifecycle.PendingError`,
  so the claim is looked at again in seconds.

## Consequences

- One claim model serves both stores; the differences stay inside the store and
  the `accessExternal` generated-key path.
- Ownership by tag means a stale or foreign user must be removed by hand, as
  for RustFS buckets, but no claim ever touches an object it did not create.
- A crash can leave the user with two keys for the length of a retry. The
  reuse rule makes that safe, at the cost of one more read of the binding
  Secret per rotation.
- A path, a boundary or a policy changed on the store after the user exists is
  not reconciled. Changing them means a new claim.
- What emulation cannot show is listed in
  [NFR-0005](../nfrs/NFR-0005-aws-behaviour-not-verified-by-moto.md).

## Addendum 1: GCP stores (NKs3-2a)

### Status

The buckets part is Accepted when NKs3-2a merges. The `BucketAccess` part
(below) is **Proposed, awaiting G-api**: `lib-operator-go` ADR 0002 section 16.7
leaves the credential model, binding keys and status identifiers of
`BucketAccess` on GCP to this operator's ADR and to the user's API review. It
is not implemented.

### Context

A store with `provider.type: GCP` and a `provider.gcp` block (`project`, and
optionally `location`, plus `endpointOverride` on a `ClusterObjectStore`)
serves `Bucket` claims on Cloud Storage. The credential is a `service_account`
key in a Secret (`credentials.json`) or the operator's own Application Default
Credentials (WorkloadIdentity), resolved by `cloud.Egress.GCP`. GCS differs
from S3 and from RustFS in ways that matter here:

- A label key allows lowercase letters, digits, underscore and dash, and a
  value the same up to 63 characters. `craftainer.dev/claim` and
  `namespace/name/uid` cannot be a label.
- A lifecycle rule has no ID.
- There is no bucket quota, and no per-bucket object lock (retention is a
  different feature with other semantics).
- A bucket name another project holds answers `409` on create and `403` on
  read, like a refused credential (told apart by a list, see below).
- fake-gcs-server keeps a bucket's name and versioning only.

### Decision

- **A GCP store** (`internal/gcs`) implements the bucket methods of
  `controller.Store` on `cloud.google.com/go/storage` over the JSON API. The
  HTTP client comes from `cloud.Egress.GCP`, so every request dials through the
  egress policy and the store's proxy. One attempt per call: the controller
  retries.
- **Ownership is a label** `craftainer-dev-claim`, written when the bucket is
  created (the label, versioning and lifecycle go in one create request, so
  there is no unmarked bucket to adopt). Its value is a digest of
  `namespace/name/uid` (the first 20 bytes of its SHA-256, in hex), because the
  claim cannot be a label value; the controller asks the store for that form
  (`EncodeMarker`) before it compares. A bucket with another or no marker is a
  `NameCollision`, as on AWS, and is never adopted, changed or deleted. Unlike
  AWS, a bucket with no marker is not taken over even when the claim recorded the
  create: GCS writes the label in the create request, so an unlabelled bucket is
  never the claim's.
- **Rules are compared by prefix and days.** A GCS rule has no ID, so a store
  reports `NoRuleIDs` and the controller ignores the IDs of the claim's rules
  when it compares. A rule of any other shape (another action, several
  prefixes, another condition) is reported `Disabled` and replaced, as on AWS.
- **A quota and object lock are refused** (`ReconcileError`, nothing is
  changed), for the reason a quota is refused on AWS.
- **The probe** lists one page of the project's buckets
  (`storage.buckets.list`), the smallest read that shows the project exists and
  the identity may use Cloud Storage in it. It discovers `accountId` (the
  project ID), `endpoint` and, when the store names one, `region` (the
  location). `location` is optional; empty is GCS's default, `US`.
- **HTTP semantics.** `404` is absent, `409` on create is `NameCollision`, `409`
  on delete is a bucket that is not empty (never emptied by the operator), and
  `401` and `403` are an authentication failure of an unreachable store,
  because GCS gives `403` both to refused credentials and to a bucket of
  another project. A `403` on a bucket read is told apart by listing the
  project's buckets: when that works the name is held elsewhere (`NameCollision`,
  so a delete of the claim is not blocked). A token the service-account key
  cannot get has no API status and is also an authentication failure.
- **`BucketAccess` is not implemented.** `ApplyAccess` answers a fixed "not
  implemented" error; finding and deleting access finds nothing, so a claim that
  never got an account can be deleted.

### BucketAccess on GCP: proposal (Proposed, awaiting G-api)

1. **Credential model.** A per-claim service account (`iam/v1`) with HMAC keys
   (`projects.hmacKeys`), used through the S3-interoperability endpoint
   `storage.googleapis.com`. It is the only model that keeps the binding shape
   and the consumer unchanged from AWS and RustFS, and GCS allows several HMAC
   keys per service account, so a rotation overlaps for real: the new key is
   issued first and the previous deleted after `gracePeriod`. Service account
   JSON keys would need another binding (`credentials.json`) and are the
   option Google discourages.
2. **Binding keys.** The self-hosted set plus `cloud: gcp`: `endpoint`
   (`https://storage.googleapis.com`, or the discovered value), `region` (the
   store's location), `bucket`, `prefix`, `accessKeyId` and `secretAccessKey`
   (the HMAC access id and secret), `pathStyle`. The service account's email is
   in the binding Secret and in status only.
3. **Status.** `externalName` is the service account id, `externalId` its
   email, `rotation.active` the HMAC access id.
4. **Marker and names.** A service account cannot carry a label, so the claim
   is in its `description` (`craftainer.dev/claim=<namespace/name/uid>`; 256
   characters at most). The account id is `naming.Tenant` with `-` and
   `HashLong`, 30 characters at most, so the name is a second barrier. Only an
   account with that exact description is adopted, changed or deleted. The
   bucket label is `craftainer-dev-claim`, as above.
5. **Grant.** A bucket IAM binding of `serviceAccount:<email>` to
   `roles/storage.objectAdmin` (`access: ReadWrite`) or
   `roles/storage.objectViewer` (`ReadOnly`), written with the policy's etag.
   A `prefix` needs an IAM condition on the resource name. Delete order: HMAC
   keys (inactive, then deleted), the binding, then the service account. A
   service account or key that is not visible yet is a `lifecycle.PendingError`.

Any change to section 16 that this needs (the binding keys, the status
identifiers, the service account marker) goes back to G-api.

### Consequences

- What emulation cannot show is listed in
  [NFR-0006](../nfrs/NFR-0006-gcp-behaviour-not-verified-by-fake-gcs-server.md).
- The label and the lifecycle rules are checked against an httptest fake of
  the JSON API, not against GCS, until a real project is used.
- Buckets are created hardened (uniform bucket-level access, public access
  prevention enforced) and a claim's own bucket that drifted is hardened again,
  as Google recommends and as AWS gives new buckets (ACLs disabled, Block Public
  Access). It fits the proposed IAM-role based BucketAccess. No API field.
- The location is applied at create only; a bucket in another location is
  neither moved nor reported.
