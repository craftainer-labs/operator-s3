#!/usr/bin/env bash
# The end-to-end run: build the image, create a kind cluster, install
# cert-manager, load the image, run the Chainsaw tests (test/e2e), and always
# tear the cluster down. The Chainsaw test installs the Helm chart itself, so
# the chart that ships is the chart that is tested.
#
# Topology (docs/adrs/0002): one kind cluster per run, named per run, on the
# pinned node image; backends an instance's e2e needs run as containers on the
# `kind` Docker network and are reached by container name (the sample API
# needs none). Teardown runs `kind delete cluster` and `docker network rm kind`.
# CI runs this same script on a runner (.github/workflows/e2e.yml).
#
# Needs docker, kind, kubectl, helm and chainsaw on PATH
# (scripts/develop.d/20-k8s-tools.sh installs them).
#
# Environment (all optional):
#   KIND_CLUSTER   cluster name; default <repo>-e2e-<pid>
#   IMG            image to build and load; default <repo>:e2e
#   E2E_SKIP_BUILD set to skip the docker build (IMG must exist)
#   E2E_KEEP       set to keep the cluster for debugging (you tear it down)
set -euo pipefail

cd "$(dirname "$0")/.."

# kindest/node for Kubernetes 1.37.0, by digest (kind v0.33.0's release notes).
KIND_NODE_IMAGE="kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
# renovate: datasource=github-releases depName=cert-manager/cert-manager
CERT_MANAGER_VERSION="v1.21.3"
CERT_MANAGER_SHA256="c04a51295adda2b1a775146751f973e787190778c7f35180861d9678876e5c74"

repo="$(basename "$PWD" | tr '[:upper:]' '[:lower:]')"
cluster="${KIND_CLUSTER:-${repo}-e2e-$$}"
img="${IMG:-${repo}:e2e}"
workdir="$(mktemp -d)"

cleanup() {
    status=$?
    set +e
    if [[ -z "${E2E_KEEP:-}" ]]; then
        kind delete cluster --name "$cluster"
        # The `kind` network is shared by every cluster and lane container;
        # remove it only when nothing is attached (docker refuses otherwise).
        docker network rm kind >/dev/null 2>&1
    else
        echo "e2e: keeping cluster $cluster (E2E_KEEP set); remove it with: kind delete cluster --name $cluster; docker network rm kind"
    fi
    rm -rf "$workdir"
    exit "$status"
}
trap cleanup EXIT

if [[ -z "${E2E_SKIP_BUILD:-}" ]]; then
    docker build --target runner -t "$img" .
fi

kind create cluster --name "$cluster" --image "$KIND_NODE_IMAGE" --wait 180s
kubectl config use-context "kind-${cluster}"

kind load docker-image "$img" --name "$cluster"

manifest="${workdir}/cert-manager.yaml"
curl -LsSf --retry 3 --retry-all-errors -o "$manifest" \
    "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
echo "${CERT_MANAGER_SHA256}  ${manifest}" | sha256sum -c -
kubectl apply -f "$manifest"
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=300s

# The chart's image: the local one, loaded into the node, never pulled.
image_repository="${img%:*}"
image_tag="${img##*:}"
export E2E_IMAGE_REPOSITORY="$image_repository" E2E_IMAGE_TAG="$image_tag" E2E_CHART_DIR="$PWD/charts/chart"

chainsaw test test/e2e --config test/e2e/chainsaw-config.yaml
