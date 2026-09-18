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

# tmuxSock is a private tmux server, so nothing here can touch a session you are using.
tmuxSock="gozellij-acceptance-$$"

cleanup() {
    # By recorded pid, never by pattern: a pattern matches this script's own command line, which
    # is how you kill your own shell. (Learned the hard way; see the note in the loop rules.)
    [ -n "$daemon_pid" ] && kill "$daemon_pid" 2>/dev/null
    tmux -L "$tmuxSock" kill-server 2>/dev/null
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

# kill_daemon_hard is what the document actually claims to survive: no chance to tidy up.
kill_daemon_hard() {
    [ -n "$daemon_pid" ] && kill -9 "$daemon_pid" 2>/dev/null
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
    # Labelled for what it actually proves. A service added by hand inherits the *daemon's*
    # environment, which under env -i has no TERM at all - so this says `logs` returns what the
    # service printed, and nothing about any environment promise. The shell's own environment is
    # the one that matters and is checked below.
    if printf '%s' "$logged" | grep -q 'GOZELLIJ='; then
        ok "logs returns what a service printed"
    else
        bad "could not read a service's output back from logs"
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
    # After the reattach, type at the service and check the answer comes back. The arithmetic is
    # deliberate: `script` echoes keystrokes onto the pty, so a marker typed literally would be
    # matched from the echo whether or not the far end ever saw it. The shell computes 6*7, and
    # only the shell can produce the string that looks for.
    out=$(in_terminal 'sleep 8; printf "echo AFTER-\$((6*7))-MARKER\n"; sleep 3; printf "\035d"; sleep 1' "$gz attach shell")
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
    # "reattached;" and not "reattaching...": the latter is printed on any disconnect, before any
    # attempt has been made, so matching it passed even when the client then gave up entirely.
    if printf '%s' "$out" | grep -q 'reattached;'; then
        ok "an attached client notices the upgrade and comes back by itself"
    else
        bad "the attached client never reported reattaching"
    fi
    if printf '%s' "$out" | grep -q 'AFTER-42-MARKER'; then
        ok "and the terminal still works afterwards: typing reaches the service and the reply comes back"
    else
        bad "after the upgrade the reattached terminal did not carry a keystroke to the service"
    fi
    # The daemon we started was replaced in place, so the pid is the same but our shell job is not
    # its parent any more; keep the pid for cleanup.
}

# ----------------------------------------------------------------- logs outlive the daemon
{
    marker="PINEAPPLE-$$-$RANDOM"
    "$gz" add pineapple -start -- sh -c "echo $marker; sleep 30" >/dev/null 2>&1
    sleep 1
    # Stop it first, which also marks it disabled. Otherwise the fresh daemon starts it again and
    # it prints the marker a second time - so the check passed even with the log file deleted,
    # which is precisely the thing it claims to be testing.
    "$gz" stop pineapple >/dev/null 2>&1
    if ! "$gz" logs pineapple 2>/dev/null | grep -q "$marker"; then
        bad "the marker was not in the log before the daemon was killed (the check would be vacuous)"
    fi
    kill_daemon_hard
    sleep 0.5
    start_daemon || exit 1
    if "$gz" logs pineapple 2>/dev/null | grep -q "$marker"; then
        ok "logs survive kill -9 of the daemon, for a service that is not restarted"
    else
        bad "the log did not survive the daemon"
    fi
}

# --------------------------------------------------------------- enabled services come back
{
    # The daemon was just restarted above; anything marked enabled should be running again.
    # tabtarget was left enabled and running, so it is the one to ask about - pineapple was
    # deliberately stopped, and asking about it would have been a check that cannot fail.
    running=$("$gz" status tabtarget 2>/dev/null | awk '/^state/{print $2}')
    if [ "$running" = "running" ] || [ "$running" = "exited" ]; then
        ok "enabled services are started again by a fresh daemon (reboot-equivalent)"
    else
        bad "an enabled service came back as '$running'"
    fi
}

# ------------------------------------------------------------------------------- tree-kill
{
    # A sleep length unique to this run, so an unrelated process on the host cannot be mistaken
    # for our child - in either direction.
    naptime=$(( 40000 + (RANDOM % 9000) ))
    "$gz" add stubborn -start -- sh -c "trap '' HUP; (trap '' HUP; exec sleep $naptime) & echo STUBBORN-UP; wait" >/dev/null 2>&1

    # Wait for the child to exist, and fail if it never does. Without this the check passed
    # whenever the service failed to start at all: no child, nothing left behind, "PASS".
    child=""
    for _ in $(seq 50); do
        child=$(ps -eo pid,args | awk -v n="$naptime" '$2=="sleep" && $3==n {print $1; exit}')
        [ -n "$child" ] && break
        sleep 0.1
    done
    if [ -z "$child" ]; then
        bad "the service's child never started, so there was nothing to test"
    else
        "$gz" stop stubborn >/dev/null 2>&1
        sleep 1
        still=$(ps -eo pid,args | awk -v n="$naptime" '$2=="sleep" && $3==n {print $1; exit}')
        if [ -n "$still" ]; then
            bad "a child that ignores SIGHUP outlived its service (pid $still)"
            kill "$still" 2>/dev/null
        else
            ok "stop takes the service's children with it"
        fi
    fi
    mode=$("$gz" doctor 2>/dev/null | awk '/stopping services/{$1=""; $2=""; print}' | sed 's/^ *//')
    say "        (tree-kill mode: $mode)"
}

# ------------------------------------------------------------------ what the screen actually does
#
# Everything above drives a pty through `script`, and `script` allocates a pty with NO SIZE when
# its own stdin is a pipe - which it is here. A terminal of unknown size is one the status line
# refuses to draw on, so none of these checks can see it at all. Three screen bugs shipped through
# that gap: an attach that appeared to hang, a detach that homed the cursor, and a status line that
# drew over vim's command line every two seconds.
#
# So this section drives a real terminal of a known size and reads the screen back. tmux is the
# tool to hand; a private server, so it cannot touch a session you are using.
if ! command -v tmux >/dev/null 2>&1; then
    say "  SKIP  the screen checks need tmux, which is not installed"
else
    pane() { tmux -L "$tmuxSock" capture-pane -p; }
    ask()  { tmux -L "$tmuxSock" display -p "$1"; }

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    tmux -L "$tmuxSock" new-session -d -x 80 -y 24 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e SHELL=/bin/sh -e TERM=xterm-256color \
        "sh -c 'PS1=\"outer\\$ \"; export PS1; exec /bin/sh -i'"
    sleep 1

    # Fill the screen before attaching, so the cursor is on the LAST ROW when the status line
    # reserves its region. That is not an edge case - it is what a shell you have been using looks
    # like - and it is the condition under which the terminal appeared to hang: the cursor ends up
    # below the new bottom margin, where a line feed does not scroll. Starting from a blank screen
    # hides the bug completely, which this check did until it was sabotaged and passed anyway.
    tmux -L "$tmuxSock" send-keys 'seq 40' Enter
    sleep 1
    tmux -L "$tmuxSock" send-keys "$gz shell; echo BACK-IN-THE-OUTER-SHELL; sleep 60" Enter
    sleep 3

    # 1. Typing works, and the output lands *inside* the region.
    #
    #    Verified by sabotage: remove the reserve() call before the first paint and this goes red
    #    four runs out of four - the cursor is restored below the bottom margin, where a line feed
    #    does not scroll, and the screen stops moving. The `head -23` matters: without it the check
    #    happened to pass or fail depending on whether the next tick had wiped row 24 yet, which
    #    made it a guard that worked only while the sleep here equalled the refresh interval.
    tmux -L "$tmuxSock" send-keys 'echo SCREEN-$((6*7))-OK' Enter
    sleep 2
    if pane | head -n 23 | grep -q 'SCREEN-42-OK'; then
        ok "typing in an attached shell produces output inside the region"
    else
        bad "nothing appeared inside the region after typing - the terminal is wedged"
    fi

    # 1b. The cursor is where the shell left it, near the bottom of the full screen it attached to.
    #
    #     Without this, an attach that homed the cursor passed all six of these: the replayed
    #     prompt printed over the top three rows of what was already on screen, everything else
    #     looked right, and nothing said a word. It is the same failure check 6 guards on detach,
    #     which was unguarded on attach.
    cy=$(ask '#{cursor_y}')
    if [ "$cy" -ge 19 ] && [ "$cy" -le 22 ]; then
        ok "attaching leaves the cursor where the shell had it, not at the top of the screen"
    else
        bad "after attaching the cursor is on row $cy of 0-23; the screen was drawn over from the top"
    fi

    # 2. The status line is on the last row, and the service has the rest.
    if pane | tail -1 | grep -q 'up '; then
        ok "the status line is on the last row"
    else
        bad "no status line on the last row: $(pane | tail -1)"
    fi
    region=$(ask '#{scroll_region_upper}-#{scroll_region_lower}')
    if [ "$region" = "0-22" ]; then
        ok "the bottom row is reserved (scrolling region $region of 0-23)"
    else
        bad "the scrolling region is $region, want 0-22 with the last row reserved"
    fi

    # 3. A full-screen program keeps its own last row. Verified by sabotage: tell the service it
    #    has all n rows again and this check fails.
    tmux -L "$tmuxSock" send-keys 'less /etc/services' Enter
    sleep 3
    if pane | tail -2 | head -1 | grep -q '/etc/services'; then
        ok "a full-screen program keeps its own bottom row"
    else
        bad "the status line drew over the full-screen program's bottom row: $(pane | tail -2 | head -1)"
    fi
    tmux -L "$tmuxSock" send-keys 'q'
    sleep 1

    # 4. Detaching gives the terminal back: region released, cursor where it was rather than homed.
    #    Verified by sabotage: move the region reset back outside the save-and-restore pair and the
    #    last check here fails with the prompt printing at row 1.
    tmux -L "$tmuxSock" send-keys C-] d
    sleep 2
    region=$(ask '#{scroll_region_upper}-#{scroll_region_lower}')
    if [ "$region" = "0-23" ]; then
        ok "detaching releases the reserved row"
    else
        bad "the scrolling region is still $region after detaching"
    fi
    where=$(pane | grep -n 'BACK-IN-THE-OUTER-SHELL' | head -1 | cut -d: -f1)
    if [ -n "$where" ] && [ "$where" -gt 3 ]; then
        ok "detaching leaves the cursor where it was, not at the top of the screen"
    else
        bad "after detaching the next prompt printed at row ${where:-?}, over what was already there"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
fi

say
if [ "$fail" -eq 0 ]; then
    say "all $pass promises held."
else
    say "$pass held, $fail did not."
fi
exit $(( fail > 0 ? 1 : 0 ))
