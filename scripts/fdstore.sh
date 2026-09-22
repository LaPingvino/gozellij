#!/usr/bin/env bash
#
# Does the daemon keep its listening socket across a restart?
#
# Not part of scripts/acceptance.sh, and it cannot be: that script starts the daemon itself under
# `env -i`, with no service manager anywhere, which is deliberate - it is how TERM=dumb was found.
# This one needs the opposite, a real systemd user manager, so it is its own script.
#
#   ./scripts/fdstore.sh
#
# It runs the daemon as a *transient* unit under a throwaway runtime and state directory, so it
# touches nothing you are using and leaves no unit file behind.
set -u

repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
unit="gozellij-fdstore-$$"
pass=0
fail=0

ok()  { printf '  PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }
say() { printf '%s\n' "${1-}"; }

cleanup() {
    systemctl --user stop "$unit" 2>/dev/null
    systemctl --user clean --what=fdstore "$unit" 2>/dev/null
    systemctl --user reset-failed "$unit" 2>/dev/null
    rm -rf "$work"
}
trap cleanup EXIT

if ! systemctl --user is-system-running >/dev/null 2>&1; then
    say "no systemd user manager here; nothing to check"
    exit 0
fi

mkdir -p "$work/run" "$work/state" "$work/home" "$work/bin"
say "building from $repo"
( cd "$repo" && go build -o "$work/bin/gozellijd" ./cmd/gozellijd && go build -o "$work/bin/gozellij" ./cmd/gozellij ) || {
    say "build failed"; exit 1
}

export GOZELLIJ_RUNTIME_DIR="$work/run" GOZELLIJ_STATE_DIR="$work/state"
gz="$work/bin/gozellij"

# The settings under test, and the ones the packaged unit carries. KillMode=process is the one the
# whole idea turns on; see docs/USER_STORIES.md C3.
systemctl --user reset-failed "$unit" 2>/dev/null
# TimeoutStartSec above is why this reports rather than hangs: a daemon that never sends READY=1
# leaves Type=notify waiting for ninety seconds by default, and a check that takes a minute and a
# half to say "it did not start" is a check nobody runs twice.
if ! systemd-run --user --unit="$unit" --collect \
    -p Type=notify -p NotifyAccess=main \
    -p TimeoutStartSec=10 \
    -p Restart=on-failure -p RestartSec=1 \
    -p FileDescriptorStoreMax=64 -p KillMode=process \
    -p Environment="GOZELLIJ_RUNTIME_DIR=$work/run" \
    -p Environment="GOZELLIJ_STATE_DIR=$work/state" \
    -p Environment="HOME=$work/home" \
    "$work/bin/gozellijd" >/dev/null 2>&1; then
    say "  FAIL  the unit never started: $(systemctl --user show "$unit" -p Result --value)"
    journalctl --user -u "$unit" --no-pager -o cat --since "-30s" | tail -5
    exit 1
fi

# Type=notify means systemd waits for READY=1, so the socket is there when start returns. If the
# daemon never sends it the unit start times out, which is itself the check.
if [ "$(systemctl --user show "$unit" -p ActiveState --value)" = "active" ]; then
    ok "the unit reaches active, so READY=1 arrived (Type=notify)"
else
    bad "the unit is $(systemctl --user show "$unit" -p ActiveState --value); READY=1 never arrived"
    journalctl --user -u "$unit" --no-pager -o cat --since "-30s" | tail -5
    exit 1
fi

if "$gz" ping >/dev/null 2>&1; then
    ok "the daemon answers on its socket"
else
    bad "the daemon does not answer"
fi

if [ "$(systemctl --user show "$unit" -p NFileDescriptorStore --value)" = "1" ]; then
    ok "systemd is holding one descriptor, which is the listening socket"
else
    bad "systemd is holding $(systemctl --user show "$unit" -p NFileDescriptorStore --value) descriptors, want 1"
fi

# The socket's identity, not just its path: a fresh socket at the same name would pass a path check
# and fail this one.
before=$(stat -c '%i' "$work/run/fabric.sock" 2>/dev/null)

# A crash, not a stop: kill -9 the main process so Restart=on-failure brings it back.
main=$(systemctl --user show "$unit" -p MainPID --value)
kill -9 "$main" 2>/dev/null
for _ in $(seq 40); do
    now=$(systemctl --user show "$unit" -p MainPID --value)
    [ -n "$now" ] && [ "$now" != "0" ] && [ "$now" != "$main" ] && break
    sleep 0.25
done
after_pid=$(systemctl --user show "$unit" -p MainPID --value)
if [ -n "$after_pid" ] && [ "$after_pid" != "$main" ] && [ "$after_pid" != "0" ]; then
    ok "the daemon came back after being killed (pid $main then $after_pid)"
else
    bad "the daemon did not come back: MainPID is [$after_pid]"
    journalctl --user -u "$unit" --no-pager -o cat --since "-30s" | tail -8
    exit 1
fi

after=$(stat -c '%i' "$work/run/fabric.sock" 2>/dev/null)
if [ -n "$before" ] && [ "$before" = "$after" ]; then
    ok "it is the same socket file, not a new one at the same name"
else
    bad "the socket was replaced: inode $before then $after"
fi

if journalctl --user -u "$unit" --no-pager -o cat --since "-60s" | grep -q "took the listening socket back from systemd"; then
    ok "the daemon says it took the socket back rather than making one"
else
    bad "the daemon made a new socket instead of adopting the stored one"
    journalctl --user -u "$unit" --no-pager -o cat --since "-60s" | tail -6
fi

if "$gz" ping >/dev/null 2>&1; then
    ok "and it answers on the socket it inherited"
else
    bad "the daemon does not answer after the restart"
fi

say
if [ "$fail" -eq 0 ]; then
    say "all $pass held."
else
    say "$pass held, $fail did not."
fi
exit $(( fail > 0 ? 1 : 0 ))
