# syntax=docker/dockerfile:1.28
# The manager image: a static Go binary on distroless (builder -> runner).
# `runner` is the release stage (image.mk's TARGET). Both bases are pinned by
# digest as well as tag (the digest is of the multi-arch index); Renovate's
# dockerfile manager bumps tag and digest together.
#
# The runner has no shell, no package manager and no home directory to write
# to; the binary is static (CGO_ENABLED=0) and writes nothing, so the image
# runs under any UID the platform assigns (e.g. OpenShift's restricted SCC,
# `docker run --user 12345`) as well as the image's own nonroot 65532.

# Also declared in .devcontainer/Dockerfile; scripts/check-dockerfile-versions.sh
# keeps the two equal.
ARG DEBIAN_VERSION=trixie

# Keep in step with scripts/develop.d/10-go.sh's GO_VERSION and go.mod's
# toolchain line (scripts/check-go-version.sh checks all three).
# renovate: datasource=docker depName=golang
ARG GO_VERSION=1.27.3

########################################
# builder -- compiles the manager. Runs on the build host's platform and
# cross-compiles, so a multi-arch build needs no emulation.
########################################
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-${DEBIAN_VERSION}@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS builder
ARG TARGETOS
ARG TARGETARCH
# GOFIPS140 selects the Go cryptographic module the binary links (EB-14):
# pass --build-arg GOFIPS140=v1.0.0 for the frozen, validated one, and run
# with GODEBUG=fips140=on to enforce it. Empty keeps Go's default.
ARG GOFIPS140=

WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

# -trimpath and -s -w: reproducible, small; the module cache is a build cache
# only. GOFLAGS=-mod=readonly fails the build if go.mod/go.sum are not tidy.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOFIPS140=${GOFIPS140:-} GOFLAGS=-mod=readonly \
    go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd

########################################
# runner -- the image that ships.
########################################
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99 AS runner
# Set by `make build` through BUILD_ARGS in image.mk; empty otherwise.
ARG APP_VERSION=
ARG APP_COMMIT=
ENV APP_VERSION=${APP_VERSION} \
    APP_COMMIT=${APP_COMMIT}

COPY --from=builder /out/manager /manager

# 8081 probes, 8443 metrics (HTTPS, authenticated), 9443 webhooks.
EXPOSE 8081 8443 9443
USER 65532:65532
ENTRYPOINT ["/manager"]
