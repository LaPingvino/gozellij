#!/usr/bin/env bash
#
# Break something on purpose, in a copy, and see which promises notice.
#
#   ./scripts/sabotage.sh internal/daemon/renderclient.go 'old text' 'new text'
#
# A check nobody has watched fail is a claim, so this project breaks things deliberately and runs
# the suite. Doing that in the working tree has an obvious hazard and a less obvious one. The
# obvious: a run that dies halfway leaves the sabotage in place. The less obvious, and the one that
# nearly cost somebody their work: the backup-and-restore window. If anyone edits the same file
# while the sabotage is in flight, the restore wins and their change is gone with no message.
#
# So: a git worktree at HEAD, entirely separate from whatever anyone has uncommitted. Break it
# there, run the suite there, throw the whole directory away. Nothing in the working tree is
# touched, and an interrupted run leaves a stale worktree rather than a broken checkout.
#
# It reports which checks failed. What it cannot tell you is whether those are the *right* ones -
# that judgement is the point of the exercise and stays with the person doing it.
set -u

if [ "$#" -lt 3 ]; then
    cat >&2 <<USAGE
usage: $0 <file> <old-text> <new-text> [suite]

  file       path within the repository, as committed at HEAD
  old-text   exact text to replace (must appear exactly once)
  new-text   what to put there
  suite      acceptance (default) or fdstore

Example:
  $0 internal/daemon/renderclient.go 's.cols, s.rows, s.reserved = cols, rows, reserved' '_ = reserved'
USAGE
    exit 2
fi

file="$1"; old="$2"; new="$3"; suite="${4:-acceptance}"
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
tree="$work/tree"

cleanup() {
    git -C "$repo" worktree remove --force "$tree" 2>/dev/null
    rm -rf "$work"
}
trap cleanup EXIT

echo "sabotaging $file in a worktree at HEAD (your working tree is not touched)"
git -C "$repo" worktree add -q --detach "$tree" HEAD || { echo "could not make a worktree" >&2; exit 1; }

# Python rather than sed: the text to replace is arbitrary and usually contains characters sed
# would treat as syntax. It also refuses when the text is not there exactly once, because a
# sabotage that silently changed nothing reports every check passing and reads as "the check does
# not work".
python3 - "$tree/$file" "$old" "$new" <<'PY' || exit 1
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
body = open(path).read()
n = body.count(old)
if n != 1:
    print(f"the text appears {n} times in {path}; it has to appear exactly once", file=sys.stderr)
    sys.exit(1)
open(path, "w").write(body.replace(old, new, 1))
PY

( cd "$tree" && go build ./... ) || { echo "the sabotaged tree does not build - fix the replacement" >&2; exit 1; }

echo "running $suite against it; this takes a few minutes"
( cd "$tree" && "./scripts/$suite.sh" > "$work/out.log" 2>&1 )
status=$?

echo
if grep -qE "^  FAIL" "$work/out.log"; then
    echo "these promises noticed:"
    grep -E "^  FAIL" "$work/out.log"
else
    echo "NOTHING FAILED. Either the sabotage did not reach anything, or the behaviour it broke"
    echo "is not checked by anything - which is worth knowing and is usually the more interesting"
    echo "of the two."
fi
echo
tail -1 "$work/out.log"
exit $status
