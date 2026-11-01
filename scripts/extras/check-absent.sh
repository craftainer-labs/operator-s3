#!/usr/bin/env bash
# Refuses to publish over an existing OCI tag. A tag in a registry is mutable:
# pushing the chart, the bundle or the catalog again under the same version
# gives it a different digest, and the signatures and attestations made for
# the first push then point at a digest the tag no longer names. So a
# re-dispatch for a tag that was already published must stop, not re-push.
#
# With --resume (release-extras.yml's `resume` input) an existing tag is not
# an error: its digest is printed as `<label>_digest=<sha256:...>` (for
# $GITHUB_OUTPUT) and the workflow then never pushes that artifact again, it
# only signs and attests the digest. Signing an existing digest is additive.
#
# Usage: check-absent.sh [--resume] <version> <label=ghcr.io/owner/name>...
# Reads: GITHUB_TOKEN (pull access to the packages, e.g. the workflow's token).
# Exit: 0 if no tag exists (or --resume); 1 if a tag exists without --resume,
# or the registry's answer is not a plain exists / does not exist; 2 on usage.
set -euo pipefail

resume=false
if [[ ${1:-} == --resume ]]; then
    resume=true
    shift
fi
if [[ $# -lt 2 ]]; then
    echo "usage: $0 [--resume] <version> <label=ghcr.io/owner/name>..." >&2
    exit 2
fi
: "${GITHUB_TOKEN:?GITHUB_TOKEN is required}"

version="$1"
shift
accept="application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

status=0
for arg in "$@"; do
    label="${arg%%=*}"
    ref="${arg#*=}"
    if [[ $label == "$arg" || ! $label =~ ^[a-z_]+$ || $ref != ghcr.io/* ]]; then
        echo "::error::expected <label>=ghcr.io/<owner>/<name>, got '$arg'" >&2
        exit 2
    fi
    path="${ref#ghcr.io/}"
    bearer=$(curl -sSf -u "token:${GITHUB_TOKEN}" "https://ghcr.io/token?scope=repository:${path}:pull&service=ghcr.io" |
        python3 -I -c 'import json, sys; print(json.load(sys.stdin)["token"])')
    answer=$(curl -sS -I -w '\nHTTP_CODE=%{http_code}\n' -H "Authorization: Bearer ${bearer}" -H "Accept: ${accept}" \
        "https://ghcr.io/v2/${path}/manifests/${version}")
    code=$(sed -n 's/^HTTP_CODE=//p' <<<"$answer")
    case "$code" in
        200)
            if [[ $resume == true ]]; then
                digest=$(tr -d '\r' <<<"$answer" | sed -n -E 's/^[Dd]ocker-[Cc]ontent-[Dd]igest:[[:space:]]*(sha256:[0-9a-f]{64})[[:space:]]*$/\1/p' | head -n1)
                if [[ -z $digest ]]; then
                    echo "::error::${ref}:${version} exists but the registry gave no digest" >&2
                    status=1
                else
                    echo "${ref}:${version} exists (${digest}); it will be signed, not pushed" >&2
                    echo "${label}_digest=${digest}"
                fi
            else
                echo "::error::${ref}:${version} already exists; refusing to re-push a published tag (dispatch with resume=true to only sign and attest it, or cut a new release)" >&2
                status=1
            fi
            ;;
        404) ;;
        *)
            echo "::error::cannot tell whether ${ref}:${version} exists (HTTP ${code})" >&2
            status=1
            ;;
    esac
done
exit "$status"
