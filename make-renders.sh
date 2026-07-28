#!/usr/bin/env bash
# Draw every view of proj into docs/model/img.
set -euo pipefail
cd "$(dirname "$0")"
exec go run ./docs/model/render "$@"
