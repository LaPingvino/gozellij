#!/usr/bin/env bash
#
# Does the daemon keep its listening socket across a restart?
#
# A script and not a Go test because it needs a real systemd user manager: the daemon runs as a
# transient unit, crashes for real, and is started again by systemd with what it stored.
#
#   ./scripts/fdstore.sh
#
# It runs the daemon as a *transient* unit under a throwaway runtime and state directory, so it
# touches nothing you are using and leaves no unit file behind.
set -u

# These scripts test gozellij from the outside, and may be run from inside it - from a gozellij
# shell, which is where anybody using it day to day will run them. Every pane they start would
# inherit GOZELLIJ from that shell, and `gozellij attach` now refuses to nest, so every attach in
# the suite would be refused as nesting. The same goes for the multiplexers the login checks
# already clear by hand.
unset GOZELLIJ ZELLIJ ZELLIJ_SESSION_NAME TMUX STY

repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
unit="gozellij-fdstore-$$"
tmuxSock="gozellij-fdstore-tmux-$$"
pass=0
fail=0

ok()  { printf '  PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }
say() { printf '%s\n' "${1-}"; }

cleanup() {
    systemctl --user stop "$unit" 2>/dev/null
    systemctl --user clean --what=fdstore "$unit" 2>/dev/null
    systemctl --user reset-failed "$unit" 2>/dev/null
    tmux -L "$tmuxSock" kill-server 2>/dev/null
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

# Waited for rather than read once. journald ingests asynchronously, and reading the log the
# instant the daemon comes back failed one run in three while the inode check beside it passed -
# a flake that says "the socket was not adopted" about a socket that was. A check that cries wolf
# is worse than no check, because the next person to see it assumes the same.
said=""
for _ in $(seq 20); do
    if journalctl --user -u "$unit" --no-pager -o cat --since "-120s" | grep -q "took the listening socket back from systemd"; then
        said=yes
        break
    fi
    sleep 0.5
done
if [ -n "$said" ]; then
    ok "the daemon says it took the socket back rather than making one"
else
    bad "the daemon made a new socket instead of adopting the stored one"
    journalctl --user -u "$unit" --no-pager -o cat --since "-120s" | tail -6
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
# Each line carries the pid that wrote it. Which is the fourth version of this check: comparing
# whole log lines passed because a restarted service also writes; comparing two readings from
# after the crash passed for the same reason; comparing a counter against its value from before
# the crash passed because the restarted process had simply been running longer than the original
# had. Every one of those was a check that could not fail, sitting next to a FAIL saying the
# service had been restarted. A pid in the output cannot be argued with by timing.
"$gz" add survivor -start -restart always -- sh -c 'i=0; while :; do echo "SURVIVOR-$$-$i"; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
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

# Age before the crash, which is the only place it means anything.
#
# This check has now been wrong three times, each one the same mistake in a new costume: a
# threshold instead of a fact, a tolerance wider than the difference, and then fifteen seconds of
# waiting placed *after* the crash - where an uptime measured from the adoption accumulates it
# just the same. The service has to be old when the daemon dies. Then "its own age" and "the time
# since the crash" are fifteen seconds apart and no tolerance can confuse them.
sleep 15

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

# The uptime has to survive too, not just the process.
#
# It did not. The adoption recorded time.Now() as the start, so a service that had been running
# since yesterday reported however long ago the daemon came back - and the error grows with the
# time since the crash rather than being the "seconds" the comment claimed. Found in production on
# the first real crash this program survived, where a shell up for an hour and a half said twenty
# minutes.
# Against ps rather than against a number I picked. The first version of this check asserted the
# uptime was at least twenty seconds, which failed on a service that was honestly seven seconds
# old - a threshold standing in for a fact. ps knows the real age; the only question is whether
# gozellij agrees with it.
real=$(ps -o etimes= -p "$svcpid" 2>/dev/null | tr -d ' ')
said=$("$gz" status survivor 2>/dev/null | awk '/^uptime:/{print $2}')
saidsecs=${said%s}; saidsecs=${saidsecs%.*}
case "$said" in *h*|*m*) saidsecs="" ;; esac
if [ -n "$real" ] && [ -n "$saidsecs" ] && [ "$((real - saidsecs))" -le 3 ] && [ "$((real - saidsecs))" -ge -3 ]; then
    ok "a recovered service keeps its real uptime (says ${said}, ps says ${real}s)"
else
    bad "the recovered service says ${said} but has been running ${real}s - that is the time since the crash, not its own"
fi

# Alive, not merely reported alive: its output has to still be arriving through the terminal the
# new daemon inherited, written by the process that was there before the crash. The pid in the
# line is what makes that unambiguous - see the note where the service is defined.
sleep 3
writer=$("$gz" logs survivor 2>/dev/null | grep -o 'SURVIVOR-[0-9]*-[0-9]*' | tail -1 | cut -d- -f2)
if [ -n "$writer" ] && [ "$writer" = "$svcpid" ]; then
    ok "and the lines still arriving were written by the process that was there before the crash"
else
    bad "the newest output was written by pid [$writer], and the service before the crash was $svcpid"
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

# ------------------------------------- somebody is attached when the daemon dies
#
# The scenario this is all actually for. You are in an ssh session, attached to your shell, and the
# daemon hits a bug. Everything above says the *process* survives; this says the person does.
#
# The client's connection dies with the daemon, so it has to notice, wait for the replacement and
# attach again - to the same shell, which has been running the whole time and still knows what you
# typed before it happened.
say
"$gz" add sticky -start -restart always -- sh -c 'PS1=""; export PS1; exec /bin/sh -i' >/dev/null 2>&1
sleep 1
tmux -L "$tmuxSock" kill-server 2>/dev/null
tmux -L "$tmuxSock" new-session -d -x 60 -y 10 \
    -e GOZELLIJ_RUNTIME_DIR="$work/run" -e GOZELLIJ_STATE_DIR="$work/state" -e HOME="$work/home" \
    -e TERM=xterm-256color \
    "sh -c 'stty -echo; exec $work/bin/gozellij attach sticky'" 2>/dev/null
sleep 2
# Something only this shell knows, set before the crash.
tmux -L "$tmuxSock" send-keys 'BEFORE=the-same-shell' Enter
sleep 1
sticky_pid=$("$gz" status sticky 2>/dev/null | awk '/^pid:/{print $2}')

main=$(systemctl --user show "$unit" -p MainPID --value)
kill -9 "$main" 2>/dev/null
for _ in $(seq 40); do
    now=$(systemctl --user show "$unit" -p MainPID --value)
    [ -n "$now" ] && [ "$now" != "0" ] && [ "$now" != "$main" ] && break
    sleep 0.25
done
# The reattach window is generous on purpose; give the client time to use it.
sleep 6

if [ "$($gz status sticky 2>/dev/null | awk '/^pid:/{print $2}')" = "$sticky_pid" ]; then
    ok "the shell somebody was attached to kept its pid"
else
    bad "the shell was replaced: $sticky_pid then $($gz status sticky 2>/dev/null | awk '/^pid:/{print $2}')"
fi

# The client has to be back, and talking to the same shell: the variable is only set in the
# process that was there before the crash.
tmux -L "$tmuxSock" send-keys 'echo STILL-HERE-$BEFORE' Enter
sleep 3
if tmux -L "$tmuxSock" capture-pane -p 2>/dev/null | grep -q 'STILL-HERE-the-same-shell'; then
    ok "and the client reattached to it by itself, with what was typed before the crash still set"
else
    bad "the client is not back on the same shell: $(tmux -L "$tmuxSock" capture-pane -p 2>/dev/null | tail -3 | tr '\n' '|')"
fi

tmux -L "$tmuxSock" kill-server 2>/dev/null
"$gz" rm sticky >/dev/null 2>&1

# ------------------------------------- an upgrade after a recovery still keeps the services
#
# The sequence that broke in production, in order: the daemon is restarted and recovers its
# services from the file-descriptor store, and then somebody runs `systemctl --user reload`. Two
# separate faults met there, and each on its own is enough to lose every service:
#
#   - the recovered pty was handed to the fabric as a bare descriptor number while the *os.File*
#     that carried it from systemd still owned it, so Go's finaliser closed it underneath the
#     running shell. The reload then reported "cannot keep pty fd 4 across exec: bad file
#     descriptor" and handed over nothing.
#   - the successor of an exec-in-place found its own socket - systemd holds a copy, so a connect
#     completes into the backlog with nobody accepting - decided another daemon was running, and
#     exited 1.
#
# So this recovers first and reloads second, which is the only order that would have caught it.
say
"$gz" add upgrader -start -restart always -- sh -c 'i=0; while :; do echo "UPG-$$-$i"; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
sleep 2
upg_pid=$("$gz" status upgrader 2>/dev/null | awk '/^pid:/{print $2}')

# A crash first, so the service is one the daemon recovered rather than one it started.
main=$(systemctl --user show "$unit" -p MainPID --value)
kill -9 "$main" 2>/dev/null
for _ in $(seq 40); do
    now=$(systemctl --user show "$unit" -p MainPID --value)
    [ -n "$now" ] && [ "$now" != "0" ] && [ "$now" != "$main" ] && break
    sleep 0.25
done
sleep 2
if [ "$("$gz" status upgrader 2>/dev/null | awk '/^pid:/{print $2}')" = "$upg_pid" ]; then
    ok "a service survives the crash it is about to be upgraded through"
else
    bad "the service did not survive the crash, so the upgrade cannot be tested"
fi

before_restarts=$(systemctl --user show "$unit" -p NRestarts --value)
systemctl --user reload "$unit" 2>/dev/null
sleep 3
after_pid=$("$gz" status upgrader 2>/dev/null | awk '/^pid:/{print $2}')
if [ -n "$after_pid" ] && [ "$after_pid" = "$upg_pid" ]; then
    ok "and keeps its pid through a reload that follows the recovery"
else
    bad "the reload lost it: $upg_pid became [$after_pid]"
fi
if [ "$(systemctl --user show "$unit" -p NRestarts --value)" = "$before_restarts" ]; then
    ok "and the reload did not make the unit restart"
else
    bad "the reload turned into a restart: $before_restarts -> $(systemctl --user show "$unit" -p NRestarts --value)"
fi
"$gz" rm upgrader >/dev/null 2>&1

# ----------------------------------------------- renamed while running, then the daemon crashes
#
# The descriptor systemd holds is filed under the service's name. A rename that left it there would
# pass every other check, and then the first crash would bring the service back under the name it
# no longer has - or fail to adopt it at all. Nothing in the Go tests can see this: they run without
# a systemd to hand descriptors to.
say
"$gz" add beforename -start -- sh -c 'while :; do sleep 1; done' >/dev/null 2>&1
sleep 1
rn_pid=$("$gz" status beforename 2>/dev/null | awk '/^pid:/{print $2}')
if "$gz" rename beforename aftername >/dev/null 2>&1; then
    ok "a running service can be renamed (pid ${rn_pid:-none})"
else
    bad "renaming a running service failed: $("$gz" rename beforename aftername 2>&1)"
fi
held=$(systemctl --user show "$unit" -p NFileDescriptorStore --value)
if [ "$held" = "2" ]; then
    ok "and systemd is holding exactly its one terminal besides the socket, not two"
else
    bad "after the rename systemd holds $held descriptors; want the socket and one terminal"
fi

main=$(systemctl --user show "$unit" -p MainPID --value)
kill -9 "$main" 2>/dev/null
for _ in $(seq 40); do
    now=$(systemctl --user show "$unit" -p MainPID --value)
    [ -n "$now" ] && [ "$now" != "0" ] && [ "$now" != "$main" ] && "$gz" ping >/dev/null 2>&1 && break
    sleep 0.25
done
after_rn=$("$gz" status aftername 2>/dev/null | awk '/^pid:/{print $2}')
if [ -n "$rn_pid" ] && [ "$after_rn" = "$rn_pid" ]; then
    ok "after a crash it is recovered under its new name, same pid"
else
    bad "after a crash aftername has pid [$after_rn], want $rn_pid; ls says: $("$gz" ls 2>&1 | tr '\n' '|')"
fi
if "$gz" status beforename >/dev/null 2>&1; then
    bad "and the old name came back from the crash"
else
    ok "and the old name did not come back"
fi
"$gz" rm aftername >/dev/null 2>&1

# ---------------------------------------------------------- systemctl restart, which people type
#
# packaging/README.md said not to, because restart used to take every service down with the
# daemon. With KillMode=process only the daemon is signalled, and the store is kept across a
# restart (FileDescriptorStorePreserve's default), so the next daemon should adopt everything. A
# packager's post-install that runs restart is common enough that which of the two is true matters.
say
"$gz" add survivor -start -- sh -c 'while :; do sleep 1; done' >/dev/null 2>&1
sleep 1
sv_pid=$("$gz" status survivor 2>/dev/null | awk '/^pid:/{print $2}')
main=$(systemctl --user show "$unit" -p MainPID --value)
t0=$(date +%s)
systemctl --user restart "$unit" 2>/dev/null
took=$(( $(date +%s) - t0 ))
# Synchronous, so this is how long the old daemon took to stop. It used to be systemd's whole stop
# timeout: a daemon that had stored its socket hung in shutdown, and only SIGKILL ended it.
if [ "$took" -le 5 ]; then
    ok "and the daemon stops promptly when asked (${took}s)"
else
    bad "systemctl restart took ${took}s: the daemon did not stop on SIGTERM and was killed at the timeout"
fi
for _ in $(seq 40); do
    now=$(systemctl --user show "$unit" -p MainPID --value)
    [ -n "$now" ] && [ "$now" != "0" ] && [ "$now" != "$main" ] && "$gz" ping >/dev/null 2>&1 && break
    sleep 0.25
done
after_sv=$("$gz" status survivor 2>/dev/null | awk '/^pid:/{print $2}')
if [ -n "$sv_pid" ] && [ "$after_sv" = "$sv_pid" ]; then
    ok "systemctl restart keeps a running service, same pid (daemon $main then $(systemctl --user show "$unit" -p MainPID --value))"
else
    bad "after systemctl restart the service's pid went $sv_pid to [$after_sv]"
fi
"$gz" rm survivor >/dev/null 2>&1

# ------------------------------------------- the store must not fill up with terminals of the dead
#
# FileDescriptorStoreMax is 64 in the packaged unit. A daemon that hands a terminal over on every
# start and never takes one back reaches that after 64 restarts, and from then on every new service
# is quietly unprotected - the exact failure this whole mechanism exists to prevent, arriving
# silently after weeks of a service that flaps.
#
# The grandchild is what makes this a real case rather than a theoretical one. A plain service's pty
# master hangs up when the service dies and systemd closes it by itself, so a simpler version of
# this check passed with the drop removed and said nothing. Anything that spawns a detached helper -
# setsid, a double fork, most things that daemonise - leaves the slave open, the master never hangs
# up, and systemd holds it for ever.
say
"$gz" add flapper -start -restart always -- sh -c 'setsid sleep 30 & echo alive; sleep 1; exit 1' >/dev/null 2>&1
sleep 12
held=$(systemctl --user show "$unit" -p NFileDescriptorStore --value)
restarts=$("$gz" status flapper 2>/dev/null | awk '/^restarts:/{print $2}')
# The socket, plus at most the one terminal of whichever instance is alive right now.
if [ -n "$held" ] && [ "$held" -le 2 ]; then
    ok "a service that keeps restarting does not fill the store (${restarts:-0} restarts, $held held)"
else
    bad "after ${restarts:-0} restarts systemd is holding $held descriptors; want no more than 2"
fi

"$gz" stop flapper >/dev/null 2>&1
sleep 2
left=$(systemctl --user show "$unit" -p NFileDescriptorStore --value)
if [ -n "$left" ] && [ "$left" -le 1 ]; then
    ok "and stopping it leaves nothing of it behind"
else
    bad "after stopping it systemd still holds $left descriptors; want only the socket"
fi
"$gz" rm flapper >/dev/null 2>&1

say
if [ "$fail" -eq 0 ]; then
    say "all $pass held."
else
    say "$pass held, $fail did not."
fi
exit $(( fail > 0 ? 1 : 0 ))
