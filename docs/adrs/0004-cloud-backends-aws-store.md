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
  is neither `status.rotation.active` nor `status.rotation.previous`; every key that is neither it nor the active
  one (the previous key too: IAM's two keys leave no room for it) is a leftover whose secret nobody holds and is deleted first. So a
  crash between the Secret write and the status write neither deletes a key in
  use nor leaves a third key to hit the limit. Reissuing a lost active key
  keeps `status.rotation.previous`, still in its grace period.
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
