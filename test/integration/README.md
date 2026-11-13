# test/integration/

Go tests of the operator's RustFS client (`internal/s3client`) against a real
RustFS 1.0.0-rc.5 with TLS, behind the build tag `integration` so `go test
./...` and the 100% coverage gate of `make test` never see them. The unit and
envtest suites use a fake behind `controller.Store` and an HTTPS test server;
this lane checks what those only claim, against the server:

- the admin routes and their SigV4 signatures (service `s3`, the region, the
  payload hash), and the authentication failures;
- the answers for something missing (404 for a user or service account on
  read, 400 for the requests that name one that is not there, success for
  removing what is gone), the `policyName` format of `user-info`, the shape of
  `list-canned-policies`, and that a policy still attached cannot be removed;
- bucket tags, versioning, lifecycle XML (with and without a prefix), object
  lock, and the quota routes and bodies (a bucket without a quota answers 200
  with `quota` 0);
- the limits of user names, service account keys and secret keys;
- that a service account has the policy of its user and no more: its bucket and
  prefix, read-only or read-write, nothing in another bucket, and that a change
  of policy does not change the key;
- the Bucket and BucketAccess controllers' sequences at the client level:
  create, take over, name collision, rotation with an overlap, and the delete
  of what the claim derived while the bucket and its objects stay;
- keep-alive against the real server, and verified TLS (an untrusted CA and a
  wrong host name fail).

Files:

- `server_test.go` — the environment, the helpers and the signed raw request.
  The tests read `S3_ENDPOINT` (default `https://localhost:9000`),
  `S3_CA_FILE` (required) and `S3_ADMIN_ACCESS_KEY` / `S3_ADMIN_SECRET_KEY` or
  the same with `_FILE` (one of each required). A missing setting fails the run;
  it never skips. Every test uses names of its own and removes what it created.
  The first test waits until the server's IAM is loaded.
- `admin_test.go`, `bucket_test.go`, `access_test.go`, `flows_test.go`,
  `transport_test.go` — the tests.
- `gen-certs.sh <dir>` — a throwaway CA, a server certificate for `rustfs`,
  `localhost` and `127.0.0.1` (as `rustfs_cert.pem` and `rustfs_key.pem`, the
  names RustFS loads from `RUSTFS_TLS_PATH`) and random admin credentials, with
  `openssl`. The key is world-readable because RustFS runs as another user than
  the one that generated it; the files live two days in a directory made for
  one run.

## Running

In CI, `.github/workflows/integration.yml` generates the certificates, starts
the pinned `rustfs/rustfs:1.0.0-rc.5-glibc` and runs the tests.

In the devcontainer, `.devcontainer/compose.instance.yml` starts the same
server as the `rustfs` service (certificates from `rustfs-tls-init` in the
`rustfs-tls` volume) and sets the `S3_*` variables, so:

```sh
go test -tags integration -count=1 ./test/integration/...
```

The integration coverage of the client (informational, the gate stays on the
unit profile):

```sh
go test -tags integration -count=1 -coverpkg=./internal/s3client/ -coverprofile=integration.cover ./test/integration/...
go tool cover -func=integration.cover
```

Anywhere else, run `gen-certs.sh` into a directory, start a server the way
`.github/workflows/integration.yml` does and export the `S3_*` variables. The
tests dial directly: the egress policy refuses loopback and is covered by its
own unit tests.

## Don't

- Point the tests at a server that holds anything you want to keep. They only
  create and remove buckets, objects, users, policies and service accounts with
  generated names, but they connect as the admin.
