# template-k8s-operator

> [!NOTE]
> This repository is vibe coded with [Claude](https://claude.com/product/claude-code).

A template for Kubernetes operators written in Go on
[kubebuilder](https://book.kubebuilder.io/) v4: an instance of
[`template-docker`](https://github.com/craftainer/template-docker) (and so of
[`template-base`](https://github.com/craftainer/template-base)) that is itself
a template. Create an operator from it and get:

- the kubebuilder v4 layout and a sample API (`Greeting`, group
  `sample.craftainer.dev`) that follows the family's CRD conventions;
- a manager with leader election, authenticated HTTPS metrics, probes and
  cert-manager-issued webhook certificates;
- unit tests, envtest suites and a kind + [Chainsaw](https://kyverno.github.io/chainsaw/)
  end-to-end test, behind a 100% coverage gate on hand-written code;
- a Helm chart ([kubebuilder `helm/v2-alpha`](https://book.kubebuilder.io/plugins/available/helm-v2-alpha)),
  published as OCI;
- an OLM bundle and file-based catalog ([operator-sdk](https://sdk.operatorframework.io/),
  installable with OLM v0 and OLM v1's `ClusterExtension`);
- a distroless static `nonroot` image that runs under an arbitrary UID;
- the Go tooling of `template-go-module` (pinned, checksummed toolchain,
  golangci-lint v2, govulncheck) and signed, attested releases.

See [`docs/TEMPLATE.md`](docs/TEMPLATE.md) for how it works and what an
instance changes, and [`docs/adrs/`](docs/adrs/) for why
([0002](docs/adrs/0002-kubernetes-operator-template.md) the template's design,
[0003](docs/adrs/0003-release-extras-and-supply-chain.md) release extras and
the supply chain).

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

## Creating an operator from this template

1. Create the repository from this template, then set
   `.github/template-sync-state.json` to `craftainer/template-docker` at the
   tag the template was created at (a repository created from a template
   starts with its parent's state file).
2. Rename: the Go module in `go.mod` and every import, `OPERATOR_NAME`,
   `LeaderElectionID` and `EventRecorderName` in `internal/app`, the chart
   (`charts/chart/Chart.yaml`, `values.yaml`: `manager.image.repository`), the
   CSV base file and its name in `config/manifests`, `namePrefix` and
   `namespace` in the kustomizations, and the labels in `config/`.
3. Replace the sample API: `kubebuilder create api` / `create webhook` for your
   kinds, delete `Greeting`, run `make manifests generate`, update the chart
   (see `docs/TEMPLATE.md`, "Helm chart") and `test/e2e/operator`.
4. Set the repository variable `OCI_REGISTRY=ghcr.io` (the image release and
   `release-extras.yml` both use it).

## License

Licensed under the [GNU General Public License v3.0](LICENSE).
