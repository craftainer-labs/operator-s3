# Per-instance settings for the image release contract in ../Makefile
# (`-include image.mk`). The Makefile itself is template-docker's and stays
# byte-identical; this file is instance-owned (`ignore` tier), so what an
# instance changes lives here and in operator.mk's variables.

# The manager image is built from the root Dockerfile's `runner` stage (a Go
# static binary on distroless static:nonroot), for both arches.
DOCKERFILE := Dockerfile
TARGET := runner
ARCHES := amd64 arm64

# Stamp the build with the release version and commit (Dockerfile ARGs).
BUILD_ARGS = --build-arg APP_VERSION=$(RELEASE_VERSION) --build-arg APP_COMMIT=$(shell git rev-parse HEAD)

# org.opencontainers.image.description and licence (GPL-3.0-only is the
# template's; the instance picks its own, see docs/TEMPLATE.md).
OCI_DESCRIPTION := A Kubernetes operator built from craftainer/template-k8s-operator
OCI_LICENSE := GPL-3.0-only

# The coverage report is not published with a release of this template.
# COVERAGE_REPORT :=

# image-smoke loads the release image tar, runs it and polls the URL until it
# answers 2xx. The manager would need an API server, so the smoke test runs
# it in --probe-only mode (MANAGER_PROBE_ONLY, see internal/app): the same
# binary and image, serving only the probes. It also runs the image under an
# arbitrary UID, as OpenShift's restricted SCC does.
IMAGE_SMOKE_PORT := 8081
IMAGE_SMOKE_PATH := /healthz
IMAGE_SMOKE_RUN_ARGS := -e MANAGER_PROBE_ONLY=true --user 12345:0 --read-only --cap-drop ALL --security-opt no-new-privileges
IMAGE_SMOKE_TIMEOUT := 60

# The operator's own targets (code generation, tests, Helm, OLM bundle).
include operator.mk

# release-check, on top of the image smoke test: lint the chart, validate the
# OLM bundle (operator-sdk) and the file-based catalog (opm), and check the
# generated files are current. Prerequisites only, never a recipe on
# release-check (the Makefile owns that target).
release-check: helm-lint bundle-validate generated-current

.PHONY: generated-current
generated-current:
	scripts/check-generated.sh
