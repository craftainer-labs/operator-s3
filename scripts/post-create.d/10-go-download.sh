#!/usr/bin/env bash
# Pre-fetches the module graph so the first build/test works offline-ish and
# fails early if go.mod / go.sum have drifted.
set -euo pipefail

go mod download
go mod verify
