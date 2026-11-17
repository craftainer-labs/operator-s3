# NFR-0005. State what the AWS integration lane cannot check

## Status

Implemented

## Attribute

Maintainability and verifiability (honest test coverage of the AWS store).

## Description

The integration lane runs the AWS store against moto server. Every claim that
rests on real AWS behaviour is either checked elsewhere or documented as
unverified. moto cannot check:

- **IAM eventual consistency.** moto is consistent at once. The `PendingError`
  retry on a missing entity, and a new key that does not work yet, are covered
  only by unit tests against a fake that returns `NoSuchEntity`.
- **Real STS web identity.** moto does not validate a web identity token or
  assume a role, so `WorkloadIdentity` and the operator's role credentials are
  untested against AWS.
- **Permission boundaries and policies limiting access.** moto does not enforce
  IAM, so neither a boundary's limits nor whether a key can really read or
  write only its bucket and prefix is verified. The policy document is checked
  as text.
- **Partition behaviour.** moto serves one endpoint. The China and GovCloud ARN
  prefix and the China endpoint domain are checked as strings in unit tests,
  not against a real `aws-cn` or `aws-us-gov` account.
- **HEAD 403 and 301 from real S3**, which a unit-test fake imitates.

## Source

A reviewer of NKs3-1b-i, and the template's rule that a test's blind spots are
written down. See [ADR 0004](../adrs/0004-cloud-backends-aws-store.md).

## Verification

Unit tests cover the fakes' side (`internal/awss3`, `internal/controller`,
`internal/s3client`). A release that changes the AWS store is run once by hand
against a real account for the four points above before it is relied on; this
file and the README's "AWS stores" section are updated when a point becomes
checkable.
