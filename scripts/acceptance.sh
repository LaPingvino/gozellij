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

# only <name>... - removes every service except the ones named.
#
# Later checks run in an environment the earlier ones littered, and three checks have now quietly
# tested the wrong thing because of it: a split opened `pineapple` left over from the log-survival
# check, then `stubborn` left over from the tree-kill check, and a status line was read that was
# perfectly correct about a service the check had never heard of. Anything that depends on *which*
# service gozellij picks must say what it expects to be there.
#
# Removal rather than a fresh daemon per section: the daemon surviving its own checks is one of the
# things being tested, and restarting it between them would quietly stop testing that.
only() {
    local keep=" $* "
    local name
    for name in $("$gz" ls 2>/dev/null | awk 'NR>1 {print $1}'); do
        case "$keep" in
            *" $name "*) ;;
            *) "$gz" rm "$name" >/dev/null 2>&1 ;;
        esac
    done
    # And say so if it did not work. A helper that clears the ground and silently fails to is
    # worse than no helper: the checks that follow would go back to depending on what the earlier
    # ones left behind, and would pass or fail for reasons nobody could see.
    local left
    left=$("$gz" ls 2>/dev/null | awk 'NR>1 {print $1}' | tr '\n' ' ')
    for name in $left; do
        case "$keep" in
            *" $name "*) ;;
            *) bad "could not clear the ground before a screen check: $name is still defined" ;;
        esac
    done
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

# ---------------------------------------------------- one command, several services
{
    for n in multi_a multi_b multi_c; do
        "$gz" add "$n" -start -- sleep 60 >/dev/null 2>&1
    done
    sleep 1
    "$gz" stop multi_a multi_b multi_c >/dev/null 2>&1
    stopped=0
    for n in multi_a multi_b multi_c; do
        "$gz" status "$n" 2>/dev/null | grep -q 'state: *stopped' && stopped=$((stopped+1))
    done
    if [ "$stopped" -eq 3 ]; then
        ok "one stop command stops every service named"
    else
        bad "stop of three services stopped $stopped of them"
    fi

    # The middle name does not exist. The two real ones must still be started, and the command must
    # still fail - stopping at the typo would leave the third one alone while exiting non-zero,
    # which reads as nothing having happened.
    partial=$("$gz" start multi_a nosuch_service multi_c 2>&1); partial_rc=$?
    started=0
    for n in multi_a multi_c; do
        "$gz" status "$n" 2>/dev/null | grep -q 'state: *running' && started=$((started+1))
    done
    if [ "$started" -eq 2 ] && [ "$partial_rc" -ne 0 ] && printf '%s' "$partial" | grep -q '1 of 3'; then
        ok "a bad name among good ones is reported without abandoning the rest"
    else
        bad "start of three with one typo: $started started, rc=$partial_rc"
    fi

    removed=$("$gz" rm multi_a multi_b multi_c 2>&1)
    if [ "$(printf '%s' "$removed" | grep -c '^removed multi_')" -eq 3 ] && ! "$gz" ls 2>/dev/null | grep -q multi_; then
        ok "one rm command forgets every service named"
    else
        bad "rm of three services left some behind: $(printf '%s' "$removed" | head -2)"
    fi
}

