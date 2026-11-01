# The operator's own targets: code generation, tests, lint, Helm chart and OLM
# bundle. Included by image.mk (the root Makefile is template-docker's release
# contract and stays byte-identical, so nothing here may redefine one of its
# targets: build, sbom, release-assets, publish, publish-packages,
# verify-published, release-check, finalize, image-smoke). Template-owned: a
# sync overwrites this file; instances set variables in image.mk before the
# `include operator.mk` line, or add targets in their own .mk file.
#
# The structure follows kubebuilder's scaffolded Makefile (v4.16); the
# targets that would collide with the image contract are renamed
# (`build` -> `go-build`, `docker-build` -> `e2e-image`).

# The CRD group's domain, the operator's name, and the image the e2e runs.
OPERATOR_NAME ?= $(notdir $(CURDIR))
IMG ?= controller:latest
KIND_CLUSTER ?= $(OPERATOR_NAME)-e2e
E2E_NAMESPACE ?= $(OPERATOR_NAME)-system

# Where the Go-installed tools go. Everything else (kubectl, kind, helm,
# chainsaw, kustomize, kubebuilder, operator-sdk, opm) is installed on PATH by
# scripts/develop.d/20-k8s-tools.sh.
LOCALBIN ?= $(CURDIR)/bin

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
KUSTOMIZE ?= kustomize
KUBECTL ?= kubectl
KIND ?= kind
HELM ?= helm
CHAINSAW ?= chainsaw
OPERATOR_SDK ?= operator-sdk
OPM ?= opm

# renovate: datasource=go depName=sigs.k8s.io/controller-tools
CONTROLLER_TOOLS_VERSION ?= v0.22.0
# setup-envtest is versioned with controller-runtime: use the exact release
# this module requires (go.mod), so the two never drift and nothing floats.
ENVTEST_VERSION ?= $(shell go list -m -f '{{.Version}}' sigs.k8s.io/controller-runtime)
# The exact kube-apiserver and etcd release envtest runs against, fetched
# (and sha512-checked) by setup-envtest. Keep it at k8s.io/api's minor in
# go.mod; `bin/setup-envtest list` shows what exists.
ENVTEST_K8S_VERSION ?= 1.37.0

CONTAINER_TOOL ?= docker
# Version and bundle channel written into the OLM bundle.
BUNDLE_VERSION ?= 0.1.0
CHANNELS ?= alpha
DEFAULT_CHANNEL ?= alpha
HELM_CHART_DIR ?= charts/chart

.PHONY: operator-help
operator-help: ## Print the operator targets.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' operator.mk

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate the CRDs, RBAC role and webhook configuration.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate the DeepCopy methods.
	"$(CONTROLLER_GEN)" object:headerFile=hack/boilerplate.go.txt paths="./..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: ## Unit and envtest tests with the coverage gate (scripts/go-coverage.sh).
	scripts/go-coverage.sh

# Prints the directory of the envtest binaries (downloading them on first use);
# scripts/go-coverage.sh calls it unless KUBEBUILDER_ASSETS is already set.
.PHONY: envtest-assets
envtest-assets: setup-envtest
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path

.PHONY: go-build
go-build: ## Build the manager binary into bin/.
	CGO_ENABLED=0 go build -o bin/manager ./cmd

.PHONY: run
run: manifests generate ## Run the manager against the current kubeconfig, without webhooks.
	go run ./cmd --webhook-port=-1

##@ End-to-end (kind + Chainsaw)

.PHONY: e2e-image
e2e-image: ## Build the runtime image the e2e deploys (IMG).
	$(CONTAINER_TOOL) build --target runner -t $(IMG) .

.PHONY: test-e2e
test-e2e: ## Create a kind cluster, deploy the chart, run the Chainsaw tests, tear it all down (scripts/e2e.sh).
	KIND_CLUSTER=$(KIND_CLUSTER) IMG=$(IMG) NAMESPACE=$(E2E_NAMESPACE) scripts/e2e.sh

##@ Deployment (kustomize)

.PHONY: build-installer
build-installer: manifests generate ## Write dist/install.yaml (CRDs, RBAC, Deployment, webhook, cert-manager objects).
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=$(IMG)
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

.PHONY: install
install: manifests ## Install the CRDs into the cluster of the current kubeconfig.
	"$(KUSTOMIZE)" build config/crd | "$(KUBECTL)" apply -f -

.PHONY: uninstall
uninstall: manifests ## Remove the CRDs from the cluster.
	"$(KUSTOMIZE)" build config/crd | "$(KUBECTL)" delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests ## Deploy the manager with kustomize (needs cert-manager).
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=$(IMG)
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: ## Remove the manager.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found -f -

##@ Helm chart

# kubebuilder's helm/v2-alpha plugin generated charts/chart from `make
# build-installer`'s output. The chart is hand-edited from here on (see
# docs/TEMPLATE.md): regenerate only into a scratch directory and merge.

.PHONY: helm-lint
helm-lint: ## helm lint the chart (strict), render it with the default and the air-gapped values, check it matches config/.
	scripts/check-chart-sync.sh
	"$(HELM)" lint --strict $(HELM_CHART_DIR)
	"$(HELM)" template t $(HELM_CHART_DIR) > /dev/null
	"$(HELM)" template t $(HELM_CHART_DIR) --set global.imageRegistry=registry.example.com --set certManager.enabled=false --set webhook.enabled=false --set metrics.enabled=false > /dev/null

##@ OLM bundle

# `make bundle BUNDLE_IMG=<image@sha256:...> BUNDLE_VERSION=1.2.3` writes ./bundle: the
# CSV with the image pinned and listed in relatedImages, CRDs, RBAC and webhook
# definitions (scripts/bundle.sh). release-extras.yml passes the released
# image's index digest; the default is a placeholder digest, so `make
# release-check` validates the bundle without a registry. ./bundle,
# ./bundle.Dockerfile and ./catalog are generated and not committed.

BUNDLE_IMG ?= ghcr.io/example/operator@sha256:0000000000000000000000000000000000000000000000000000000000000000

.PHONY: bundle
bundle: manifests ## Generate and validate the OLM bundle (BUNDLE_IMG, BUNDLE_VERSION, CHANNELS, DEFAULT_CHANNEL).
	scripts/bundle.sh "$(BUNDLE_IMG)" "$(BUNDLE_VERSION)" "$(CHANNELS)" "$(DEFAULT_CHANNEL)"

.PHONY: bundle-validate
bundle-validate: bundle ## Validate the bundle (operator-sdk) and a file-based catalog built from it (opm).
	scripts/catalog.sh validate bundle

##@ Dependencies

$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen into bin/ if needed.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: $(ENVTEST) ## Install setup-envtest into bin/ if needed.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

# go-install-tool installs a pinned Go tool into bin/ under a versioned name
# and symlinks it, so a version bump reinstalls and nothing else does.
# $1 target path, $2 package, $3 version
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
echo "Installing $(2)@$(3)" >&2; \
rm -f "$(1)"; \
GOBIN="$(LOCALBIN)" go install $(2)@$(3); \
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)"; \
}; \
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef
