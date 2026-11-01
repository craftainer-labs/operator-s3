#!/usr/bin/env bash
# Fails when the committed generated files are stale: runs `make manifests
# generate` (controller-gen: CRDs, RBAC role, webhook configuration, DeepCopy)
# and compares checksums of the generated paths before and after. Unlike
# `git diff`, it works on an uncommitted tree, so it can run as a hook. On
# failure the regenerated files stay in place, ready to commit.
set -euo pipefail

cd "$(dirname "$0")/.."

# digest: a checksum over the generated files (their names too).
digest() {
    find config/crd/bases config/rbac/role.yaml config/webhook/manifests.yaml api -type f \( -name '*.yaml' -o -name 'zz_generated.*' \) -print0 |
        sort -z | xargs -0 sha256sum | sha256sum
}

before="$(digest)"
make --no-print-directory -s manifests generate >/dev/null
after="$(digest)"
if [[ "$before" != "$after" ]]; then
    echo "error: generated files were stale; 'make manifests generate' has updated them, review and commit" >&2
    exit 1
fi
echo "generated files are current"
