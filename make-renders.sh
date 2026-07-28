#!/usr/bin/env bash
# Draw every view of proj into docs/model/img, then check the pictures.
set -euo pipefail
cd "$(dirname "$0")"
go run ./docs/model/render "$@"

# The check reads the renders that were just drawn: a state nothing renders, a
# render that drew nothing, two states that came out as one picture. Findings
# are advisory, so a redraw started from the book's own button reports them
# instead of failing where nobody can read an exit status.
if command -v viewbook >/dev/null 2>&1; then
  viewbook --gaps docs/model
fi
