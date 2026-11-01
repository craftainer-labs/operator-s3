#!/usr/bin/env bash
# Prints the OCI references release-extras.yml publishes to, as key=value
# lines (for $GITHUB_OUTPUT). They derive from the same variables the image
# release (the Makefile's `publish`) uses, so every artifact sits next to the
# image:
#   image      <registry>/<name>             (published by release.yml)
#   bundle     <registry>/<name>-bundle      (OLM bundle image)
#   catalog    <registry>/<name>-catalog     (file-based catalog image)
#   chart_repo oci://<registry>/<owner>/charts   (what `helm push` takes)
#   chart      <registry>/<owner>/charts/<chart name>
# where <name> is OCI_IMAGE_NAME, or the lower-cased owner/repo.
#
# Only GHCR is supported: the extras log in, sign and attest with the
# workflow's GITHUB_TOKEN, which only ghcr.io accepts.
#
# Reads: OCI_REGISTRY (must be ghcr.io), OCI_IMAGE_NAME (optional),
# GITHUB_REPOSITORY; the chart name comes from charts/chart/Chart.yaml.
set -euo pipefail

: "${OCI_REGISTRY:?set the OCI_REGISTRY repository variable (ghcr.io)}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

if [[ $OCI_REGISTRY != ghcr.io ]]; then
    echo "::error::release-extras.yml publishes to ghcr.io only (OCI_REGISTRY is '$OCI_REGISTRY')" >&2
    exit 1
fi

cd "$(dirname "$0")/../.."
repo="$(tr '[:upper:]' '[:lower:]' <<<"$GITHUB_REPOSITORY")"
owner="${repo%%/*}"
name="$(tr '[:upper:]' '[:lower:]' <<<"${OCI_IMAGE_NAME:-$repo}")"
chart_name="$(sed -n -E 's/^name:[[:space:]]*([A-Za-z0-9._-]+)[[:space:]]*$/\1/p' charts/chart/Chart.yaml | head -n1)"
if [[ -z $chart_name ]]; then
    echo "::error::no chart name in charts/chart/Chart.yaml" >&2
    exit 1
fi

echo "image=${OCI_REGISTRY}/${name}"
echo "bundle=${OCI_REGISTRY}/${name}-bundle"
echo "catalog=${OCI_REGISTRY}/${name}-catalog"
echo "chart_repo=oci://${OCI_REGISTRY}/${owner}/charts"
echo "chart=${OCI_REGISTRY}/${owner}/charts/${chart_name}"
