# 0001. Shared image layer as a template-docker mid-layer

## Status

Accepted

## Context

Three app templates (`template-fastapi`, `template-axum`, `template-react`)
each shipped a container image, and each carried its own copy of the image
release: the `Makefile`, the OCI labels, the SBOM step, the smoke test and
the arbitrary-UID runner setup. Only `template-fastapi` was an instance of
`template-base`, so only it received template-sync fixes; the axum and
react copies drifted. Fixing a release bug meant three separate edits, and
two of them never reached the instances.

## Decision

We will keep the generic image release in one template, `template-docker`:
an instance of `template-base` that is itself a template, and the parent of
the three app templates.

- It owns the `Makefile` (`replace` tier) implementing base's release
  contract for images. Per-instance settings go in `image.mk` (`ignore`
  tier): `DOCKERFILE`, `TARGET`, `ARCHES`, `BUILD_ARGS`, `EXTRA_LABELS`,
  `OCI_DESCRIPTION`, `OCI_LICENSE`, `COVERAGE_REPORT` and the
  `IMAGE_SMOKE_*` variables. Instances add `release-check` steps as
  prerequisites, since the Makefile gives that target no recipe.
- It owns `scripts/image-smoke.sh`, `scripts/runner-setup.sh` (with the
  instance hook directory `runner-setup.d/`) and
  `scripts/check-dockerfile-versions.sh`, all `replace` tier. The
  `Dockerfile` is a skeleton the instance owns (`ignore`); `.dockerignore`
  is `merge`.
- It adds the `hadolint`, `check-dockerfile-versions` and `image-scan`
  hooks (the last at the `manual` stage, using Trivy, pinned by tag and digest because of the March 2026
  compromise of its releases, GHSA-69fq-xp46-6x23) and Renovate
  coverage for the Makefile pins and the hook images.
- OCI labels follow `template-fastapi`'s ADR-0014; the image name derives
  from `GITHUB_REPOSITORY`; registry auth stays the opt-in `OCI_REGISTRY*`
  secrets contract.

## Consequences

- Sync becomes three hops: base to `template-docker` to the app template
  to its instances. A base change reaches an app only after
  `template-docker` has taken it and cut a tag.
- Existing apps re-parent once: bootstrap the sync state with
  `template_repo=craftainer/template-docker`, and migrate axum and react
  to base's layout first (see `docs/TEMPLATE.md`).
- `template-fastapi`'s ADR-0014 and ADR-0016 stay in that repo and gain
  supersede notes pointing here.
- An app that needs something the Makefile's variables don't cover has to
  change `template-docker`, not its own copy of the Makefile.
- `template-docker` must keep its manifest, hooks and Renovate config
  merged with base's by hand on each base sync.
