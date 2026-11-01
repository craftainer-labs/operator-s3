# 0003. Publish the chart, bundle and catalog from a dispatched workflow, signed and attested by a reusable one

## Status

Accepted

## Context

An operator ships more than an image: a Helm chart (OCI), an OLM bundle image
and a file-based catalog image. `template-docker`'s release contract (ADR
0001) builds, scans, publishes and merges the image, and nothing else:

- Its `release.yml` and `Makefile` are base-owned `replace` files. No job an
  instance can reach has `id-token` or `attestations` permissions, so the
  contract cannot sign or attest (the planned fix, NX1, is later).
- The bundle's CSV must name the image by digest, and the digest that matters
  is the multi-arch index's, which exists only after `finalize`.
- A workflow started by `workflow_run` is a zizmor finding
  (`dangerous-triggers`) that this family does not suppress; a release created
  with `GITHUB_TOKEN` starts no `release:` workflows; events created with
  `GITHUB_TOKEN` start only `workflow_dispatch` and `repository_dispatch`
  runs. `template-go-module` and `template-rust-cli` made the same call for
  their own post-release workflows (plan decision log, 2026-10-31).
- SLSA Build L3 needs the provenance to be produced by a builder the build
  steps cannot tamper with; on GitHub that means a reusable workflow whose
  signing identity is the called file, not the caller.

## Decision

We will publish the extras from `.github/workflows/release-extras.yml`, which
is `workflow_dispatch`-only with a `tag` input, and sign and attest with a
local reusable workflow, `.github/workflows/sign-attest.yml`
(`workflow_call`).

- **Dispatch.** The release lander dispatches it right after each `Release`
  succeeds (`gh workflow run release-extras.yml -f tag=vX.Y.Z`) and watches it.
  A guard job refuses to run unless: the ref is the default branch; the tag has
  the release-tag shape; a published (non-draft) release exists for it; and the
  tag is in the default branch's history. A concurrency group per tag
  serialises runs, and a repeated run for the same tag is safe (content-
  addressed pushes; signing again adds a signature).
- **What it publishes.** It resolves the released image's index digest with
  `docker buildx imagetools inspect`; packages the chart with that digest
  written into `values.yaml` (`scripts/extras/stage-chart.sh`) and
  `helm push`es it to `oci://ghcr.io/<owner>/charts`; generates the bundle for
  `<image>@<index digest>` (`scripts/bundle.sh`, which validates it) and pushes
  `<image>-bundle:<version>`; renders a file-based catalog from the pushed
  bundle image, validates it with `opm`, and pushes `<image>-catalog:<version>`
  for amd64 and arm64. Names derive from the same `OCI_REGISTRY` and
  `OCI_IMAGE_NAME` variables the image release uses (`scripts/extras/refs.sh`).
  GHCR only: login, signing and attestation use the workflow's `GITHUB_TOKEN`.
- **Signing and attestation.** For the image index, the chart, the bundle and
  the catalog, `sign-attest.yml` runs `cosign sign` (keyless), verifies the
  signature against its own workflow identity, and, for the three artifacts
  built in the same run, attaches a GitHub build-provenance attestation with
  `actions/attest` pushed to the registry. The image index was built by
  `release.yml`, not here, so it is signed but not given a provenance
  attestation that would misdescribe its builder. Only the reusable workflow
  and the job that calls it hold `id-token` and `attestations`; the publishing
  job holds `packages: write` and nothing else.
- **Pinning.** Every action is pinned by commit SHA, cosign by version (the
  installer verifies it), the tool binaries by version and SHA-256, and the
  catalog's `opm` base image by digest. zizmor passes with no suppression.
- **Sync classification.** Both workflows are `replace` tier in this template's
  manifest, as tombstone candidates: when the contract gains signing and a
  post-`finalize` publish hook (NX1 and NX4), the files are deleted and their
  manifest lines kept, so a sync removes them from every operator.

## Consequences

- Chart, bundle and catalog appear after the image, not with it, and only if
  someone dispatches the workflow. Forgetting is silent: a missing dispatch
  does not fail the `Release` run. The release lander's checklist carries it.
- The bundle and chart can name the image by digest because they are built
  after `finalize`; a rebuilt image with the same tag leaves earlier chart and
  bundle versions pointing at the digest they were released with.
- Provenance for the image index is absent until NX1 moves signing into the
  contract. The README says so.
- The catalog holds one bundle. An upgrade graph across versions needs the
  earlier bundles' entries (see ADR 0002).
- Registries other than GHCR are refused by `refs.sh` rather than half-working;
  supporting one means passing its credentials into the reusable workflow.
- The workflow's behaviour is covered by script tests and structural tests on
  the workflow text (`scripts/test_operator_scripts.py`), not by a live run in
  this repository: the first live run happens when the first release is
  dispatched.
