#!/usr/bin/env bash
#
# Re-checks the promises in docs/REPLACING_GEZELLIJ.md, on this machine, now.
#
# Every "Verified" in that document was true when somebody typed it. That is not the same as being
# true, and the gap between the two is exactly what this project keeps finding in other people's
# code: a claim nothing re-checks is a claim that decays. The attach client has been rewritten
# since most of those lines were written, and "an attached client reattaches by itself" has been
# an argument rather than a measurement ever since.
#
# So this runs them. It needs a real pty for most of it, which is what `script -qec` is for, and it
# runs against its own daemon in a throwaway directory - it will not touch a fabric you are using.
#
#   ./scripts/acceptance.sh            # build and run everything
#   ./scripts/acceptance.sh -k         # keep the working directory for inspection
#
# Exit status is 0 only if every promise held.

set -u

keep=0
[ "${1:-}" = "-k" ] && keep=1

repo=$(cd "$(dirname "$0")/.." && pwd)
real_home="$HOME"
work=$(mktemp -d "${TMPDIR:-/tmp}/gozellij-acceptance.XXXXXX")
bin="$work/bin"
run="$work/run"
state="$work/state"
home="$work/home"
mkdir -p "$bin" "$run" "$state" "$home"

pass=0
fail=0
daemon_pid=""

say()  { printf '%s\n' "$*"; }
ok()   { pass=$((pass + 1)); printf '  PASS  %s\n' "$*"; }
bad()  { fail=$((fail + 1)); printf '  FAIL  %s\n' "$*"; }

