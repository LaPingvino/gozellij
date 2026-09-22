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

# ---------------------------------------------------------------- and now the point of all this
#
# A service started before the crash, still running afterwards, with the same pid. This is the
# promise C3 is about: "the daemon hits a nil pointer, or the OOM killer. Synapse should not care."
say
"$gz" add survivor -start -restart always -- sh -c 'i=0; while :; do echo "SURVIVOR-$i"; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
sleep 2
svcpid=$("$gz" status survivor 2>/dev/null | awk '/^pid:/{print $2}')
if [ -n "$svcpid" ] && [ "$svcpid" != "0" ]; then
    ok "a service is running before the crash (pid $svcpid)"
else
    bad "the service never started, so there is nothing to lose"
    exit 1
fi

if [ "$(systemctl --user show "$unit" -p NFileDescriptorStore --value)" -ge 2 ]; then
    ok "systemd is holding the service's terminal as well as the socket"
else
    bad "systemd holds $(systemctl --user show "$unit" -p NFileDescriptorStore --value) descriptors; want the socket and one terminal"
fi

main=$(systemctl --user show "$unit" -p MainPID --value)
kill -9 "$main" 2>/dev/null
for _ in $(seq 40); do
    now=$(systemctl --user show "$unit" -p MainPID --value)
    [ -n "$now" ] && [ "$now" != "0" ] && [ "$now" != "$main" ] && break
    sleep 0.25
done
sleep 2

after_svc=$("$gz" status survivor 2>/dev/null | awk '/^pid:/{print $2}')
if [ -n "$after_svc" ] && [ "$after_svc" = "$svcpid" ]; then
    ok "the service kept its pid across the daemon being killed"
else
    bad "the service's pid went $svcpid then [$after_svc] - it was restarted, not recovered"
fi

# Alive, not merely reported alive: its output has to still be arriving through the terminal the
# new daemon inherited. A pid that matches proves the process; this proves the pipe.
first=$("$gz" logs survivor 2>/dev/null | tail -1)
sleep 3
second=$("$gz" logs survivor 2>/dev/null | tail -1)
if [ -n "$first" ] && [ "$first" != "$second" ]; then
    ok "and its output is still arriving through the terminal that was handed back"
else
    bad "the recovered service is not producing output: [$first] then [$second]"
fi

# Stopping it has to work too. A recovered process is not this daemon's child, so the ordinary
# kill-and-wait cannot be what happens.
"$gz" stop survivor >/dev/null 2>&1
sleep 2
if kill -0 "$svcpid" 2>/dev/null; then
    bad "the recovered service is still running after stop"
else
    ok "a recovered service can still be stopped"
fi
"$gz" rm survivor >/dev/null 2>&1

say
if [ "$fail" -eq 0 ]; then
    say "all $pass held."
else
    say "$pass held, $fail did not."
fi
exit $(( fail > 0 ? 1 : 0 ))