# ------------------------------------------------------- following several services at once
{
    "$gz" add follow_a -start -- sh -c 'i=0; while [ $i -lt 20 ]; do echo AAA-$i; i=$((i+1)); sleep 0.3; done' >/dev/null 2>&1
    "$gz" add follow_b -start -- sh -c 'i=0; while [ $i -lt 20 ]; do echo BBB-$i; i=$((i+1)); sleep 0.3; done' >/dev/null 2>&1
    sleep 1
    both=$(timeout 3 "$gz" logs -f follow_a follow_b 2>&1)
    one=$(timeout 2 "$gz" logs -f follow_a 2>&1)
    refused=$("$gz" logs follow_a follow_b 2>&1); refused_rc=$?
    "$gz" rm follow_a follow_b >/dev/null || bad "could not clean up the followed services"

    # Both services, and every line of each carrying its own name. Checking only that both names
    # appear would pass if the prefixes were attached to the wrong lines, which is the way this
    # can actually go wrong.
    if printf '%s' "$both" | grep -q 'follow_a | AAA-' && printf '%s' "$both" | grep -q 'follow_b | BBB-' \
       && ! printf '%s' "$both" | grep -q 'follow_a | BBB-\|follow_b | AAA-'; then
        ok "logs -f follows several services, each line under its own name"
    else
        bad "following two services did not interleave correctly: $(printf '%s' "$both" | head -3)"
    fi

    # One service must be untouched, because `logs -f x | grep` should not have to strip a prefix.
    if printf '%s' "$one" | grep -q '^AAA-' && ! printf '%s' "$one" | grep -q 'follow_a |'; then
        ok "logs -f with one service is unprefixed, as it always was"
    else
        bad "following one service added something: $(printf '%s' "$one" | head -2)"
    fi

    if [ "$refused_rc" -ne 0 ] && printf '%s' "$refused" | grep -q 'several with -f'; then
        ok "several names without -f is refused rather than run together"
    else
        bad "logs of two services without -f was accepted (rc=$refused_rc)"
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
    pane() { tmux -L "$tmuxSock" capture-pane -p "$@"; }
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
        bad "after attaching the cursor is on row $cy of 0-23, so the screen was drawn over"
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

    # ------------------------------------------------ the rendered attach, which owns the screen
    #
    # The default attach borrows the terminal: the status line needs its single cursor-save slot
    # and a scrolling region, shared with a service that does not know it is sharing. With
    # GOZELLIJ_RENDER=1 the client keeps a grid of its own and paints it, so the status line is a
    # row the service was never given. The observable difference is the scrolling region: the
    # borrowing one sets it, and this one must leave the whole screen alone.
    only
    "$gz" add renderdemo -start -- sh -c 'printf "RENDERED-OUTPUT-HERE\r\n"; sleep 120' >/dev/null 2>&1
    sleep 1
    tmux -L "$tmuxSock" new-session -d -x 60 -y 12 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render renderdemo'"
    sleep 3

    if pane | grep -q 'RENDERED-OUTPUT-HERE'; then
        ok "a rendered attach shows the service's output"
    else
        bad "the rendered attach drew no service output: $(pane | head -2)"
    fi

    if pane | sed -n '12p' | grep -q 'renderdemo'; then
        ok "the rendered attach draws the status line on the last row"
    else
        bad "row 12 of the rendered screen is $(pane | sed -n '12p')"
    fi

    # The point of the whole exercise. A region of 0-11 on a twelve-row screen is the terminal
    # untouched; the borrowing painter would have set 0-10 to keep the last row for itself.
    region=$(ask '#{scroll_region_upper}-#{scroll_region_lower}')
    if [ "$region" = "0-11" ]; then
        ok "the rendered attach borrows no scrolling region (it is $region, the whole screen)"
    else
        bad "the rendered attach set a scrolling region: $region"
    fi

    # -no-render must win over the environment. Somebody who has switched rendering on for their
    # session needs a way to get the plain attach for one command, which is the first thing they
    # will want if a screen ever looks wrong.
    # Its own server, so that "the pane being asked about" is not in question. Asking the first
    # server for `-t :.+` returned the original, still-rendered window and the check failed for
    # that reason rather than for the one it names.
    override="${tmuxSock}-override"
    tmux -L "$override" new-session -d -x 60 -y 12 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e GOZELLIJ_RENDER=1 -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -no-render renderdemo'"
    sleep 2
    plainRegion=$(tmux -L "$override" display -p '#{scroll_region_upper}-#{scroll_region_lower}' 2>/dev/null || echo unknown)
    tmux -L "$override" kill-server 2>/dev/null
    if [ "$plainRegion" != "0-11" ]; then
        ok "-no-render falls back to the byte pipe even with GOZELLIJ_RENDER set (region $plainRegion)"
    else
        bad "-no-render still rendered: region $plainRegion"
    fi

    # --------------------------------------------------------------- two services, one screen
    #
    # The thing a byte pipe cannot do at all: two services drawing at once would be two programs
    # writing over each other. Owning the grid is what makes this possible, which is why DESIGN.md
    # puts the emulator before the multiplexing.
    "$gz" add rendertwo -start -- sh -c 'printf "SECOND-PANE-TEXT\r\n"; sleep 120' >/dev/null 2>&1
    sleep 1
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 3

    if pane | grep -q 'RENDERED-OUTPUT-HERE.*SECOND-PANE-TEXT'; then
        ok "Ctrl-] | puts two services side by side on one screen"
    else
        bad "splitting did not show both services: $(pane | head -1)"
    fi

    # Which pane has the keyboard. With two shells on screen there is no other way to tell, and
    # typing into the wrong one is the mistake this prevents.
    if pane | sed -n '12p' | grep -q 'renderdemo \[rendertwo\]'; then
        ok "the status line marks which pane the keyboard is going to"
    else
        bad "the pane marker is wrong: $(pane | sed -n '12p')"
    fi

    tmux -L "$tmuxSock" send-keys C-] 'o'
    sleep 2
    if pane | sed -n '12p' | grep -q '\[renderdemo\] rendertwo'; then
        ok "Ctrl-] o moves the keyboard to the other pane"
    else
        bad "focus did not move: $(pane | sed -n '12p')"
    fi

    # And still nothing borrowed, with two panes up.
    region=$(ask '#{scroll_region_upper}-#{scroll_region_lower}')
    if [ "$region" = "0-11" ]; then
        ok "a split screen still borrows no scrolling region"
    else
        bad "the split screen set a scrolling region: $region"
    fi

    # ------------------------------------------ two shells, and typing into the right one
    #
    # The core of what a multiplexer does, and until now only the drawing was checked. A keystroke
    # going to the wrong pane is the mistake that gets discovered by running something destructive
    # in the wrong shell, so it is worth a check that uses real interactive shells rather than
    # services that only print.
    only
    "$gz" add sh1 -start -- sh -c 'PS1="one$ "; export PS1; exec /bin/sh -i' >/dev/null 2>&1
    "$gz" add sh2 -start -- sh -c 'PS1="two$ "; export PS1; exec /bin/sh -i' >/dev/null 2>&1
    sleep 1
    tmux -L "$tmuxSock" new-session -d -x 70 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e SHELL=/bin/sh -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render sh1'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 2
    tmux -L "$tmuxSock" send-keys 'echo TYPED-RIGHT' Enter
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] 'o'
    sleep 1
    tmux -L "$tmuxSock" send-keys 'echo TYPED-LEFT' Enter
    sleep 2

    # Each answer must be in its own pane, which is a statement about columns: the left one at
    # column 0 and the right one past the seam. Checking that both strings appear somewhere would
    # pass if every keystroke went to the same shell - and checking for them at the start of a line
    # fails for a reason that is not about panes at all, because both shells answer at once and
    # their output shares a row.
    leftCol=$(pane | awk '/TYPED-LEFT/ {print index($0, "TYPED-LEFT"); exit}')
    rightCol=$(pane | awk '/TYPED-RIGHT/ {print index($0, "TYPED-RIGHT"); exit}')
    # Which side of the seam each landed on, which is the actual claim. Asserting column 1 for the
    # left one was wrong twice over: the first match is the line where the shell echoed the command
    # back, not its output, and a prompt occupies the first columns anyway.
    if [ -n "$leftCol" ] && [ "$leftCol" -lt 30 ] && [ -n "$rightCol" ] && [ "$rightCol" -gt 30 ]; then
        ok "keystrokes go to the focused pane, and Ctrl-] o changes which that is"
    else
        bad "typing landed in the wrong pane: left at column ${leftCol:-none}, right at ${rightCol:-none}"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm sh1 sh2 >/dev/null 2>&1

    # ------------------------------------------- what gozellij says has to be readable
    #
    # In a rendered attach, standard error is covered by the next repaint within milliseconds: a
    # service exiting wrote "exited with code 3" to a screen that erased it before anyone could
    # read it. Messages go to the status line now, for a few seconds.
    # Two services, because with one the attach ends the moment it exits and there is no screen
    # left to say anything on. The first version of this check did exactly that and reported an
    # empty status line, which was true and about the wrong thing.
    # Named so that the second immediately follows the first in sorted order: a split opens the
    # next service there is, and the services left behind by earlier checks are in that order too.
    # Without this the split opened `stubborn` from the tree-kill section and the check reported a
    # status line that was perfectly correct about something else.
    only
    "$gz" add msg1 -start -- sh -c 'printf "STILL-HERE\r\n"; sleep 120' >/dev/null 2>&1
    "$gz" add msg2 -start -- sh -c 'printf "ABOUT-TO-FAIL\r\n"; sleep 5; exit 3' >/dev/null 2>&1
    sleep 1
    tmux -L "$tmuxSock" new-session -d -x 60 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render msg1; echo BACK-IN-SHELL; sleep 60'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 6

    if pane | sed -n '10p' | grep -q 'exited with code 3'; then
        ok "a service's exit is said on the status line, where it can be read"
    else
        bad "the status line does not mention the failure: $(pane | sed -n '10p')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm msg1 msg2 >/dev/null 2>&1

    # -------------------------------------- a split screen survives the daemon being replaced
    #
    # `gozellij upgrade` keeps every process running, and that promise is worth less than it sounds
    # if the screen showing them has to be rebuilt afterwards. Both panes must still be live: the
    # failure this catches was silent, because two frozen panes look exactly like two idle shells.
    only
    "$gz" add tickone -start -- sh -c 'i=0; while :; do printf "one-%d\r\n" $i; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
    "$gz" add ticktwo -start -- sh -c 'i=0; while :; do printf "two-%d\r\n" $i; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
    sleep 1
    tmux -L "$tmuxSock" new-session -d -x 60 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render tickone'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 3

    beforeUpgrade=$(pane | head -1)
    "$gz" upgrade >/dev/null 2>&1
    sleep 6
    afterUpgrade=$(pane | head -1)
    if printf '%s' "$beforeUpgrade" | grep -q 'one-' && [ "$beforeUpgrade" != "$afterUpgrade" ] \
       && printf '%s' "$afterUpgrade" | grep -q 'two-'; then
        ok "both panes of a split screen stay live across a daemon upgrade"
    else
        bad "after the upgrade the panes read [$afterUpgrade], before [$beforeUpgrade]"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm tickone ticktwo >/dev/null 2>&1

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm renderdemo rendertwo >/dev/null 2>&1

    # ------------------------------------------------ looking back through a rendered pane
    #
    # A rendered attach paints the whole screen, which takes the user's own terminal scrollback
    # away: the lines that scrolled past are in the emulator's grid, not in the terminal's history.
    # Giving them back is not a nicety, it is repairing something the mode broke.
    "$gz" add scroller -start -- sh -c 'i=1; while [ $i -le 60 ]; do printf "row-%02d\r\n" $i; i=$((i+1)); done; sleep 120' >/dev/null 2>&1
    sleep 1
    tmux -L "$tmuxSock" new-session -d -x 40 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e GOZELLIJ_RENDER=1 -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach scroller'"
    sleep 3

    live=$(pane | sed -n '8p')
    tmux -L "$tmuxSock" send-keys C-] 'b'
    sleep 1
    back=$(pane | sed -n '8p')
    if [ -n "$live" ] && [ "$back" != "$live" ] && pane | grep -q 'row-'; then
        ok "Ctrl-] b looks back through a rendered pane's scrollback"
    else
        bad "scrolling back changed nothing: still $back"
    fi

    tmux -L "$tmuxSock" send-keys C-] 'g'
    sleep 1
    if [ "$(pane | sed -n '8p')" = "$live" ]; then
        ok "Ctrl-] g returns to the live screen"
    else
        bad "after Ctrl-] g the screen is $(pane | sed -n '8p'), want $live"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm scroller >/dev/null 2>&1

    # ------------------------------------- the emulator against tmux, on a real program, for real
    #
    # Everything else here checks that gozellij does what gozellij intends. This checks that its
    # terminal emulator agrees with a different one about what a real program's output means: the
    # same editor, the same file, the same keystroke, one through gozellij and one straight into
    # tmux at the size gozellij gives the service. The screens must match.
    #
    # vim on a file with wide characters and an accent, because the subject has to exercise
    # something. The first version of this check used a pager on a file of numbers, and three
    # deliberate breakages of the emulator - erase-in-line, the right margin, cursor positioning -
    # all passed it: ASCII in a pager touches almost none of an emulator.
    #
    # Both sides get the same TERM and the same LANG. Without that, the service inherits the
    # daemon's environment, which under `env -i` has no locale at all - vim then falls back to
    # latin1 and draws each byte of a wide character as its own blue placeholder. That is vim
    # being told something different, not an emulator disagreeing, and it made the two screens
    # differ for a reason that had nothing to do with what is being tested.
    #
    # Rows 1 to 22 only: row 23 is vim's message line, and attaching sends a resize which makes vim
    # clear it. That is about when each vim was told its size, not about either emulator.
    printf 'alpha\n\xe6\x97\xa5\xe6\x9c\xac\xe8\xaa\x9e line\ncaf\xc3\xa9\nlast\n' > "$home/wide-one.txt"
    cp "$home/wide-one.txt" "$home/wide-two.txt"
    "$gz" add vimmy -start -- sh -c "TERM=xterm-256color LANG=C.UTF-8 exec vim -u NONE -N -n $home/wide-one.txt" >/dev/null 2>&1
    sleep 1
    plain="${tmuxSock}-plain"
    tmux -L "$tmuxSock" new-session -d -x 80 -y 24 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e GOZELLIJ_RENDER=1 -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach vimmy'"
    tmux -L "$plain" new-session -d -x 80 -y 23 -e TERM=xterm-256color -e LANG=C.UTF-8 \
        "sh -c 'stty -echo; exec vim -u NONE -N -n $home/wide-two.txt'"
    sleep 3
    tmux -L "$tmuxSock" send-keys G
    tmux -L "$plain" send-keys G
    sleep 2

    # With -e, so that colours are compared and not just text. Without it the comparison is blind
    # to every attribute: breaking bright colours in the emulator passed this check, because
    # capture-pane without -e returns the characters and nothing about how they are drawn.
    tmux -L "$tmuxSock" capture-pane -pe | head -22 > "$home/screen-gozellij.txt"
    tmux -L "$plain" capture-pane -pe | head -22 > "$home/screen-tmux.txt"
    if ! grep -q 'alpha' "$home/screen-tmux.txt"; then
        bad "the reference editor drew nothing, so this comparison would be vacuous"
    elif ! grep -q '日本語' "$home/screen-tmux.txt"; then
        bad "the reference editor did not draw the wide characters, so this comparison proves less than it claims"
    elif diff -q "$home/screen-gozellij.txt" "$home/screen-tmux.txt" >/dev/null; then
        ok "gozellij's emulator and tmux's draw a real editor identically, wide characters included"
    else
        bad "the two emulators disagree: $(diff "$home/screen-gozellij.txt" "$home/screen-tmux.txt" | head -4 | tr '\n' ' ')"
    fi

    tmux -L "$plain" kill-server 2>/dev/null
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm vimmy >/dev/null 2>&1

    # Not checked here: that attaching does not overwrite the line you typed the command on.
    #
    # It is a real bug when it happens - reserving the bottom row used to land the cursor on the
    # last line of your content, and the replayed prompt printed over it, leaving it in neither
    # the screen nor the scrollback - but two attempts to reproduce it from this script did not,
    # and a check that passes when the fix is removed is worse than no check. It reproduces by
    # hand; the recipe is in docs/REPLACING_GEZELLIJ.md so that anyone can repeat it.
fi

say
if [ "$fail" -eq 0 ]; then
    say "all $pass promises held."
else
    say "$pass held, $fail did not."
fi
exit $(( fail > 0 ? 1 : 0 ))
