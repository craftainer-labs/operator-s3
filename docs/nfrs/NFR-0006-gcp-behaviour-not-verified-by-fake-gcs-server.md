# NFR-0006. State what the GCS integration lane cannot check

## Status

Implemented

## Attribute

Maintainability and verifiability (honest test coverage of the GCS store).

## Description

The integration lane runs the GCS store against fake-gcs-server (BSD-2-Clause,
pinned by digest). Every claim that rests on real GCS behaviour is either
checked elsewhere or documented as unverified. fake-gcs-server cannot check:

- **The ownership label, the lifecycle rules and the location.** It keeps a
  bucket's name and versioning, and accepts and drops the rest. The unit tests
  check that the store sends them, against an httptest fake of the JSON API
  written from Google's reference; whether GCS keeps and returns them as the
  fake does (label value limits, the shape of a returned rule) is not checked.
- **Uniform bucket-level access and public access prevention.** It accepts
  and drops `iamConfiguration` and never returns it, so the integration lane
  cannot assert that they are set or read back (a bucket there always reads as
  drifted). The unit tests assert what the store sends and how it reads a
  bucket; that GCS applies and reports them is not checked.
- **Authentication and IAM.** It validates no token and enforces no
  permission, so a refused credential, the `403` of a bucket another project
  holds, `storage.buckets.list` as the probe's permission, and the
  `service_account` and workload identity credentials of a real cluster are not
  exercised.
- **Name collisions and `409` on a non-empty bucket.** The unit fake imitates
  them; the emulator's answer for a bucket that holds objects is not used.
- **Service accounts, bucket IAM policies and HMAC keys.** The emulator has
  none. They are not used until `BucketAccess` on GCP is decided and
  implemented (ADR 0004, addendum); that item adds an httptest fake of
  `iam/v1` and the HMAC endpoints on a private address, and lists what that
  fake cannot check (IAM eventual consistency, a role really limiting a key).

## Source

The template's rule that a test's blind spots are written down, and NKR1's
findings on fake-gcs-server. See
[ADR 0004](../adrs/0004-cloud-backends-aws-store.md), addendum 1.

## Verification

Unit tests cover the fake's side (`internal/gcs`, `internal/controller`,
`internal/s3client`). A release that changes the GCS store is run once by hand
against a real project for the points above before it is relied on; this file
and the README's "GCP stores" section are updated when a point becomes
checkable.
