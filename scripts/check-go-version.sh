#!/usr/bin/env bash
# The Go version appears in three places that must agree: the devcontainer's
# GO_VERSION (scripts/develop.d/10-go.sh), the Dockerfile's `ARG GO_VERSION`
# (the image's builder) and go.mod's `toolchain` line. Renovate moves them in
# one group; this fails when they drift.
set -euo pipefail

cd "$(dirname "$0")/.."
develop="$(sed -n -E 's/^GO_VERSION=([0-9][0-9.]*)[[:space:]]*$/\1/p' scripts/develop.d/10-go.sh)"
docker="$(sed -n -E 's/^ARG GO_VERSION=([0-9][0-9.]*)[[:space:]]*$/\1/p' Dockerfile)"
mod="$(sed -n -E 's/^toolchain go([0-9][0-9.]*)[[:space:]]*$/\1/p' go.mod)"

status=0
for pair in "scripts/develop.d/10-go.sh:$develop" "Dockerfile:$docker" "go.mod:$mod"; do
    if [[ -z "${pair#*:}" ]]; then
        echo "error: no Go version found in ${pair%%:*}" >&2
        status=1
    fi
done
if [[ "$develop" != "$docker" || "$develop" != "$mod" ]]; then
    echo "error: Go versions differ: 10-go.sh=$develop Dockerfile=$docker go.mod=$mod" >&2
    status=1
fi
exit "$status"
