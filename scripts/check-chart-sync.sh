#!/usr/bin/env bash
# Fails when the Helm chart's generated resources drifted from config/: the
# CRDs and the manager ClusterRole rules. kubebuilder's helm/v2-alpha plugin
# generated charts/chart from `make build-installer`; the chart is hand-edited
# since (values, image helper), so a controller-gen change (a new field, a new
# RBAC marker) must be carried over by regenerating into a scratch directory
# and merging, as docs/TEMPLATE.md describes. This catches the forgetting.
#
# Usage: check-chart-sync.sh   (needs helm and kustomize on PATH)
set -euo pipefail

cd "$(dirname "$0")/.."
chart=charts/chart
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# strip: drop helm's "# Source:" comments and document separators.
strip() { grep -v -e '^# Source:' -e '^---$' -e 'helm.sh/resource-policy' || true; }

helm template t "$chart" --set crd.keep=false | strip >"$tmp/chart-all.yaml"
kustomize build config/crd | grep -v '^---$' >"$tmp/crd.yaml"

# The chart renders its CRD documents in template order; every CRD document
# of config/crd must appear verbatim in that output.
status=0
python3 -I - "$tmp/crd.yaml" "$tmp/chart-all.yaml" <<'PY' || status=1
import sys

crd = open(sys.argv[1]).read().strip()
chart = open(sys.argv[2]).read()


def normalise(text):
    return "\n".join(line.rstrip() for line in text.splitlines() if line.strip())


if normalise(crd) not in normalise(chart):
    print("error: the chart's CustomResourceDefinition differs from config/crd; regenerate it (docs/TEMPLATE.md, 'Helm chart')", file=sys.stderr)
    sys.exit(1)
PY

# The manager role's rules.
rules() { sed -n '/^rules:/,$p' | grep -v -e '^---$' -e '^$'; }
helm template t "$chart" -s templates/rbac/manager-role.yaml | rules >"$tmp/chart-rules.yaml"
rules <config/rbac/role.yaml >"$tmp/role-rules.yaml"
if ! diff -u "$tmp/role-rules.yaml" "$tmp/chart-rules.yaml"; then
    echo "error: the chart's manager ClusterRole rules differ from config/rbac/role.yaml; regenerate the chart (docs/TEMPLATE.md, 'Helm chart')" >&2
    status=1
fi
exit "$status"
