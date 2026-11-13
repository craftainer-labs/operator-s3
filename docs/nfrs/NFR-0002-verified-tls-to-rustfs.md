# NFR-0002. Every RustFS request is TLS 1.2 or newer, verifies the store, and is signed

## Status

Implemented

## Attribute

Security (encryption in transit, FIPS-capable build, plan row EB-14).

## Description

- The endpoint is an `https` URL with a host and without `user:password@`, a
  query or a fragment (`s3client.New`; the CRD and the webhooks check the same).
  There is no plaintext fallback.
- The store's certificate chain and host name are verified against the store's
  CA bundle (`tls.caBundle`, key `ca.crt` unless the reference names another;
  the system roots when there is none), with `tls.serverName` overriding the
  name. `InsecureSkipVerify` is never set. The TLS floor is `tls.minVersion`
  (1.2 by default, 1.3 on request). HTTP/2 is not negotiated.
- A redirect is never followed: it would carry a signed request to a host the
  egress policy has not seen named.
- Every request, admin and S3, is signed with AWS Signature Version 4, service
  `s3`, the store's `region` (`us-east-1` by default), over the SHA-256 of the
  body: the S3 calls by `minio-go/v7`, the admin calls with its
  `pkg/signer`. Both use one `http.Transport`, with the dialer, the CA bundle
  and the TLS floor above and no environment proxy. The admin API is RustFS's
  `/rustfs/admin/v3`, whose payloads are plain JSON. MinIO's `/minio/admin`
  surface (encrypted payloads, AGPL `madmin-go`) is not used (NR4).

## Source

Plan row EB-14 and the NR4 decision;
[ADR 0002 of lib-operator-go](https://github.com/craftainer-labs/lib-operator-go/blob/main/docs/adrs/0002-operator-crd-conventions.md), section 13.

## Verification

`TestNewRejectsWhatItCannotUse`, `TestNewDefaults`, `TestRequestsAreSigned` and `TestTheRegionReachesTheSignature` (the test
store checks the signature of every request, admin and S3, with its own
implementation of SigV4) and
`TestTransportFailuresAreCategorised` (an untrusted CA, a wrong name and a
plaintext server all fail) in `internal/s3client`; `TestBucketLifecycleRetain`
(`internal/controller`) checks that the store's endpoint, region, bundle
and floor reach the client. The `fips140=only` run is the library's
(lib-operator-go); this repository's release build follows `docs/TEMPLATE.md`.
Signing and the admin routes against a real RustFS, and that its certificate
chain and host name are verified (an untrusted CA and a wrong name fail with the
fixed category; TLS 1.3 and the name override work), are checked by the
integration lane, `TestSigV4IsAccepted` and `TestTLSIsVerified` in
`test/integration`.
