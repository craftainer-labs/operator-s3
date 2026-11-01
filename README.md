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

Status: the API types, CRDs and admission webhooks are in place; the
controllers that act on them are added in later changes, so the manager
serves the webhooks and nothing reconciles yet.

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
