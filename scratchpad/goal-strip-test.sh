#!/usr/bin/env bash
# Goal-persistence test. Run ONLY while the proj claude session is NOT running
# (quit it first), otherwise the live process re-appends the sentinel on its
# next Stop and the strip is undone.
#
# Removes every /goal state record from the transcript:
#   - goal_status attachment records (hold {sentinel:true, condition:"..."} )
#   - isMeta "session-scoped Stop hook is now active" records
# Then start `claude -c` in the proj session and check whether the goal is
# still active. Gone => transcript is the store. Still there => another store.
set -euo pipefail

f="$HOME/.claude/projects/-home-marius-projects-code-proj/79d275dd-4500-43da-9d2e-5ee84c40b32b.jsonl"
[ -f "$f" ] || { echo "transcript not found: $f" >&2; exit 1; }

# refuse to run if the session process is alive (would re-append)
if pgrep -f 'remote-control proj @pc0' >/dev/null; then
  echo "REFUSING: proj claude session still running. Quit it first, then re-run." >&2
  exit 1
fi

cp "$f" "$f.pre-strip.bak"
before=$(grep -c '"attachment":{"type":"goal_status"' "$f" || true)

# Drop the two record kinds. Both patterns are the serialized on-disk forms,
# absent from ordinary prose/tool lines.
grep -v '"attachment":{"type":"goal_status"' "$f" \
  | grep -v '"isMeta":true.*session-scoped Stop hook is now active with condition' \
  > "$f.new"
mv "$f.new" "$f"

after=$(grep -c '"attachment":{"type":"goal_status"' "$f" || true)
echo "goal_status attachments: before=$before after=$after"
echo "backup: $f.pre-strip.bak"
echo "now start: claude -c   (in the proj session) and check for the /goal indicator"
