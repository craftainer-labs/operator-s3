#!/usr/bin/env bash
# Installs the Kubernetes tooling the operator template's checks, e2e and
# release use, each pinned and verified against the SHA-256 its release
# publishes: kubectl, kind, helm, kustomize, chainsaw, kubebuilder,
# operator-sdk and opm. Runs as root during the `develop` stage build (see
# README.md here), and in CI (.github/workflows/e2e.yml and
# release-extras.yml) with INSTALL_DIR set to a runner directory. The Go
# toolchain, golangci-lint and govulncheck come from 10-go.sh; controller-gen
# and setup-envtest are `go install`ed by operator.mk.
#
# Usage: 20-k8s-tools.sh [tool...]   (default: all; names as in TOOLS below)
# Environment: INSTALL_DIR (default /usr/local/bin).
set -euo pipefail

# renovate: datasource=github-releases depName=kubernetes/kubernetes
KUBECTL_VERSION=1.37.2
# renovate: datasource=github-releases depName=kubernetes-sigs/kind
KIND_VERSION=0.33.0
# renovate: datasource=github-releases depName=helm/helm
HELM_VERSION=4.3.1
# renovate: datasource=github-releases depName=kubernetes-sigs/kustomize extractVersion=^kustomize/v(?<version>.+)$
KUSTOMIZE_VERSION=5.8.1
# renovate: datasource=github-releases depName=kyverno/chainsaw
CHAINSAW_VERSION=0.2.15
# renovate: datasource=github-releases depName=kubernetes-sigs/kubebuilder
KUBEBUILDER_VERSION=4.16.0
# renovate: datasource=github-releases depName=operator-framework/operator-sdk
OPERATOR_SDK_VERSION=1.42.3
# renovate: datasource=github-releases depName=operator-framework/operator-registry
OPM_VERSION=1.74.0

TOOLS=(kubectl kind helm kustomize chainsaw kubebuilder operator-sdk opm)
install_dir="${INSTALL_DIR:-/usr/local/bin}"

case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *)
        echo "unsupported architecture for the Kubernetes tools: $(uname -m)" >&2
        exit 1
        ;;
esac

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
mkdir -p "$install_dir"

# fetch URL FILE: download, failing on any HTTP error.
fetch() {
    curl -LsSf --retry 3 --retry-all-errors -o "$2" "$1"
}

# verify DIR FILE SUM: FILE in DIR must hash to SUM (a 64-hex SHA-256).
verify() {
    if [[ ! "$3" =~ ^[0-9a-f]{64}$ ]]; then
        echo "no valid SHA-256 for $2: '$3'" >&2
        exit 1
    fi
    (cd "$1" && echo "$3  $2" | sha256sum -c -)
}

# sum_from FILE NAME: the SHA-256 listed for exactly NAME in a checksums file
# ("<sum>  <name>" lines; an exact match, as the files also list .sbom.json).
sum_from() {
    awk -v f="$2" '$2 == f { print $1 }' "$1"
}

# install_binary NAME FILE: the file in $tmpdir becomes $install_dir/NAME.
install_binary() {
    install -m755 "${tmpdir}/$2" "${install_dir}/$1"
}

install_kubectl() {
    local base="https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${arch}"
    fetch "${base}/kubectl" "${tmpdir}/kubectl"
    fetch "${base}/kubectl.sha256" "${tmpdir}/kubectl.sha256"
    verify "$tmpdir" kubectl "$(tr -d '[:space:]' <"${tmpdir}/kubectl.sha256")"
    install_binary kubectl kubectl
}

install_kind() {
    local file="kind-linux-${arch}"
    local base="https://github.com/kubernetes-sigs/kind/releases/download/v${KIND_VERSION}"
    fetch "${base}/${file}" "${tmpdir}/${file}"
    fetch "${base}/${file}.sha256sum" "${tmpdir}/${file}.sha256sum"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/${file}.sha256sum" "$file")"
    install_binary kind "$file"
}

install_helm() {
    local file="helm-v${HELM_VERSION}-linux-${arch}.tar.gz"
    fetch "https://get.helm.sh/${file}" "${tmpdir}/${file}"
    fetch "https://get.helm.sh/${file}.sha256sum" "${tmpdir}/${file}.sha256sum"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/${file}.sha256sum" "$file")"
    mkdir "${tmpdir}/helm"
    tar -xzf "${tmpdir}/${file}" -C "${tmpdir}/helm"
    install -m755 "${tmpdir}/helm/linux-${arch}/helm" "${install_dir}/helm"
}

install_kustomize() {
    local file="kustomize_v${KUSTOMIZE_VERSION}_linux_${arch}.tar.gz"
    local base="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}"
    fetch "${base}/${file}" "${tmpdir}/${file}"
    fetch "${base}/checksums.txt" "${tmpdir}/kustomize-checksums.txt"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/kustomize-checksums.txt" "$file")"
    mkdir "${tmpdir}/kustomize"
    tar -xzf "${tmpdir}/${file}" -C "${tmpdir}/kustomize"
    install -m755 "${tmpdir}/kustomize/kustomize" "${install_dir}/kustomize"
}

install_chainsaw() {
    local file="chainsaw_linux_${arch}.tar.gz"
    local base="https://github.com/kyverno/chainsaw/releases/download/v${CHAINSAW_VERSION}"
    fetch "${base}/${file}" "${tmpdir}/${file}"
    fetch "${base}/checksums.txt" "${tmpdir}/chainsaw-checksums.txt"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/chainsaw-checksums.txt" "$file")"
    mkdir "${tmpdir}/chainsaw"
    tar -xzf "${tmpdir}/${file}" -C "${tmpdir}/chainsaw"
    install -m755 "${tmpdir}/chainsaw/chainsaw" "${install_dir}/chainsaw"
}

install_kubebuilder() {
    local file="kubebuilder_linux_${arch}"
    local base="https://github.com/kubernetes-sigs/kubebuilder/releases/download/v${KUBEBUILDER_VERSION}"
    fetch "${base}/${file}" "${tmpdir}/${file}"
    fetch "${base}/checksums.txt" "${tmpdir}/kubebuilder-checksums.txt"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/kubebuilder-checksums.txt" "$file")"
    install_binary kubebuilder "$file"
}

install_operator-sdk() {
    local file="operator-sdk_linux_${arch}"
    local base="https://github.com/operator-framework/operator-sdk/releases/download/v${OPERATOR_SDK_VERSION}"
    fetch "${base}/${file}" "${tmpdir}/${file}"
    fetch "${base}/checksums.txt" "${tmpdir}/operator-sdk-checksums.txt"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/operator-sdk-checksums.txt" "$file")"
    install_binary operator-sdk "$file"
}

install_opm() {
    local file="linux-${arch}-opm"
    local base="https://github.com/operator-framework/operator-registry/releases/download/v${OPM_VERSION}"
    fetch "${base}/${file}" "${tmpdir}/${file}"
    fetch "${base}/checksums.txt" "${tmpdir}/opm-checksums.txt"
    verify "$tmpdir" "$file" "$(sum_from "${tmpdir}/opm-checksums.txt" "$file")"
    install_binary opm "$file"
}

if [[ $# -eq 0 ]]; then
    set -- "${TOOLS[@]}"
fi
for tool in "$@"; do
    if ! declare -F "install_${tool}" >/dev/null; then
        echo "unknown tool: ${tool} (known: ${TOOLS[*]})" >&2
        exit 2
    fi
    echo "==> ${tool}"
    "install_${tool}"
done
