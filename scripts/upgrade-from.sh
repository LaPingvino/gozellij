#!/usr/bin/env bash
#
# Does upgrading from an older build to this one keep everything?
#
#   ./scripts/upgrade-from.sh <old-revision>      e.g. 14ae127, the build that is installed
#
# The upgrade somebody actually does is from whatever is installed to whatever was just built, and
# that jump can span weeks of changes to the handover. Nothing else tests two *different* builds:
# the other suites upgrade a binary to itself. So this builds both, runs the old daemon under a
# systemd user unit the way the package does, attaches an old client in tmux, swaps the binary the
# way pacman does (a new file renamed over the old), and runs the new client's `gozellij upgrade`.
#
# What must hold: every service keeps its pid, and the terminal attached with the old client comes
# back to the same shell - checked with a variable set in it before the upgrade.
set -u
old="${1:?usage: $0 <old-revision>}"
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/gz-upgrade.XXXXXX")
unit="gz-upgrade-$$"
tmuxSock="gz-upgrade-$$"
pass=0 fail=0
ok()  { printf '  PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }

# This runs from inside gozellij, often enough: none of the caller's multiplexer or systemd
# variables may reach the daemons started here.
unset GOZELLIJ ZELLIJ ZELLIJ_SESSION_NAME TMUX STY NOTIFY_SOCKET LISTEN_FDS LISTEN_PID INVOCATION_ID

cleanup() {
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    systemctl --user stop "$unit" 2>/dev/null
    systemctl --user clean --what=fdstore "$unit" 2>/dev/null
    systemctl --user reset-failed "$unit" 2>/dev/null
    for wt in "$work/wt-old" "$work/wt-new"; do
        git -C "$repo" worktree remove --force "$wt" 2>/dev/null
    done
    rm -rf "$work"
}
trap cleanup EXIT

if ! systemctl --user is-system-running >/dev/null 2>&1; then
    echo "no systemd user manager here; nothing to check"
    exit 0
fi

build() { # rev dir
    git -C "$repo" worktree add -f --detach "$work/wt-$2" "$1" >/dev/null 2>&1 || { echo "cannot check out $1"; exit 1; }
    ( cd "$work/wt-$2" && go build -o "$work/$2/" ./cmd/... ) || { echo "build of $1 failed"; exit 1; }
}
echo "building $old and $(git -C "$repo" rev-parse --short HEAD)"
build "$old" old
build HEAD new
mkdir -p "$work/bin" "$work/run" "$work/state" "$work/home"
export GOZELLIJ_RUNTIME_DIR="$work/run" GOZELLIJ_STATE_DIR="$work/state"
cp -f "$work/old/gozellijd" "$work/bin/gozellijd"

if ! systemd-run --user --unit="$unit" --collect -p Type=notify -p NotifyAccess=main \
    -p TimeoutStartSec=10 -p Restart=on-failure -p RestartSec=1 \
    -p FileDescriptorStoreMax=64 -p KillMode=process \
    -p "ExecReload=/bin/kill -USR1 \$MAINPID" \
    -p Environment="GOZELLIJ_RUNTIME_DIR=$work/run" -p Environment="GOZELLIJ_STATE_DIR=$work/state" \
    -p Environment="HOME=$work/home" "$work/bin/gozellijd" >/dev/null 2>&1; then
    echo "  FAIL  the old daemon never started"; exit 1
fi
sleep 1

old_gz="$work/old/gozellij" new_gz="$work/new/gozellij"
"$old_gz" add shell -start -- bash --norc -i >/dev/null
"$old_gz" add ticker -start -restart always -- sh -c 'while :; do sleep 1; done' >/dev/null
sleep 1
before=$("$old_gz" ls | awk 'NR>1{print $1"="$3}' | sort | tr '\n' ' ')

tmux -L "$tmuxSock" new-session -d -x 120 -y 30 \
    -e GOZELLIJ_RUNTIME_DIR="$work/run" -e GOZELLIJ_STATE_DIR="$work/state" -e HOME="$work/home" \
    -e TERM=xterm-256color "sh -c '$old_gz attach shell; echo attach-exit=\$?; sleep 60'"
sleep 2
tmux -L "$tmuxSock" send-keys 'MARK=set-before-the-upgrade-$((6*7))' Enter
sleep 0.5

# The way pacman replaces a file: a new one renamed over it, so the running daemon's image is
# "(deleted)" and upgrade has to find the new one by path.
cp -f "$work/new/gozellijd" "$work/bin/.gozellijd.new" && mv -f "$work/bin/.gozellijd.new" "$work/bin/gozellijd"

if out=$("$new_gz" upgrade 2>&1); then
    ok "the new client upgraded the old daemon ($(echo "$out" | tail -1))"
else
    bad "upgrade failed: $out"
fi
sleep 2
after=$("$new_gz" ls | awk 'NR>1{print $1"="$3}' | sort | tr '\n' ' ')
if [ -n "$before" ] && [ "$after" = "$before" ]; then
    ok "every service kept its pid ($after)"
else
    bad "pids went [$before] to [$after]"
fi

tmux -L "$tmuxSock" send-keys 'echo "MARK is $MARK"' Enter
sleep 1
if tmux -L "$tmuxSock" capture-pane -p -J | grep -q 'MARK is set-before-the-upgrade-42'; then
    ok "the terminal attached with the old client came back to the same shell"
else
    bad "the old client is not on the same shell: $(tmux -L "$tmuxSock" capture-pane -p -J | grep -v '^$' | tail -3 | tr '\n' '|')"
fi

if [ "$fail" -eq 0 ]; then echo "all $pass held."; else echo "$pass held, $fail did not."; fi
exit $(( fail > 0 ? 1 : 0 ))
