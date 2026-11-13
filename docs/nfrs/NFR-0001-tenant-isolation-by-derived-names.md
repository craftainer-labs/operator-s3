# NFR-0001. A claim reaches only the RustFS objects derived from its own namespace and name

## Status

Implemented

## Attribute

Security (multi-tenancy, plan row EB-3).

## Description

The namespace is the tenant. The operator uses an admin credential, which
RustFS cannot confine to a tenant, so only these things keep a claim away from
another tenant's objects:

- every bucket and IAM user name is derived (`internal/s3client/names.go`:
  `BucketName`, `UserName`) with `naming.Tenant` from the claim's namespace and
  name; there is no field that sets a name. A bucket can hold the ownership
  marker, so it takes the short hash (50 bits, at most 63 bytes); an IAM user
  and its service accounts cannot, so the user takes the long hash (80 bits, at
  most 54 bytes, which leaves room for a policy name of 63). The access key of
  a service account is derived from the claim's UID and the rotation sequence,
  so no other claim can derive it;
- the marker `craftainer.dev/claim=<namespace>/<name>/<uid>` is the bucket tag
  of the same key. A bucket whose marker names another claim is never read
  beyond that tag, changed or deleted, and the claim gets `Synced=False`,
  reason `NameCollision`. A bucket with no marker is refused as unmanaged
  unless the claim's status recorded the create before it was issued
  (`status.externalId` is `create-issued`); the operator never adopts or
  deletes a bucket it did not create;
- IAM users cannot hold a marker, so the same rule rests on the status alone: a
  user that exists while the claim recorded no create is refused and never
  changed or deleted. A user's policy is attached by name (`<user>-<digest of
  the document>`), and only policies of that form are ever removed;
- a cluster store serves only the namespaces its `allowedNamespaces` admits
  (`server.Resolve`), and a namespaced store only its own.

A BucketAccess may use only a Bucket of its own namespace on the same store,
and its policy covers that bucket (and the prefix, which may not hold `*`, `?`
or `$`) and nothing else.

## Source

Plan row EB-3; [ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), sections 2 and 10.

## Verification

Unit and envtest suites, on every push: `TestDerivedNames` and
`TestAccessKeyIsDeterministicPerGeneration` (`internal/s3client`),
`TestBucketLifecycleRetain` and `TestBucketIntentFirstAdoption` (a collision
or an unmarked bucket leaves no write and no delete),
`TestAccessNeverAdoptsAnUnmarkedUser`, `TestAccessRefusesAWideningPrefix`,
`TestBucketWithoutAStoreOrANamespace` (`NamespaceNotAllowed`) and
`TestAccessNeedsAUsableBucket` (`internal/controller`). Whether the claim
reaches only its own objects on a live store, and that a policy confines a key
to its bucket and prefix, is checked by the RustFS integration lane in
`test/integration`: `TestPolicyConfinesAKeyToItsBucketAndPrefix`,
`TestDeleteAccessTakesOnlyItsOwn` and the two claim flows.
