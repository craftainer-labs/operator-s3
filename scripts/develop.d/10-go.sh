#!/usr/bin/env bash
# Installs the Go toolchain (pinned, sha256-verified) and the gate tools the
# pre-commit hooks call: golangci-lint (v2) and govulncheck. Runs as root
# during the `develop` stage build (see README.md here).
#
# Keep GO_VERSION in step with go.mod's `toolchain` directive: a mismatch
# makes the devcontainer and any other Go on the path disagree on the
# compiler (a go.mod newer than the installed Go downloads its own).
set -euo pipefail

# renovate: datasource=golang-version depName=go
GO_VERSION=1.27.3
# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION=2.15.0
# renovate: datasource=go depName=golang.org/x/vuln
GOVULNCHECK_VERSION=v1.9.0

# A C compiler for `go test -race` (the race detector needs cgo) and for any
# dependency that uses cgo.
if ! command -v cc >/dev/null; then
    apt-get update
    apt-get install -y --no-install-recommends build-essential
    apt-get clean
    rm -rf /var/lib/apt/lists/*
fi

arch="$(dpkg --print-architecture)"
case "$arch" in
    amd64 | arm64) ;;
    *)
        echo "unsupported architecture for the Go tools: $arch" >&2
        exit 1
        ;;
esac
tmpdir="$(mktemp -d)"
trap 'chmod -R u+w "$tmpdir" 2>/dev/null || true; rm -rf "$tmpdir"' EXIT

# The Go toolchain: the tarball comes from dl.google.com, its SHA-256 from
# go.dev's download index (a different host), so a tampered mirror of the
# tarball alone fails the check.
go_file="go${GO_VERSION}.linux-${arch}.tar.gz"
curl -LsSf -o "${tmpdir}/${go_file}" "https://dl.google.com/go/${go_file}"
go_sha="$(curl -LsSf 'https://go.dev/dl/?mode=json&include=all' \
    | python3 -c 'import json, sys
for rel in json.load(sys.stdin):
    for f in rel["files"]:
        if f["filename"] == sys.argv[1]:
            print(f["sha256"]); sys.exit(0)
sys.exit("no checksum listed for " + sys.argv[1])' "$go_file")"
echo "${go_sha}  ${tmpdir}/${go_file}" | sha256sum -c -
rm -rf /usr/local/go
tar -xzf "${tmpdir}/${go_file}" -C /usr/local
for bin in go gofmt; do
    ln -sf "/usr/local/go/bin/${bin}" "/usr/local/bin/${bin}"
done

# golangci-lint as a release binary; the release publishes one checksums file.
gl_dir="golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${arch}"
gl_base="https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}"
curl -LsSf -o "${tmpdir}/${gl_dir}.tar.gz" "${gl_base}/${gl_dir}.tar.gz"
curl -LsSf -o "${tmpdir}/checksums.txt" "${gl_base}/golangci-lint-${GOLANGCI_LINT_VERSION}-checksums.txt"
# Exact name match: the file also lists "<name>.tar.gz.sbom.json".
(cd "$tmpdir" && awk -v f="${gl_dir}.tar.gz" '$2 == f' checksums.txt | sha256sum -c -)
tar -xzf "${tmpdir}/${gl_dir}.tar.gz" -C "$tmpdir"
install -m755 "${tmpdir}/${gl_dir}/golangci-lint" /usr/local/bin/golangci-lint

# govulncheck has no release binaries: build the pinned module version. The
# module proxy and checksum database verify the source (go.sum-style), and
# the build uses throwaway GOPATH/GOCACHE so none of it stays in the image.
GOBIN=/usr/local/bin GOPATH="${tmpdir}/gopath" GOCACHE="${tmpdir}/gocache" \
    GOFLAGS=-mod=mod CGO_ENABLED=0 \
    go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
