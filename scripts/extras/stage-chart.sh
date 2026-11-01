#!/usr/bin/env bash
# Copies the Helm chart to a staging directory with the manager image pinned
# to a digest: values.yaml's `digest: ""` becomes the released image index's
# digest, so a published chart installs exactly the image that was released
# (and signed), whatever the tag points to later.
#
# Usage: stage-chart.sh DIGEST OUT_DIR   (DIGEST is sha256:<64 hex>)
# Writes OUT_DIR/chart/; the chart itself in charts/chart is not touched.
set -euo pipefail

if [[ $# -ne 2 ]]; then
    echo "usage: $0 DIGEST OUT_DIR" >&2
    exit 2
fi
digest="$1"
out="$2"
if [[ ! $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "error: not an image digest: '$digest'" >&2
    exit 1
fi

cd "$(dirname "$0")/../.."
rm -rf "$out/chart"
mkdir -p "$out"
cp -R charts/chart "$out/chart"

values="$out/chart/values.yaml"
if [[ "$(grep -c -E '^    digest: ""$' "$values")" -ne 1 ]]; then
    echo "error: expected exactly one '    digest: \"\"' line in charts/chart/values.yaml" >&2
    exit 1
fi
sed -i -E "s|^    digest: \"\"\$|    digest: \"${digest}\"|" "$values"
