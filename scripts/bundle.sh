#!/usr/bin/env bash
# Generates the OLM bundle (./bundle and ./bundle.Dockerfile) for one image
# and version, then validates it with operator-sdk.
#
# Usage: bundle.sh IMAGE VERSION CHANNELS DEFAULT_CHANNEL
#   IMAGE            the manager image, by digest: registry/name@sha256:<64 hex>
#                    (a bundle must not point at a mutable tag; the released
#                    image's index digest is known only after `finalize`)
#   VERSION          the bundle's semantic version, without a leading "v"
#   CHANNELS         comma-separated OLM channels the bundle belongs to
#   DEFAULT_CHANNEL  the package's default channel
#
# The kustomize inputs (config/) are copied first, because pointing the
# manager at IMAGE edits config/manager/kustomization.yaml, which is committed
# and must stay as it is. `--use-image-digests` makes operator-sdk list the
# image in spec.relatedImages (what "disconnected" in the CSV's
# infrastructure-features annotation promises).
set -euo pipefail

if [[ $# -ne 4 ]]; then
    echo "usage: $0 IMAGE VERSION CHANNELS DEFAULT_CHANNEL" >&2
    exit 2
fi
image="$1"
version="$2"
channels="$3"
default_channel="$4"

if [[ ! "$image" =~ ^[a-z0-9][a-z0-9./_:-]*@sha256:[0-9a-f]{64}$ ]]; then
    echo "error: IMAGE must be a digest reference (registry/name@sha256:<64 hex>), got '$image'" >&2
    exit 1
fi
if [[ ! "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
    echo "error: VERSION must be a semantic version without a leading v, got '$version'" >&2
    exit 1
fi

cd "$(dirname "$0")/.."
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cp -R config "$tmp/config"
(cd "$tmp/config/manager" && kustomize edit set image "controller=${image}")

rm -rf bundle bundle.Dockerfile
kustomize build "$tmp/config/manifests" |
    operator-sdk generate bundle -q --overwrite \
        --kustomize-dir "$tmp/config/manifests" \
        --version "$version" --channels "$channels" --default-channel "$default_channel" \
        --use-image-digests
operator-sdk bundle validate ./bundle --select-optional suite=operatorframework
