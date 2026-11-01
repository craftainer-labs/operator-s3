#!/usr/bin/env bash
# Runs the unit and envtest suites under the race detector and fails when the
# statement coverage of the hand-written code is below COVERAGE_MIN (percent,
# default 100). -coverpkg=./... counts every package's statements, including
# those a package only reaches through another package's tests, and shows a
# package with no tests at 0% instead of leaving it out. Generated code
# (zz_generated.*) and cmd/ (a wrapper around internal/app, see there) are
# left out of the total, not silently: the filter below is the only exclusion.
#
# The envtest suites need kube-apiserver and etcd. KUBEBUILDER_ASSETS names
# their directory; without it, `make envtest-assets` downloads the pinned
# release with setup-envtest.
set -euo pipefail

cd "$(dirname "$0")/.."
min="${COVERAGE_MIN:-100}"
profile="${COVERAGE_PROFILE:-coverage.out}"

if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
    KUBEBUILDER_ASSETS="$(make --no-print-directory -s envtest-assets)"
    export KUBEBUILDER_ASSETS
fi

raw="${profile}.raw"
go test -race -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$raw" ./...
grep -v -e '/zz_generated\.' -e '/cmd/' "$raw" > "$profile"
rm -f "$raw"

total="$(go tool cover -func="$profile" | awk '/^total:/ { sub("%", "", $NF); print $NF }')"
if [[ -z "$total" ]]; then
    echo "error: no coverage total in $profile" >&2
    exit 1
fi
if awk -v t="$total" -v m="$min" 'BEGIN { exit !(t + 0 >= m + 0) }'; then
    echo "OK: coverage ${total}% >= ${min}%"
else
    echo "error: coverage ${total}% is below the ${min}% gate (COVERAGE_MIN)" >&2
    go tool cover -func="$profile" | grep -v '100.0%' >&2 || true
    exit 1
fi