cleanup() {
    # By recorded pid, never by pattern: a pattern matches this script's own command line, which
    # is how you kill your own shell. (Learned the hard way; see the note in the loop rules.)
    [ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null
    sleep 0.3
    if [ "$keep" = 1 ]; then
        say "working directory kept at $work"
    else
        rm -rf "$work"
    fi
}
trap cleanup EXIT

gz="$bin/gozellij"
gzd="$bin/gozellijd"

# Build before the environment is swapped. With HOME pointed at the throwaway directory, Go puts
# its module cache there - read-only, so the cleanup cannot remove it, and the run ends in a
# screenful of permission errors that hide the results.
say "building from $repo"
( cd "$repo" && go build -o "$gzd" ./cmd/gozellijd && go build -o "$gz" ./cmd/gozellij ) || {
    say "build failed"; exit 1
}

export GOZELLIJ_RUNTIME_DIR="$run" GOZELLIJ_STATE_DIR="$state" HOME="$home"

# start_daemon [version] - starts a daemon under an empty environment.
#
# env -i on purpose: under the systemd user unit the daemon has almost no environment, and a test
# that inherits the developer's terminal proves nothing about that. This is how TERM=dumb was
# found.
start_daemon() {
    local version="${1:-v1}"
    env -i PATH="/usr/bin:/bin" HOME="$home" \
        GOZELLIJ_RUNTIME_DIR="$run" GOZELLIJ_STATE_DIR="$state" \
        "$gzd" >>"$work/daemon.log" 2>&1 &
    daemon_pid=$!
    for _ in $(seq 50); do
        "$gz" ping >/dev/null 2>&1 && return 0
        sleep 0.1
    done
    say "the daemon never answered; see $work/daemon.log"
    return 1
}

stop_daemon() {
    [ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null
    wait "$daemon_pid" 2>/dev/null
    daemon_pid=""
}

# in_terminal <keys-script> <command...> - runs a command on a real pty, feeding it keys over time.
#
# The keys come from a shell fragment rather than a file, because *when* a key arrives is half of
# what is being tested. A file is delivered the instant the program starts reading, so "attach,
# wait for something to happen, then detach" becomes "attach and detach", and the thing in the
# middle is never observed. Two checks passed for that reason before this was fixed - which is the
# same mistake as a `Verified` line nothing re-runs, one level down.
in_terminal() {
    local keys="$1"; shift
    timeout 30 script -qec "$*" /dev/null < <( eval "$keys" ) 2>&1
}

start_daemon || exit 1

say
say "the promises in docs/REPLACING_GEZELLIJ.md:"

# ---------------------------------------------------------------- an interactive shell on a pty
{
    out=$(TERM=xterm-256color in_terminal 'sleep 1; printf "tty; echo DOLLAR-DASH=\$-\n"; sleep 1; printf "exit\n"; sleep 1' "$gz shell")
    if printf '%s' "$out" | grep -q '/dev/pts/'; then
        ok "bare gozellij lands you in a shell on a real pts"
    else
        bad "bare gozellij did not give a pts: $(printf '%s' "$out" | tail -2)"
    fi
    if printf '%s' "$out" | grep -qE 'DOLLAR-DASH=[^=]*i'; then
        ok "that shell is interactive (\$- contains i)"
    else
        bad "the shell is not interactive"
    fi
    if printf '%s' "$out" | grep -q 'exited'; then
        ok "exit returns you to your own prompt"
    else
        bad "exit did not end the attach"
    fi
}

# ---------------------------------------------------------------------------- the environment
{
    "$gz" add envprobe -start -- sh -c 'echo "TERM=[$TERM] GOZELLIJ=[$GOZELLIJ]"; sleep 30' >/dev/null 2>&1
    sleep 1
    logged=$("$gz" logs envprobe 2>/dev/null)
    "$gz" rm envprobe >/dev/null 2>&1
    # A service added by hand inherits the daemon's environment, which under env -i has no TERM.
    # The *shell* is the one that captures yours; that is checked below.
    if printf '%s' "$logged" | grep -q 'TERM='; then
        ok "a service's environment is visible in its log"
    else
        bad "could not read a service's environment back"
    fi
}

# ------------------------------------------------------------------- the shell captures YOUR term
{
    "$gz" stop shell >/dev/null 2>&1
    out=$(TERM=xterm-256color in_terminal 'sleep 1; printf "echo MY-TERM=\$TERM\n"; sleep 1; printf "exit\n"; sleep 1' "$gz shell")
    if printf '%s' "$out" | grep -q 'MY-TERM=xterm-256color'; then
        ok "the shell gets the terminal you started it from, not the daemon's"
    else
        bad "the shell's TERM is not yours: $(printf '%s' "$out" | grep -o 'MY-TERM=[^ ]*' | head -1)"
    fi
}

# ------------------------------------------------------------------------ detach keeps it running
{
    "$gz" stop shell >/dev/null 2>&1
    out=$(in_terminal 'sleep 1; printf "echo MARKER-ALIVE\n"; sleep 1; printf "\035d"; sleep 1' "$gz shell")
    sleep 0.5
    state_now=$("$gz" status shell 2>/dev/null | awk '/^state/{print $2}')
    if printf '%s' "$out" | grep -q 'detached from'; then
        ok "Ctrl-] d detaches"
    else
        bad "Ctrl-] d did not detach"
    fi
    if [ "$state_now" = "running" ]; then
        ok "the service keeps running after you detach"
    else
        bad "the service is '$state_now' after a detach, not running"
    fi
}

# --------------------------------------------------------------------------------------- tabs
{
    "$gz" add tabtarget -start -- sh -c 'echo I-AM-TABTARGET; sleep 30' >/dev/null 2>&1
    sleep 0.5
    out=$(in_terminal 'sleep 1; printf "\035n"; sleep 2; printf "\035d"; sleep 1' "$gz attach shell")
    if printf '%s' "$out" | grep -q 'I-AM-TABTARGET'; then
        ok "Ctrl-] n switches to another service in the same terminal"
    else
        bad "Ctrl-] n did not switch"
    fi
}

# -------------------------------------------------------- an upgrade keeps every pid, and the client
{
    before=$("$gz" ls 2>/dev/null | awk 'NR>1 {print $1"="$3}' | sort)
    # A different binary, so the version really changes and the daemon really re-execs.
    ( cd "$repo" && HOME="$real_home" go build -ldflags "-X main.Version=acceptance-v2" -o "$gzd" ./cmd/gozellijd ) || bad "could not build v2"

    # A client attached across the upgrade is the part nothing else tests. It has to still be
    # attached when the daemon replaces itself, so the detach key comes well afterwards.
    ( sleep 3; "$gz" upgrade >"$work/upgrade.out" 2>&1 ) &
    upgrader=$!
    out=$(in_terminal 'sleep 8; printf "\035d"; sleep 1' "$gz attach shell")
    wait "$upgrader" 2>/dev/null

    sleep 1
    after=$("$gz" ls 2>/dev/null | awk 'NR>1 {print $1"="$3}' | sort)
    if [ -n "$before" ] && [ "$before" = "$after" ]; then
        ok "gozellij upgrade keeps every service's pid"
    else
        bad "pids changed across the upgrade: before [$before] after [$after]"
    fi
    if grep -q 'acceptance-v2' "$work/upgrade.out" 2>/dev/null; then
        ok "the daemon really is the new binary afterwards"
    else
        bad "the upgrade did not report the new version: $(cat "$work/upgrade.out" 2>/dev/null | tr '\n' ' ')"
    fi
    if printf '%s' "$out" | grep -q 'reattach'; then
        ok "an attached client notices the upgrade and reattaches by itself"
    else
        bad "the attached client said nothing about reattaching"
    fi
    # The daemon we started was replaced in place, so the pid is the same but our shell job is not
    # its parent any more; keep the pid for cleanup.
}

# ----------------------------------------------------------------- logs outlive the daemon
{
    "$gz" add pineapple -start -- sh -c 'echo PINEAPPLE-42; sleep 30' >/dev/null 2>&1
    sleep 1
    stop_daemon
    sleep 0.5
    start_daemon || exit 1
    if "$gz" logs pineapple 2>/dev/null | grep -q 'PINEAPPLE-42'; then
        ok "logs survive the daemon being killed outright"
    else
        bad "the log did not survive the daemon"
    fi
}

# --------------------------------------------------------------- enabled services come back
{
    # The daemon was just restarted above; anything marked enabled should be running again.
    running=$("$gz" status pineapple 2>/dev/null | awk '/^state/{print $2}')
    if [ "$running" = "running" ] || [ "$running" = "exited" ]; then
        ok "enabled services are started again by a fresh daemon (reboot-equivalent)"
    else
        bad "an enabled service came back as '$running'"
    fi
}

# ------------------------------------------------------------------------------- tree-kill
{
    "$gz" add stubborn -start -- sh -c 'trap "" HUP; (trap "" HUP; exec sleep 4242) & echo STUBBORN-UP; wait' >/dev/null 2>&1
    sleep 1
    "$gz" stop stubborn >/dev/null 2>&1
    sleep 1
    if ps -eo args | grep -q '^sleep 4242$'; then
        bad "a child that ignores SIGHUP outlived its service"
        # Clean it up by pid, having read the list.
        child=$(ps -eo pid,args | awk '$2=="sleep" && $3=="4242" {print $1; exit}')
        [ -n "$child" ] && kill "$child" 2>/dev/null
    else
        ok "stop takes the service's children with it"
    fi
    mode=$("$gz" doctor 2>/dev/null | awk '/stopping services/{$1=""; $2=""; print}' | sed 's/^ *//')
    say "        (tree-kill mode: $mode)"
}

say
if [ "$fail" -eq 0 ]; then
    say "all $pass promises held."
else
    say "$pass held, $fail did not."
fi
exit $(( fail > 0 ? 1 : 0 ))
