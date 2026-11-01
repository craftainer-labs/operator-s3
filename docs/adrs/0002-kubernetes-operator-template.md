# 0002. Provide a Kubernetes operator template on kubebuilder, tested in three tiers

## Status

Accepted

## Context

The craftainer-labs organization ships five Kubernetes operators, one per
backend (database, object storage, MQTT, cache, OIDC), each in its own
repository. They share a Go library (`lib-operator-go`) and a set of CRD
conventions, and every one needs the same plumbing: a manager with leader
election and authenticated metrics, admission webhooks with certificates, a
Helm chart, an OLM bundle, a tiny image that survives an arbitrary UID, and
tests that run against a real API server. Without a template, five repositories
copy it by hand and drift.

Inputs that bear on the design:

- `template-docker` (this template's parent, ADR 0001) already owns the image
  release contract: its root `Makefile`, `release.yml` and `scripts/` are
  `replace` tier and must stay byte-identical, so everything operator-specific
  has to attach through the instance-owned `image.mk`, the hook directories and
  new files.
- kubebuilder v4.16 (controller-runtime v0.25, Kubernetes 1.37 libraries)
  scaffolds the layout (`PROJECT`, `api/`, `cmd/`, `internal/`, `config/`) and
  has a Helm plugin (`helm/v2-alpha`). operator-sdk generates OLM bundles from
  that layout. Both OLM v0 and OLM v1 (`ClusterExtension`) install a
  `registry+v1` bundle that supports the `AllNamespaces` install mode; OLM v1
  also supports bundles with webhooks when cert-manager is present (operator-
  controller v1.12 docs, "Installation of Bundles containing Webhooks").
- `lib-operator-go` ADR 0002 fixes the conventions every operator follows:
  defaults in the schema, `Ready` and `Synced` conditions set on the first
  reconcile with `observedGeneration`, the `craftainer.dev/finalizer` finalizer
  with a `deletionPolicy`, a `craftainer.dev/poll-interval` annotation, Events.
- Backends reach a kind cluster as containers on the `kind` Docker network
  (plan decision of 2026-10-31); this devcontainer has no backend stack.
- Machines are small: a Go build and an envtest run share a 2 GB budget.

## Decision

We will create `craftainer/template-k8s-operator`, an instance of
`template-docker` that is itself a template, and create every operator from it.
It contains:

- **A kubebuilder v4 layout with one sample API**, `Greeting` in
  `sample.craftainer.dev/v1alpha1`. It provisions a ConfigMap (so the template
  needs no backend) and demonstrates the family conventions above, including a
  refusal to take over an object it does not own. The webhook is validating
  only, because defaults live in the schema.
- **Logic out of `main`.** `cmd/main.go` only passes the process's signals,
  arguments and `ctrl.GetConfig` to `internal/app.Run`, which builds the
  manager: leader election (Lease, released on cancel), a secure metrics
  endpoint (HTTPS, with controller-runtime's TokenReview and
  SubjectAccessReview filter), HTTP/2 off, TLS 1.2 or newer, JSON logs, probes,
  webhooks with a certificate directory. A `--probe-only` mode (also
  `MANAGER_PROBE_ONLY=true`) serves only `/healthz` and `/readyz`; it exists so
  the image can be smoke-tested without an API server.
- **Three test tiers.**
  1. Unit tests: the reconciler against controller-runtime's fake client, with
     faults injected into single calls so every error branch runs.
  2. envtest: the reconciler in a real manager, the webhook served to a real
     kube-apiserver, and `internal/app.Run` itself, against kube-apiserver and
     etcd fetched by `setup-envtest` at a pinned Kubernetes release
     (`ENVTEST_K8S_VERSION`, with `setup-envtest` at the controller-runtime
     version in `go.mod`).
  3. kind + Chainsaw end to end (`scripts/e2e.sh`, `test/e2e/`): one kind
     cluster per run on a digest-pinned node image, cert-manager from a
     checksummed release manifest, the image loaded into the node, then a
     Chainsaw test that installs the Helm chart into a Pod Security
     `restricted` namespace with the manager as an arbitrary UID, and checks the
     Lease, a Greeting reaching `Ready`, an update, the webhook's refusal, both
     deletion policies, and that `/metrics` answers 401 without a token and
     200 with one. Teardown is `kind delete cluster` and `docker network rm
     kind`. Backends an operator's e2e needs run as containers on the `kind`
     network and are reached by container name. CI runs the same script
     (`.github/workflows/e2e.yml`) with the same pinned kind binary.
- **A coverage gate over hand-written code.** `scripts/go-coverage.sh` runs
  unit and envtest suites under the race detector and requires 100% statement
  coverage (`COVERAGE_MIN`), excluding only `zz_generated.*` and `cmd/` (a
  wrapper around `internal/app`). `govulncheck` scans the built manager binary
  (`scripts/govulncheck.sh`), because its source-mode analysis of client-go was
  OOM-killed in a 2 GB container; `GOVULNCHECK_MODE=source` is available.
- **A distroless runtime image.** `Dockerfile` builds a static binary
  (`CGO_ENABLED=0`, cross-compiled) and ships it on
  `gcr.io/distroless/static-debian13:nonroot`, both bases pinned by digest. The
  binary writes nothing and the image has no home directory to write to, so it
  runs under any UID (`image.mk` smoke-tests it as `--user 12345:0`, read-only,
  capabilities dropped; the e2e runs it as 12345 under `restricted`).
- **A Helm chart** generated by kubebuilder's `helm/v2-alpha` plugin into
  `charts/chart` (not `dist/`, which the release contract owns and cleans), then
  edited by hand: `global.imageRegistry` and a digest-aware image helper for
  air-gapped installs. `scripts/check-chart-sync.sh` fails when the chart's CRD
  or manager role drift from `config/`.
- **An OLM bundle** from `operator-sdk`: `config/manifests` is `config/default`
  without cert-manager (OLM issues the webhook certificate itself from the
  CSV's `webhookdefinitions`), the CSV base declares `AllNamespaces` only and
  the `disconnected` infrastructure feature, and `scripts/bundle.sh` pins the
  manager image by digest and lists it in `relatedImages`. The bundle and a
  single-bundle file-based catalog are validated by `operator-sdk bundle
  validate` and `opm validate` in `make release-check`; they are generated, not
  committed.
- **The tooling of `template-go-module`**: the same pinned, checksummed Go
  toolchain, golangci-lint v2 and govulncheck installer, `.golangci.yml`
  (plus `ginkgolinter`), and the `prek` hooks. The Kubernetes tools (kubectl,
  kind, helm, kustomize, chainsaw, kubebuilder, operator-sdk, opm) come from
  `scripts/develop.d/20-k8s-tools.sh`, each verified against its release's
  SHA-256; controller-gen and setup-envtest are `go install`ed at pinned
  versions into `bin/`.
- **Operator targets in `operator.mk`**, included from `image.mk`, because the
  root `Makefile` cannot change. `image.mk` only adds prerequisites to
  `release-check` (`helm-lint`, `bundle-validate`, `generated-current`). Names
  that would shadow the image contract's targets are avoided.

## Consequences

- A new operator starts with the full quality bar and receives fixes by sync.
  Its own work is the API types, controllers, webhooks, CRDs, chart values, CSV
  base and Chainsaw tests, all `ignore` tier.
- Regenerating the Helm chart overwrites its hand-edited manager template; the
  chart is merged by hand and the sync check catches only CRD and RBAC drift.
  The cost of using the plugin's output as a starting point rather than owning
  a chart from scratch.
- 100% coverage over hand-written code costs fake-client tests for each error
  branch; the benefit is that a new branch in a reconciler cannot be added
  untested. An instance with generated clients or a large `main` lowers
  `COVERAGE_MIN` explicitly rather than excluding files.
- Binary-mode `govulncheck` cannot see vulnerabilities in code that is not in
  the shipped binary (test helpers); it can miss nothing the binary calls.
- The e2e needs about 2 GB free and Docker; it runs in the kind lane of the
  orchestration plan locally and on a GitHub-hosted runner in CI, not inside
  the prek hooks.
- `--probe-only` is production code that exists for tests. It is a few lines
  and covered, and the alternative (a fake kubeconfig) is brittle.
- One bundle per catalog image: a catalog with an upgrade graph needs the
  earlier bundles' entries as well; an operator that publishes several versions
  must extend `scripts/catalog.sh` (or render from its previous catalog image).
