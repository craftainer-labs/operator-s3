# scripts/

- `develop.sh` — `../.devcontainer/Dockerfile`'s `develop`-stage setup: installs `prek`,
  the Claude Code CLI, `snip` and the GitHub CLI (`gh`), the only tooling this template requires
  regardless of instance language. An instance adds its own language
  runtime/tooling install on top, either by extending this script or
  copying its shape into a second script. Prefer the hook below over
  either. Every download goes through its `fetch` helper (bounded curl
  retries with backoff; GitHub URLs authenticated when the optional
  `github_token` BuildKit secret is present) and the installers
  that download for themselves run through `retry` (the installer script
  itself is fetched once, by `fetch`, so the two layers of retries do not
  multiply; a failed download names the installer and URL before the script
  aborts, an empty one is rejected, and `fetch` itself retries DNS and
  connect failures (curl exit 6/7) and truncated transfers (18/52/55/56) up
  to 4 times; timeouts are left to curl's own `--retry`), so CI's shared-IP
  HTTP 429s don't fail the image build. Local builds need no token.
- `post-create.sh` — the devcontainer's `postCreateCommand`: git
  `safe.directory`, `prek install`, then every `post-create.d/*.sh`.
- `develop.d/`, `post-create.d/`, `runner-setup.d/` — instance hook
  directories, empty apart from their own `README.md`; see
  `docs/TEMPLATE.md`'s "Instance extension points".
- `runner-setup.sh` — optional; the top-level `Dockerfile`'s `runner` stage
  setup: group-0 runtime user for arbitrary-UID platforms (run before copying
  app code, which is then copied read-only), then every
  `runner-setup.d/*.sh`. An instance whose runner image doesn't call it
  (an nginx runner, say) ignores it and `runner-setup.d/`.
- `image-scan.sh` — the manual-stage `image-scan` hook: Trivy over the
  tar for the current `RELEASE_VERSION`/arch in `dist/`.
- `image-dast.sh` — the manual-stage `image-dast` hook: starts the image from the
  tar in `dist/` as `image-smoke.sh` does and runs the pinned ZAP baseline scan
  (passive) in its network namespace, with rule levels from the
  instance-owned `.zap/rules.tsv`; reports go to `dist/dast/`.
- `test-image-scripts.sh` — the `image-scripts-test` hook: offline tests of
  the Makefile's repo derivation, the `publish`/`finalize` registry
  credentials (secrets, the ghcr.io `GITHUB_TOKEN` fallback, the non-ghcr
  failure, no password in an argv; docker stubbed) and these scripts.
- `image-smoke.sh` — `make image-smoke` (a `release-check` prerequisite):
  converts the arch's image in the release OCI tar from `dist/` to a
  docker-archive with the pinned skopeo (`docker load` can't read a buildx
  OCI tar on the classic image store), loads and runs it, and polls
  `IMAGE_SMOKE_PATH` until 2xx. Fails when there is no tar for the arch under
  test. Needs `SKOPEO_IMAGE`, which the Makefile exports; usage in the
  script's header.
- `skopeo.sh` — runs the pinned skopeo image as the caller's user with
  `dist/` mounted at `/dist`; used by `make publish` and `image-smoke.sh`.
- `check-dockerfile-versions.sh ARG...` — fails when a named `ARG` default
  differs between `.devcontainer/Dockerfile` and the top-level `Dockerfile`.
- `develop.d/10-go.sh`, `develop.d/20-k8s-tools.sh`,
  `post-create.d/10-go-download.sh` — the devcontainer's pinned,
  checksum-verified tools: Go, golangci-lint and govulncheck
  (`template-go-module`'s installer); kubectl, kind, helm, kustomize, chainsaw,
  kubebuilder, operator-sdk and opm (`20-k8s-tools.sh [tool...]`, with
  `INSTALL_DIR` for CI); and `go mod download` after create.
- `go-coverage.sh`, `govulncheck.sh`, `check-generated.sh`,
  `check-go-version.sh`, `check-chart-sync.sh` — the Go and Kubernetes hooks'
  entry points (`docs/TEMPLATE.md`, "Kubernetes operator template notes").
- `bundle.sh`, `catalog.sh` — the OLM bundle for an image digest and version,
  and a single-bundle file-based catalog for it (`render`, `validate`,
  `dockerfile`).
- `e2e.sh` — the kind + Chainsaw end-to-end run, always followed by teardown.
- `extras/` — what `release-extras.yml` calls: `check-dispatch.sh` (the
  dispatch guard), `check-absent.sh` (refuses to publish over an existing tag;
  with `--resume` prints the digests of what exists), `refs.sh` (artifact names), `stage-chart.sh` (the chart with
  the image digest pinned).
- `test_operator_scripts.py` — offline tests of all of the above and of the
  workflows' shape, run by the `operator-scripts-test` hook.

## Do

- Pass a token to the build as a BuildKit secret (`github_token`), never
  a `--build-arg`/`ARG`: an ARG is baked into the image history.
- Keep this script's own tools pinned to an exact version (a `# renovate:`
  ARG in `../.devcontainer/Dockerfile`) and checksum-verified where the upstream project
  publishes checksums, matching the pattern each install step here already
  follows.

## Don't

- Add a language runtime or stack-specific tool to `develop.sh` — that
  belongs in the instance's own layer (`develop.d/`), as this template's Go
  and Kubernetes tools do.
