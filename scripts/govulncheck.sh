#!/usr/bin/env bash
# govulncheck for the operator. Default mode `binary`: builds the manager the
# way the image does (static, CGO off) and scans that binary, so what is
# checked is what ships. `GOVULNCHECK_MODE=source` runs `govulncheck ./...`
# instead, which also covers test-only code; its call-graph analysis of
# client-go and its dependencies needs more than 2 GB of memory (it was
# OOM-killed in a 2 GB container), which is why binary mode is the default.
# Both fail only for a vulnerability the code actually calls.
set -euo pipefail

cd "$(dirname "$0")/.."
mode="${GOVULNCHECK_MODE:-binary}"

case "$mode" in
    binary)
        tmp="$(mktemp -d)"
        trap 'rm -rf "$tmp"' EXIT
        CGO_ENABLED=0 go build -trimpath -o "$tmp/manager" ./cmd
        govulncheck -mode=binary "$tmp/manager"
        ;;
    source)
        govulncheck ./...
        ;;
    *)
        echo "error: GOVULNCHECK_MODE must be binary or source, got '$mode'" >&2
        exit 2
        ;;
esac
