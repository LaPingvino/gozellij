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

# These scripts test gozellij from the outside, and may be run from inside it - from a gozellij
# shell, which is where anybody using it day to day will run them. Every pane they start would
# inherit GOZELLIJ from that shell, and `gozellij attach` now refuses to nest, so every attach in
# the suite would be refused as nesting. The same goes for the multiplexers the login checks
# already clear by hand.
unset GOZELLIJ ZELLIJ ZELLIJ_SESSION_NAME TMUX STY

keep=0
noscreen=0
for arg in "$@"; do
    case "$arg" in
        -k) keep=1 ;;
        # Skip the section that drives a real terminal. Twenty-two of the promises live outside
        # it and ninety-one inside, so this is most of a six-minute run - useful when the change
        # you are testing cannot reach a screen, and dishonest as a default, which is why it is
        # not one and why the summary says what was left out.
        -noscreen) noscreen=1 ;;
        -h|--help)
            printf 'usage: %s [-k] [-noscreen]\n  -k         keep the work directory (daemon.log survives)\n  -noscreen  skip the checks that need tmux and a real terminal\n' "$0"
            exit 0 ;;
        *) printf 'unknown argument: %s (try -h)\n' "$arg" >&2; exit 2 ;;
    esac
done

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

# On the sleeps, because they are the first thing anybody tries to remove.
#
# There are about 216 seconds of literal `sleep` in this script and the whole run takes five
# minutes, so they look like the obvious target. Measured: converting the 24 that follow an attach
# into "wait until the pane has drawn" saved roughly 50 seconds and broke five checks, because
# several of those attaches redirect their output to a file and their pane is *meant* to stay
# blank. Fifty seconds of three hundred, for a heuristic that is right eighty percent of the time,
# on the script that guards everything else. It was reverted.
#
# What is left is inherent rather than lazy: 31 tmux servers, a daemon per section, and real
# programs drawing on real terminals. A materially faster suite means running sections in parallel,
# which is a different piece of work and a real one - not a search-and-replace on sleeps.
#
# tmuxSock is a private tmux server, so nothing here can touch a session you are using.
#
# A new one per screen section, not one for the whole run. Sections used to share a server and rely
# on kill-server having taken effect before the next new-session; when it had not, a check read the
# previous section's window. That produced a check reporting a pane as stalled when a run by hand
# showed it live three times out of three, and before that a check that asked the wrong pane for
# its scrolling region. Shared state between checks has now caused three separate false results,
# and a socket name costs nothing.
tmuxSockSeq=0
tmuxSock="gozellij-acceptance-$$-0"

# newscreen starts a fresh tmux server for the next section and leaves the old one killed.
newscreen() {
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    tmuxSockSeq=$((tmuxSockSeq + 1))
    tmuxSock="gozellij-acceptance-$$-$tmuxSockSeq"
}

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
    # That the new binary is really at that path before anything asks the daemon to exec it.
    #
    # Without this, a build that had not finished landing shows up later as "the upgrade did not
    # report the new version" - which reads as a broken upgrade when it is a v2 that was never
    # there. Seen once, not reproducible in two runs afterwards, and this is what will say which
    # half it was next time.
    if "$gzd" -version 2>&1 | grep -q 'acceptance-v2'; then
        ok "the replacement binary is in place before the upgrade is asked for"
    else
        bad "v2 is not at $gzd: $("$gzd" -version 2>&1 | head -1)"
    fi

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
    # Ask the daemon directly as well as reading what upgrade printed. This check has failed twice
    # with "did not report the new version" while the replacement binary was demonstrably on disk
    # and the upgrade path works deterministically when driven on its own - so the question is
    # whether the daemon failed to exec, or exec'd and the client reported the wrong thing. One
    # message cannot say which, and a check that fails occasionally and cannot explain itself gets
    # ignored, which is worse than not having it.
    daemon_says=$("$gz" doctor 2>/dev/null | awk '/^ok    daemon version/{print $4}')
    if grep -q 'acceptance-v2' "$work/upgrade.out" 2>/dev/null; then
        ok "the daemon really is the new binary afterwards"
    elif [ "$daemon_says" = "acceptance-v2" ]; then
        bad "the daemon IS the new binary (it says $daemon_says) but the upgrade command did not say so: $(cat "$work/upgrade.out" 2>/dev/null | tr '\n' ' ')"
    else
        bad "the daemon is still [$daemon_says] after the upgrade, so the exec did not take: $(cat "$work/upgrade.out" 2>/dev/null | tr '\n' ' ')"
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
# ------------------------------------------- takeover tells you the truth either way
#
# The last command with nothing driving it, and the reason it was left is real: takeover reads
# /proc for other multiplexers, so what it finds depends on the machine. That is an argument
# against asserting *what* it says, not against asserting that it is honest.
#
# Two shapes are allowed and nothing else. Either there is nothing to take over and it says so, or
# it found something and says both what it found and that it cannot adopt it - because the
# terminal lives inside that process and the kernel will not hand it over. What it must never do
# is claim to have taken something over, or print a list and leave somebody believing their
# session moved.
only
tookout=$("$gz" takeover 2>&1)
tookrc=$?

if [ "$tookrc" = "0" ]; then
    ok "takeover exits cleanly whatever it finds"
else
    bad "takeover exited $tookrc saying: $(printf '%s' "$tookout" | head -2 | tr '\n' '|')"
fi

if printf '%s' "$tookout" | grep -qiE "nothing to take over|no other multiplexer"; then
    ok "and with nothing to take over it says so"
elif printf '%s' "$tookout" | grep -q "cannot take these over" &&
     printf '%s' "$tookout" | grep -q "gozellij add"; then
    # The branch this machine takes, since gezellij is what gozellij replaced: it names what it
    # found, says plainly that it cannot adopt those terminals, and gives the command that
    # recreates them under gozellij.
    ok "and having found something it says it cannot adopt it, and what to type instead"
else
    bad "takeover printed neither shape: $(printf '%s' "$tookout" | head -4 | tr '\n' '|')"
fi

# Whatever it printed, it did not claim to have done anything. The word is the test: a takeover
# that said "took over" would be lying on this machine, because it cannot.
if ! printf '%s' "$tookout" | grep -qiE "took over|taken over|moved [0-9]+ "; then
    ok "and it never claims to have taken anything over"
else
    bad "takeover claimed to have done something: $(printf '%s' "$tookout" | grep -iE 'took over|taken over|moved' | head -2 | tr '\n' '|')"
fi

# ------------------------------------ a service file you can read, repair, and break
#
# Design rule 5: state the fabric must not lose goes on disk, in a format a human can read and
# repair. That is an escape hatch, and an escape hatch nobody has tried is a decoration. The fabric
# has a unit test for re-reading a hand-edited definition; what had nothing was the whole path -
# edit the file with an editor, ask gozellij to restart, get the thing you edited.
#
# And the other half, which is where rule 5 meets rule 1: an editing mistake. A definition that
# does not parse was logged by the daemon and mentioned nowhere a person looks - `ls` said "no
# services defined", which is exactly what it says when you have none. Somebody whose service
# vanished after a hand edit could only learn why from the daemon's log.
only
"$gz" add editable -start -restart no -- sh -c 'printf "BEFORE-THE-EDIT\r\n"; sleep 300' >/dev/null 2>&1
sleep 2
def="$state/services/editable.json"

if [ -f "$def" ] && grep -q '"command"' "$def" && grep -q '^  "' "$def"; then
    ok "a service is on disk as JSON with one field per line, which a person can edit"
else
    bad "the definition is: $(head -3 "$def" 2>/dev/null | tr '\n' '|')"
fi

# The escape hatch itself: change it with an editor, restart, get what you wrote.
sed -i 's/BEFORE-THE-EDIT/AFTER-THE-EDIT/' "$def"
"$gz" restart editable >/dev/null 2>&1
sleep 2
if "$gz" logs editable 2>/dev/null | grep -q 'AFTER-THE-EDIT'; then
    ok "and a hand edit is honoured on the next start"
else
    bad "after editing the file the service still said: $("$gz" logs editable 2>/dev/null | tail -2 | tr '\n' '|')"
fi

# And an editing mistake is reported where somebody would look for it. Checked through doctor
# rather than the daemon's log, because the log is not where a person goes.
"$gz" stop editable >/dev/null 2>&1
printf 'not json at all\n' > "$def"
#
# Matched on the file's own name, which only this check prints. Written first as a grep for
# "service definitions" and that passed with the check deleted: the state-directory warning says
# "other users can read your service definitions", so the words were already on screen. Four
# assertions today have passed on something the system produced anyway; this is the fourth.
if "$gz" doctor 2>/dev/null | grep -q 'editable.json'; then
    ok "and a definition that cannot be read is named by doctor, not just logged"
else
    bad "doctor does not name the broken file: $("$gz" doctor 2>/dev/null | grep -iE 'definition|json' | tr '\n' '|')"
fi
rm -f "$def"
"$gz" rm editable >/dev/null 2>&1

# ------------------------------------------------ when there is no daemon at all
#
# Every other check here runs with a daemon up, because the suite starts one. The state nobody
# covered is the one somebody meets first: a reboot with linger off, a unit that is not enabled,
# a machine where nothing has started yet. Typing `gozellij` then has to say what is wrong and how
# to fix it, and `doctor` has to keep working - being unable to reach the daemon is precisely when
# a person runs doctor, so a doctor that needs one would be useless exactly when it is wanted.
#
# Its own empty runtime directory, so there is genuinely nothing to talk to.
only
emptyrun=$(mktemp -d "$work/nodaemon.XXXXXX")
nod() { GOZELLIJ_RUNTIME_DIR="$emptyrun" GOZELLIJ_STATE_DIR="$state" "$gz" "$@"; }

nodout=$(nod ls 2>&1)
nodrc=$?
if [ "$nodrc" != "0" ]; then
    ok "a command that needs the daemon fails rather than printing nothing"
else
    bad "ls with no daemon exited 0 saying: $(printf '%s' "$nodout" | head -2 | tr '\n' '|')"
fi

# Which socket it looked for, because somebody running two of these - a test one and a real one -
# needs to know which is missing, and because the path is the thing they can check.
if printf '%s' "$nodout" | grep -q "$emptyrun"; then
    ok "and it names the socket it looked for"
else
    bad "it did not say where it looked: $(printf '%s' "$nodout" | head -2 | tr '\n' '|')"
fi

if printf '%s' "$nodout" | grep -q 'gozellijd'; then
    ok "and how to start one"
else
    bad "it did not say how to start a daemon: $(printf '%s' "$nodout" | head -2 | tr '\n' '|')"
fi

# doctor is the one that must survive it. A FAIL line about the daemon, and the rest of its checks
# still run - that is the difference between a diagnostic and another thing that is broken.
docout=$(nod doctor 2>&1)
if printf '%s' "$docout" | grep -qE '^FAIL +daemon'; then
    ok "and doctor reports the missing daemon as a failure"
else
    bad "doctor says: $(printf '%s' "$docout" | grep -iE 'daemon' | head -2 | tr '\n' '|')"
fi
if [ "$(printf '%s' "$docout" | grep -cE '^(ok|warn|note|FAIL)')" -ge 4 ]; then
    ok "and keeps running its other checks without one"
else
    bad "doctor printed only $(printf '%s' "$docout" | grep -cE '^(ok|warn|note|FAIL)') lines without a daemon"
fi

# ----------------------------------------- gozellijd -logs off keeps output off the disk
#
# A privacy promise with nothing checking it. A shell you live in has everything you typed and
# everything it answered in its transcript - which is why `-logs off` exists for the whole daemon
# and `add -log off` for one service - and a flag that quietly stopped working would write all of
# it while somebody believed it was not being written. Silence is the failure mode, which is the
# argument for checking it rather than trusting it.
#
# Its own daemon, on its own directories, because the flag is set when the daemon starts and the
# suite's daemon is already up. None of gozellijd's flags had ever been driven from here: the
# suite configures it by environment, so -socket, -state, -logs and -v were all untested.
only
offrun=$(mktemp -d "$work/offrun.XXXXXX")
offstate=$(mktemp -d "$work/offstate.XXXXXX")
env -i PATH="/usr/bin:/bin" HOME="$home" \
    GOZELLIJ_RUNTIME_DIR="$offrun" GOZELLIJ_STATE_DIR="$offstate" \
    "$gzd" -logs off >>"$work/daemon-logsoff.log" 2>&1 &
offpid=$!
offok=0
for _ in $(seq 50); do
    GOZELLIJ_RUNTIME_DIR="$offrun" GOZELLIJ_STATE_DIR="$offstate" "$gz" ping >/dev/null 2>&1 && { offok=1; break; }
    sleep 0.1
done

if [ "$offok" = 1 ]; then
    off() { GOZELLIJ_RUNTIME_DIR="$offrun" GOZELLIJ_STATE_DIR="$offstate" "$gz" "$@"; }
    # The marker is computed by the service, not written in its command: the command itself is
    # recorded in the service definition on disk, as it has to be, so a literal would be found
    # there and read as a leak. Only output can contain the answer.
    off add hushed -start -restart no -- sh -c 'printf "OFFDISK-%s\r\n" $((6*7)); sleep 120' >/dev/null 2>&1
    sleep 2

    if [ -z "$(find "$offstate" -name '*.log' -type f 2>/dev/null | head -1)" ]; then
        ok "gozellijd -logs off writes no log file at all"
    else
        bad "it wrote $(find "$offstate" -name '*.log' -type f | head -2 | tr '\n' ' ')"
    fi

    if [ -z "$(grep -rl 'OFFDISK-42' "$offstate" "$offrun" 2>/dev/null | head -1)" ]; then
        ok "and what the service printed is nowhere on disk"
    else
        bad "the output reached $(grep -rl 'OFFDISK-42' "$offstate" "$offrun" 2>/dev/null | head -2 | tr '\n' ' ')"
    fi

    # Off the disk is not the same as gone: it is still in memory, which is what makes the flag
    # usable rather than merely safe.
    if off logs hushed 2>/dev/null | grep -q 'OFFDISK-42'; then
        ok "and logs still reads it from memory"
    else
        bad "logs said: $(off logs hushed 2>&1 | head -2 | tr '\n' '|')"
    fi

    if [ "$(off ls 2>/dev/null | awk '$1 == "hushed" {print $7}')" = "-" ]; then
        ok "and ls says so rather than showing a size that does not exist"
    else
        bad "ls shows the log column as [$(off ls 2>/dev/null | awk '$1 == "hushed" {print $7}')]"
    fi

    off stop hushed >/dev/null 2>&1
else
    bad "the -logs off daemon never answered; see $work/daemon-logsoff.log"
fi
[ -n "${offpid:-}" ] && kill "$offpid" 2>/dev/null

# ------------------------------------------------- the mistakes you make by typing
#
# The errors a person actually meets: a name already taken, a policy spelled wrong, a command
# forgotten, a service that is not there. Every one of them is already right - non-zero, and a
# message naming the thing - and not one was checked. Rule 1 has no exception for the unhappy
# path, and a command that fails quietly or exits 0 on a refusal is worse than one that fails
# loudly, because a script around it carries on.
#
# Measured first, and the measuring had to be done twice: the first pass read `$?` after a pipe
# into `head` and got head's status, so every one of these looked like exit 0. That mistake is in
# this project's notes twice already.
only
"$gz" add taken -start -restart no -- sh -c 'sleep 300' >/dev/null 2>&1
sleep 1

refused() {
    # refused <what> <expected text> <command...>
    what=$1; want=$2; shift 2
    out=$("$@" 2>&1); rc=$?
    if [ "$rc" != "0" ] && printf '%s' "$out" | grep -q "$want"; then
        ok "$what"
    else
        bad "$what: exit $rc saying $(printf '%s' "$out" | head -2 | tr '\n' '|')"
    fi
}

refused "adding a name that is already taken is refused"        'already exists' \
        "$gz" add taken -- sh -c 'sleep 1'
refused "and a restart policy that is not one says what the three are" 'no, on-failure or always' \
        "$gz" add spelt -restart sometimes -- sh -c 'sleep 1'
refused "and add with no command shows you one"                 'gozellij add' \
        "$gz" add nocommand
refused "and status of a service that is not there names it"    'no such service: ghost' \
        "$gz" status ghost
refused "and so does stop"                                      'no such service: ghost' \
        "$gz" stop ghost

"$gz" add mover -restart no -- sh -c 'sleep 300' >/dev/null 2>&1
refused "and renaming onto a name in use is refused"            'already exists' \
        "$gz" rename mover taken

# The one that is a behaviour rather than a message: rm on a running service takes the process
# with it. A remove that forgot the service and left its process running would leak one every
# time, with nothing left that knows how to stop it.
takenpid=$("$gz" status taken 2>/dev/null | awk '/^pid:/{print $2}')
"$gz" rm taken >/dev/null 2>&1
sleep 1
if [ -n "$takenpid" ] && ! kill -0 "$takenpid" 2>/dev/null; then
    ok "and rm takes a running service's process with it, rather than orphaning it"
else
    bad "rm left pid $takenpid running"
fi
"$gz" rm mover >/dev/null 2>&1

# --------------------------------------------- where a service runs, and what it runs with
#
# -dir and -env, neither of them driven. -dir appears once further down, but only because a `less`
# needs to find its file: it would fail if the flag were ignored, which is coverage by accident and
# says nothing about the flag if that check is ever rewritten. -env had nothing at all.
#
# Measured before it was written down. The interesting part is not that the variables arrive, it
# is that they are *added*: a service whose PATH disappeared because somebody set one variable
# would be a long afternoon, and nothing here would have noticed.
only
mkdir -p "$home/somewhere"
"$gz" add placed -dir "$home/somewhere" -env ONE=first -env TWO=second -start -restart no -- \
    sh -c 'echo "AT=[$PWD] ONE=[$ONE] TWO=[$TWO] HOME=[$HOME]"; sleep 120' >/dev/null 2>&1
sleep 2
saw=$("$gz" logs placed 2>/dev/null | grep 'AT=' | tail -1)

if printf '%s' "$saw" | grep -q "AT=\[$home/somewhere\]"; then
    ok "-dir runs the service where you said"
else
    bad "the service ran in: $(printf '%s' "$saw" | sed 's/ .*//')"
fi

# Repeatable, and both survive: one -env overwriting the other is the obvious way to get this
# half-right.
if printf '%s' "$saw" | grep -q 'ONE=\[first\]' && printf '%s' "$saw" | grep -q 'TWO=\[second\]'; then
    ok "and -env is repeatable, with every one of them arriving"
else
    bad "the service saw: $saw"
fi

# Added, not substituted. The service still has the environment it would have had.
#
# HOME, not PATH, and that is the whole check. The first version asked for PATH and passed against
# a build that inherited nothing at all: a shell started with an empty environment invents a
# default PATH for itself, so ${PATH:+set} is true either way and the assertion could not fail.
# Measured: `env -i /bin/sh -c 'echo $PATH $HOME'` prints a PATH and an empty HOME. HOME is the
# one the shell will not make up.
if printf '%s' "$saw" | grep -q "HOME=\[$home\]"; then
    ok "and it adds to the environment rather than replacing it"
else
    bad "setting -env cost the service the rest of its environment: $saw"
fi
"$gz" stop placed >/dev/null 2>&1
"$gz" rm placed >/dev/null 2>&1

# ------------------------------------------- the restart policy you asked for is the one you get
#
# The policies themselves are well covered by unit tests - no, on-failure and always, the backoff,
# the flapping history, nine tests between them. What none of those can see is the wiring: the
# flag parsed by the CLI, written to the registry, read back and honoured by a supervisor in
# another process. A value that is parsed correctly and then dropped on the way would pass every
# one of them, which is exactly how the configured prefix was broken for everybody who set one.
only
"$gz" add comesback -restart always -start -- sh -c 'sleep 1; exit 0' >/dev/null 2>&1
"$gz" add staysdead -restart no -start -- sh -c 'sleep 1; exit 0' >/dev/null 2>&1
"$gz" add onlyfails -restart on-failure -start -- sh -c 'sleep 1; exit 0' >/dev/null 2>&1

# Long enough for a couple of one-second runs and the backoff between them.
for _ in $(seq 24); do
    [ "$("$gz" status comesback 2>/dev/null | awk '/^starts:/{print $2}')" -gt 1 ] 2>/dev/null && break
    sleep 0.5
done

starts=$("$gz" status comesback 2>/dev/null | awk '/^starts:/{print $2}')
if [ "${starts:-0}" -gt 1 ]; then
    ok "-restart always brings a service back after it exits (started $starts times)"
else
    bad "a service set to always restart started $starts time(s)"
fi

if [ "$("$gz" status staysdead 2>/dev/null | awk '/^starts:/{print $2}')" = "1" ]; then
    ok "and -restart no leaves it where it fell"
else
    bad "a service set not to restart started $("$gz" status staysdead 2>/dev/null | awk '/^starts:/{print $2}') times"
fi

# on-failure and a clean exit: nothing failed, so nothing is restarted. The distinction is the
# whole point of the policy having three values rather than two.
if [ "$("$gz" status onlyfails 2>/dev/null | awk '/^starts:/{print $2}')" = "1" ]; then
    ok "and -restart on-failure lets a clean exit be the end of it"
else
    bad "on-failure restarted a service that exited 0: started $("$gz" status onlyfails 2>/dev/null | awk '/^starts:/{print $2}') times"
fi

# And the same policy, with a failure this time, does come back.
"$gz" add failsalot -restart on-failure -start -- sh -c 'sleep 1; exit 3' >/dev/null 2>&1
for _ in $(seq 24); do
    [ "$("$gz" status failsalot 2>/dev/null | awk '/^starts:/{print $2}')" -gt 1 ] 2>/dev/null && break
    sleep 0.5
done
if [ "$("$gz" status failsalot 2>/dev/null | awk '/^starts:/{print $2}')" -gt 1 ] 2>/dev/null; then
    ok "and on-failure does bring one back when it actually fails"
else
    bad "on-failure did not restart a service that exited 3: started $("$gz" status failsalot 2>/dev/null | awk '/^starts:/{print $2}') time(s)"
fi

"$gz" stop comesback staysdead onlyfails failsalot >/dev/null 2>&1
"$gz" rm comesback staysdead onlyfails failsalot >/dev/null 2>&1

# ------------------------------------------------- restart, and what it must not throw away
#
# A core lifecycle command with nothing driving it. `restart` is listed in the document among the
# commands that take several names and is marked **Verified** there on the strength of the others
# being checked - stop, start and rm all are, restart never was.
#
# It carries a promise the others do not, and that promise is the interesting part:
# replaceSupervisor deliberately reuses the output buffer, so `logs` still shows what a service
# said before it was restarted. An operator does not care whether a process died or was replaced;
# losing the transcript at the moment you restart something is losing it exactly when you are
# trying to find out what went wrong.
only
"$gz" add bouncer -start -restart no -- sh -c 'echo SAID-BEFORE-RESTART; sleep 300' >/dev/null 2>&1
sleep 2
before=$("$gz" status bouncer 2>/dev/null | awk '/^pid:/{print $2}')
"$gz" restart bouncer >/dev/null 2>&1
sleep 2
after=$("$gz" status bouncer 2>/dev/null | awk '/^pid:/{print $2}')

if [ -n "$before" ] && [ -n "$after" ] && [ "$before" != "$after" ]; then
    ok "restart really replaces the process ($before became $after)"
else
    bad "the pid was $before and is now $after"
fi
if "$gz" status bouncer 2>/dev/null | grep -q '^state: *running'; then
    ok "and the service is running afterwards, not merely stopped"
else
    bad "after a restart the state is: $("$gz" status bouncer 2>/dev/null | grep '^state:')"
fi

# The transcript survives a restart. Worth checking on its own - a restart that quietly emptied
# the log would look like a working restart right up to the moment somebody needed the log.
#
# It does NOT check what the comment here first claimed. This passes because the log *file* is on
# disk and `logs` reads it; replaceSupervisor carrying the output buffer across has nothing to do
# with it. Dropping that carry entirely left all 151 promises standing, which is how the wrong
# reason was found - the check was right and the sentence next to it was not. The carry gets its
# own check below, where there is no file to hide behind.
if "$gz" logs bouncer 2>/dev/null | grep -q 'SAID-BEFORE-RESTART'; then
    ok "and what it said before the restart is still in its log"
else
    bad "the log lost everything from before: $("$gz" logs bouncer 2>/dev/null | tail -2 | tr '\n' '|')"
fi

# And now the carry itself, with `-log off` so there is no file at all. This is the promise in
# replaceSupervisor - "the output buffer is reused, so logs still shows what the service said
# before it was stopped" - and until now nothing anywhere exercised it: every other service in
# this suite writes to disk, which answers the question before the buffer is ever consulted.
#
# Counting rather than matching, because a restart re-runs the same command and prints the same
# line again. Two occurrences means one was kept from before; one means the buffer went with the
# supervisor that was replaced.
"$gz" add nofile -log off -start -restart no -- sh -c 'echo CARRIED-ACROSS; sleep 300' >/dev/null 2>&1
sleep 2
if [ -f "$state/logs/nofile.log" ]; then
    bad "-log off wrote a log file anyway"
else
    ok "-log off keeps a service's output off the disk entirely"
fi
"$gz" restart nofile >/dev/null 2>&1
sleep 2
carried=$("$gz" logs nofile 2>/dev/null | grep -c 'CARRIED-ACROSS')
if [ "${carried:-0}" -ge 2 ]; then
    ok "and a restart keeps what it said before, with no file to read it from"
else
    bad "after restarting a service with no log file, its output appeared $carried time(s), want 2"
fi
"$gz" stop nofile >/dev/null 2>&1
"$gz" rm nofile >/dev/null 2>&1

# Several names at once, like its neighbours - and a bad name among good ones reported without
# abandoning the rest, which is the rule the whole family follows.
"$gz" add bounce2 -start -restart no -- sh -c 'sleep 300' >/dev/null 2>&1
sleep 1
p1=$("$gz" status bouncer 2>/dev/null | awk '/^pid:/{print $2}')
p2=$("$gz" status bounce2 2>/dev/null | awk '/^pid:/{print $2}')
"$gz" restart bouncer bounce2 >/dev/null 2>&1
sleep 2
q1=$("$gz" status bouncer 2>/dev/null | awk '/^pid:/{print $2}')
q2=$("$gz" status bounce2 2>/dev/null | awk '/^pid:/{print $2}')
if [ "$p1" != "$q1" ] && [ "$p2" != "$q2" ] && [ -n "$q1" ] && [ -n "$q2" ]; then
    ok "one restart command restarts every service named"
else
    bad "restarting two: $p1->$q1 and $p2->$q2"
fi

restsaid=$("$gz" restart bouncer nosuchthing bounce2 2>&1)
reststatus=$?
if [ "$reststatus" != "0" ] && printf '%s' "$restsaid" | grep -q 'nosuchthing'; then
    ok "and a bad name among good ones is reported, with a non-zero status"
else
    bad "restarting a missing service exited $reststatus saying: $(printf '%s' "$restsaid" | tr '\n' '|' | head -c 120)"
fi
"$gz" stop bouncer bounce2 >/dev/null 2>&1
"$gz" rm bouncer bounce2 >/dev/null 2>&1

# ---------------------------------------- a broken environment is reported, not suffered
#
# A log directory that cannot be written is not exotic: a full disk, a mode somebody tightened,
# a state directory on a filesystem that went read-only. What a multiplexer must not do then is
# take your shells down with it, and what it must not do instead is carry on quietly while the
# transcript everybody assumes exists is not being written.
#
# gozellij already gets this right, and that is exactly why it needs checking: good behaviour
# nothing defends is good behaviour until somebody refactors. Every one of these was measured
# by hand first - the service keeps running, and four different places say what is wrong.
only
"$gz" add quiet1 -start -restart no -- sh -c 'echo BEFORE-BREAK; sleep 120' >/dev/null 2>&1
sleep 1
chmod 500 "$state/logs"
"$gz" add broke -start -restart no -- sh -c 'echo AFTER-BREAK; sleep 120' >/dev/null 2>&1
sleep 2
brokepid=$("$gz" status broke 2>/dev/null | awk '/^pid:/{print $2}')

# The service runs. This is the promise that matters: a disk problem is not a reason to lose
# the shell you are working in.
if [ -n "$brokepid" ] && "$gz" status broke 2>/dev/null | grep -q '^state: *running'; then
    ok "a service still starts when its log cannot be written"
else
    bad "the service did not run: $("$gz" status broke 2>/dev/null | tr '\n' '|' | head -c 150)"
fi

# And says so, in status, with the reason. Rule 1: not writing the log is a thing that did not
# happen, and something that did not happen has to be said.
if "$gz" status broke 2>/dev/null | grep -q 'log error:.*permission denied'; then
    ok "and status says why the log is not being written"
else
    bad "status says nothing about the log: $("$gz" status broke 2>/dev/null | grep -i log | tr '\n' '|')"
fi

# `logs` is the other place somebody looks, and it has the harder job: it still has the output
# in memory, so it can show it - but it must not let you think you are reading a file that is
# being kept up to date.
logsaid=$("$gz" logs broke 2>&1)
if printf '%s' "$logsaid" | grep -q 'not being written' && printf '%s' "$logsaid" | grep -q 'AFTER-BREAK'; then
    ok "and logs still shows the output while saying the file is not keeping up"
else
    bad "logs said: $(printf '%s' "$logsaid" | tr '\n' '|' | head -c 200)"
fi

# doctor is where somebody goes when they already suspect something, so it has to name the
# service rather than say the state directory looks odd.
if "$gz" doctor 2>/dev/null | grep -q 'logs: broke'; then
    ok "and doctor names the service whose log is failing"
else
    bad "doctor says: $("$gz" doctor 2>/dev/null | grep -i log | tr '\n' '|' | head -c 200)"
fi

# The service that was already writing before the break is not dragged down with it.
if "$gz" status quiet1 2>/dev/null | grep -q '^state: *running'; then
    ok "and a service that was already logging is unaffected"
else
    bad "the earlier service stopped: $("$gz" status quiet1 2>/dev/null | tr '\n' '|' | head -c 120)"
fi

chmod 700 "$state/logs"
"$gz" stop broke quiet1 >/dev/null 2>&1
"$gz" rm broke quiet1 >/dev/null 2>&1

# ------------------------------------------ the package that replaces the other multiplexer
#
# The PKGBUILD is the only thing standing between a commit here and the machine somebody
# actually uses. A typo in it is not a small bug: it is "I cannot upgrade", and it is found at
# the worst moment, by the person who wanted the fix.
#
# Nothing checked it. makepkg --printsrcinfo sources the file and prints what it declares,
# offline and without sudo, so the parts that matter can be asserted rather than assumed - and
# the parts that matter are the ones that let it take a machine over from gezellij. Getting
# `replaces` wrong does not fail loudly; it leaves both installed, two login blocks, and a
# fight over the terminal.
only
if ! command -v makepkg >/dev/null 2>&1; then
    say "  SKIP  the packaging checks, because makepkg is not installed"
else
    srcinfo=$(cd "$repo/packaging/arch" && timeout 60 makepkg --printsrcinfo 2>&1)
    if [ -n "$srcinfo" ] && printf '%s' "$srcinfo" | grep -q '^pkgbase = gozellij-git'; then
        ok "the PKGBUILD parses and declares itself"
    else
        bad "makepkg could not read the PKGBUILD: $(printf '%s' "$srcinfo" | head -3 | tr '\n' '|')"
    fi

    # The whole point of the package: it takes over from the other one rather than sitting
    # beside it.
    if printf '%s' "$srcinfo" | grep -q 'replaces = gezellij-git' &&
       printf '%s' "$srcinfo" | grep -q 'conflicts = gezellij-git'; then
        ok "and it replaces and conflicts with gezellij-git, so pacman swaps them"
    else
        bad "the package does not replace gezellij-git: $(printf '%s' "$srcinfo" | grep -E 'replaces|conflicts' | tr '\n' '|')"
    fi

    # Every file the package build reaches for, checked from the recipe rather than believed.
    # A rename in the repo that nobody carried into the PKGBUILD fails at `makepkg`, on the
    # machine of the person upgrading.
    missing=""
    for f in packaging/systemd/gozellijd.service packaging/arch/gozellij-git.install LICENSE; do
        [ -f "$repo/$f" ] || missing="$missing $f"
    done
    if [ -z "$missing" ]; then
        ok "and every file its package() step installs is in the repository"
    else
        bad "the PKGBUILD installs files that are not here:$missing"
    fi

    # The unit it generates is the repo's with ExecStart rewritten to the installed path. If
    # that sed stops matching, the unit ships pointing at somebody's build directory.
    generated=$(sed 's|^ExecStart=.*|ExecStart=/usr/bin/gozellijd|' "$repo/packaging/systemd/gozellijd.service")
    if printf '%s' "$generated" | grep -q '^ExecStart=/usr/bin/gozellijd$'; then
        ok "and the unit it generates starts the installed daemon, not a build directory"
    else
        bad "the generated unit says: $(printf '%s' "$generated" | grep -i execstart | tr '\n' '|')"
    fi
fi

# So this section drives a real terminal of a known size and reads the screen back. tmux is the
# tool to hand; a private server, so it cannot touch a session you are using.
if [ "$noscreen" = 1 ]; then
    say "  SKIP  the screen checks, because -noscreen was given"
elif ! command -v tmux >/dev/null 2>&1; then
    say "  SKIP  the screen checks need tmux, which is not installed"
else
    pane() { tmux -L "$tmuxSock" capture-pane -p "$@"; }
    ask()  { tmux -L "$tmuxSock" display -p "$1"; }

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    newscreen
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
    newscreen
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
    # Anchored to the start of the line, not merely present on it. The tab bar also brackets the
    # service you are in, so a loose grep would match that instead and this check would pass
    # whether or not the pane marker existed at all - which is how a check quietly stops checking
    # when something else on the same row learns to look like it.
    if pane | sed -n '12p' | grep -q '^renderdemo \[rendertwo\]'; then
        ok "the status line marks which pane the keyboard is going to"
    else
        bad "the pane marker is wrong: $(pane | sed -n '12p')"
    fi

    tmux -L "$tmuxSock" send-keys C-] 'o'
    sleep 2
    if pane | sed -n '12p' | grep -q '^\[renderdemo\] rendertwo'; then
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

    # ------------------------------------------------ the help key has to answer where you look
    #
    # Ctrl-] ? exists to tell you what the keys are. In a rendered attach it wrote its answer to
    # standard error, which the next paint covered within milliseconds - a help key that helps
    # nobody. The same went for the message saying the key you pressed does nothing.
    only
    "$gz" add helpy -start -- sh -c 'printf "HELP-DEMO\r\n"; sleep 120' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render helpy'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '?'
    sleep 2
    if pane | sed -n '8p' | grep -q 'detach'; then
        ok "Ctrl-] ? answers on the status line, where it can be read"
    else
        bad "the help key said nothing visible: $(pane | sed -n '8p')"
    fi

    tmux -L "$tmuxSock" send-keys C-] 'z'
    sleep 2
    if pane | sed -n '8p' | grep -q 'does nothing'; then
        ok "a key that does nothing says so"
    else
        bad "an unknown key was swallowed: $(pane | sed -n '8p')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm helpy >/dev/null 2>&1

    # --------------------------------------------- the picker has to be visible to be a picker
    #
    # `Ctrl-] l` draws a menu and waits for a keystroke. In a rendered attach the repaint that
    # keeps the status clock moving was drawing the last frame straight over it, so the question
    # was invisible while the answer still worked - a menu answered by guesswork.
    only
    for n in pika pikb pikc; do
        "$gz" add "$n" -start -- sh -c "i=0; while :; do printf '$n-%d\r\n' \$i; i=\$((i+1)); sleep 1; done" >/dev/null 2>&1
    done
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render pika'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] 'l'
    # Longer than one repaint interval, which is what used to erase it.
    sleep 3

    if pane | grep -q 'pick a service' && pane | grep -q '3 pikc'; then
        ok "Ctrl-] l shows its menu in a rendered attach, and it stays up"
    else
        bad "the picker's menu is not on screen: $(pane | head -2 | tr '\n' '|')"
    fi

    tmux -L "$tmuxSock" send-keys '3'
    sleep 3
    if pane | grep -q 'pikc-'; then
        ok "choosing from the menu switches to that service"
    else
        bad "after choosing, the screen shows $(pane | head -1)"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm pika pikb pikc >/dev/null 2>&1

    # ------------------------------------------- saying what was dropped rather than dropping it
    #
    # A byte pipe hands anything it does not understand to the terminal, which may well understand
    # it. A rendered attach interprets the stream, so an unimplemented sequence is simply gone -
    # and the user has no way to connect an odd-looking screen to it. Said once per sequence, on
    # the status line, with the way back to the byte pipe.
    only
    # OSC 52, a clipboard write, which this emulator does not implement. It used to be OSC 8 - and
    # then OSC 8 was implemented, and this check failed for the best possible reason: its example
    # had stopped being an example. Whatever is picked here has to be something still on the list
    # in internal/vt/grid/probe_test.go, and this note is here so the next person knows to look.
    "$gz" add exotic -start -- sh -c 'printf "\033]52;c;aGVsbG8=\007clipboard\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 70 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render exotic'"
    sleep 3

    if pane | sed -n '6p' | grep -q 'does not implement'; then
        ok "an unimplemented sequence is said out loud rather than silently dropped"
    else
        bad "nothing said anything about the dropped sequence: $(pane | sed -n '6p')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm exotic >/dev/null 2>&1

    # ------------------------------------------------------------ a way to fix a wrong screen
    #
    # The first thing anybody reaches for when a screen looks wrong. It repaints from the grid and
    # forgets what this client assumed the terminal already had - the modes, the title and the
    # cursor shape are otherwise only sent when they change, so a terminal that lost them would
    # not get them back until something changed again.
    only
    "$gz" add drawn -start -- sh -c 'printf "\033]2;TITLE-HERE\007\033]7;file://box/redrawn-here\033\\\\\033[?2004hCONTENT\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render drawn > $home/redraw.bin'"
    sleep 3

    # Occurrences, not lines: the output is one binary blob, so grep -c counts it as one however
    # many times the string is in it. That misread a working redraw as a broken one once.
    occurrences() { grep -ao "$1" "$home/redraw.bin" | wc -l; }
    titleBefore=$(occurrences 'TITLE-HERE')
    dirBefore=$(occurrences 'redrawn-here')
    tmux -L "$tmuxSock" send-keys C-] 'r'
    sleep 2
    if [ "$(occurrences 'TITLE-HERE')" -gt "$titleBefore" ] \
       && [ "$(occurrences "$(printf '\033')\[?2004h")" -gt 1 ]; then
        ok "Ctrl-] r repaints and re-sends what the terminal was assumed to have"
    else
        bad "after a redraw the title was sent $(occurrences 'TITLE-HERE') times, want more than $titleBefore"
    fi
    # The working directory is in that list too, and was the one thing Forget did not let go of:
    # everything else came back after a repaint and the terminal was left believing whatever
    # directory it had been told before it lost its state.
    if [ "$(occurrences 'redrawn-here')" -gt "$dirBefore" ]; then
        ok "and the working directory comes back with them"
    else
        bad "the directory was sent $dirBefore times before the repaint and $(occurrences 'redrawn-here') after"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm drawn >/dev/null 2>&1

    # ------------------------------------------------------- panes that are not the same size
    #
    # An even split is not always the right split. Ctrl-] > gives the focused pane more of the
    # screen and Ctrl-] < gives it less, which is measured here by where the seam between them
    # falls: the column the second service's output starts in.
    only
    for n in resa resb; do
        "$gz" add "$n" -start -- sh -c "while :; do printf '$n-XXXXXXXXXXXXXXXXXXXX\r\n'; sleep 1; done" >/dev/null 2>&1
    done
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render resa'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 2

    seam() { pane | awk '/resb-/ {print index($0, "resb-"); exit}'; }
    even=$(seam)
    tmux -L "$tmuxSock" send-keys C-] '>'
    sleep 2
    grown=$(seam)
    if [ -n "$even" ] && [ -n "$grown" ] && [ "$grown" -lt "$even" ]; then
        ok "Ctrl-] > gives the focused pane more of the screen"
    else
        bad "the seam did not move left: $even then $grown"
    fi

    tmux -L "$tmuxSock" send-keys C-] '<'
    tmux -L "$tmuxSock" send-keys C-] '<'
    sleep 2
    shrunk=$(seam)
    if [ -n "$shrunk" ] && [ "$shrunk" -gt "$grown" ]; then
        ok "Ctrl-] < gives it less"
    else
        bad "the seam did not move back right: $grown then $shrunk"
    fi

    # And a pane cannot be grown until its neighbour has nothing.
    #
    # Thirty presses, and the neighbour must still be at least ten columns. Ten presses was not
    # enough to tell anything: with the bound the focused pane reaches two thirds of the screen
    # and without it three quarters, and both leave the neighbour perfectly visible. Unbounded,
    # thirty presses take it to a seventh - about seven columns - which is a stripe that still
    # takes keystrokes.
    for _ in $(seq 1 30); do tmux -L "$tmuxSock" send-keys C-] '>'; done
    sleep 2
    squeezed=$(seam)
    if [ -n "$squeezed" ] && [ "$squeezed" -ge 10 ]; then
        ok "a pane cannot be grown until its neighbour is a stripe"
    else
        bad "after thirty presses the neighbour is ${squeezed:-no} columns wide"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm resa resb >/dev/null 2>&1

    # ----------------------------------------------------- the set that draws boxes has to draw
    #
    # A program selects the DEC line-drawing set and sends "lqqqk", which is the top of a box and
    # not five letters. ncurses uses it whenever the terminal description says to. Ignoring it puts
    # scattered letters where a person expects a frame.
    #
    # Checked on the screen rather than in the corpus, because the corpus cannot see it: tmux's
    # capture-pane reports the underlying letter whether or not the set was applied, so a recording
    # of a terminal that draws boxes and one that does not are identical.
    only
    "$gz" add boxy -start -- sh -c 'printf "\033(0lqqqk\033(B\r\n\033(0mqqqj\033(B\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 20 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render boxy'"
    sleep 3

    if [ "$(pane | sed -n '1p')" = "┌───┐" ] && [ "$(pane | sed -n '2p')" = "└───┘" ]; then
        ok "the line-drawing set draws lines, not letters"
    else
        bad "the box came out as [$(pane | sed -n '1p')]"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm boxy >/dev/null 2>&1

    # ------------------------------------------- a program that asks the terminal gets an answer
    #
    # A byte pipe gets this for free: the query reaches the user's real terminal and the reply
    # comes back through the same pipe. A client that interprets the stream is the terminal, and a
    # program that asks where the cursor is and is never told waits for an answer that is not
    # coming - which is a hang, not a cosmetic difference.
    #
    # Checked through the service's own log rather than the screen: the service's pty echoes what
    # it receives, so the reply lands in its output where it can be read back exactly.
    only
    "$gz" add asker -start -- sh -c 'sleep 3; printf "\033[2;1H\033[6n"; sleep 30' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 44 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render asker'"
    sleep 7

    # The service asked from row 2, column 1, so the answer has to say so.
    # Through cat -v, so the pattern is plain text. Matching a raw escape byte against what grep
    # treats as a binary file failed while the reply was demonstrably in the log - the failure
    # message printed it.
    if "$gz" logs asker 2>/dev/null | cat -v | grep -q '\^\[\[2;1R'; then
        ok "a rendered attach answers a program that asks where the cursor is"
    else
        bad "the service never received a cursor report: $("$gz" logs asker 2>/dev/null | cat -v | tail -1)"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm asker >/dev/null 2>&1

    # ------------------------------------------------ the cursor shape has to reach the terminal
    #
    # The last item on the list of things nothing checked. A program that asks for a bar while
    # editing and a block otherwise is doing something the user can see, and a rendered attach that
    # keeps the shape to itself leaves whatever the previous program set.
    only
    "$gz" add shaper -start -- sh -c 'printf "\033[5 qBAR-CURSOR\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 5 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render shaper > $home/shape.bin'"
    sleep 3

    if grep -q "$(printf '\033')\[5 q" "$home/shape.bin"; then
        ok "a rendered attach passes the cursor shape to the terminal"
    else
        bad "the cursor shape never reached the terminal"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    sleep 1
    # And puts it back. A cursor left as a blinking bar after a detach outlives the program that
    # asked for it, the same way mouse reporting does.
    if grep -q "$(printf '\033')\[0 q" "$home/shape.bin"; then
        ok "and resets it on the way out"
    else
        bad "the cursor shape was left as the service set it"
    fi

    "$gz" rm shaper >/dev/null 2>&1

    # -------------------------------------- the working directory has to reach the terminal too
    #
    # OSC 7 is how a shell says where it is, and it is why your terminal's next tab opens in the
    # directory the last one was in. A byte pipe hands it over untouched. A rendered attach reads
    # the stream, so it stops there unless it is passed on - which would make the path that adds
    # panes the path that quietly takes this away, and a capability the byte pipe has and the
    # rendered attach does not is a reason not to use the rendered attach.
    #
    # tmux records it as #{pane_path}; that it does was measured, not assumed.
    only
    "$gz" add pwder -start -- sh -c 'printf "\033]7;file://box/tmp/where-i-am\033\\\\here\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 5 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render pwder'"
    sleep 3

    if [ "$(ask '#{pane_path}')" = "file://box/tmp/where-i-am" ]; then
        ok "a rendered attach passes the working directory to the terminal"
    else
        bad "the terminal was told the directory is [$(ask '#{pane_path}')]"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm pwder >/dev/null 2>&1

    # ---------------------------------------------- the window title has to reach the terminal
    #
    # Same shape as the modes below: a byte pipe hands OSC 2 to the real terminal, so a shell's
    # title tracks what it is running. A client that interprets the stream has to carry it, or the
    # title freezes at whatever it said when the attach started.
    only
    "$gz" add titler -start -- sh -c 'printf "\033]2;MY-WINDOW-TITLE\007running\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 5 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render titler'"
    sleep 3

    if [ "$(ask '#{pane_title}')" = "MY-WINDOW-TITLE" ]; then
        ok "a rendered attach passes the window title to the terminal"
    else
        bad "the terminal's title is [$(ask '#{pane_title}')], want MY-WINDOW-TITLE"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm titler >/dev/null 2>&1

    # ---------------------------------------- a rendered attach survives the window changing size
    #
    # Nothing tested this, and it is the most ordinary thing a person does: you drag the corner of
    # your terminal while something is open in it. In a rendered attach that is a longer chain than
    # anywhere else - SIGWINCH, the screen resizes, the panes are laid out again, each grid reflows
    # its scrollback to the new width, each service is told its new size, the program redraws, and
    # the whole thing is painted. Every link is unit-tested and the chain was not.
    only
    # It reports the width it believes it has, which is the thing the resize is supposed to change.
    # stty rather than tput: the daemon runs under a nearly empty environment, so a service has no
    # TERM and tput cannot answer. stty asks the terminal itself.
    "$gz" add sizer -start -- sh -c 'while :; do printf "SIZER-COLS-%s\r\n" "$(stty size 2>/dev/null | cut -d" " -f2)"; sleep 1; done' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 24 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render sizer'"
    sleep 3
    if ! pane | grep -q 'SIZER-'; then
        bad "the rendered attach drew nothing before the resize, so this proves nothing"
    else
        ok "a rendered attach is drawing before the window changes size"

        # Narrower and shorter, which is the direction that has to reflow and re-place things.
        tmux -L "$tmuxSock" resize-window -x 50 -y 14 2>/dev/null || \
            tmux -L "$tmuxSock" set-option -g window-size manual 2>/dev/null
        tmux -L "$tmuxSock" resize-window -x 50 -y 14 2>/dev/null
        sleep 4

        if pane | grep -q 'SIZER-'; then
            ok "and it is still drawing the service afterwards"
        else
            bad "the pane went blank after the resize: $(pane | head -3 | tr '\n' '|')"
        fi

        # The status line has to follow the window down, not stay on row 24 of a 14-row screen.
        if pane | sed -n '14p' | grep -q 'sizer'; then
            ok "the status line moved to the new last row"
        else
            bad "row 14 of the resized window is: [$(pane | sed -n '14p')]"
        fi

        # And the service was told. This replaced a check on the widest line drawn, which could
        # not fail: tmux clips its own capture to the window width, so a leftover from the old
        # size is invisible to it whatever the program does. Asking the service what width it
        # believes it has goes all the way down the chain instead of looking at the top of it.
        saw=$(pane | grep -o 'SIZER-COLS-[0-9]*' | tail -1 | sed 's/.*-//')
        if [ "$saw" = "50" ]; then
            ok "the service was told its new width ($saw columns)"
        else
            bad "the service still believes it has $saw columns, not 50"
        fi
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm sizer >/dev/null 2>&1

    # -------------------------------------- and resizing while you are reading the scrollback
    #
    # The case most likely to be wrong, because two things that each rewrite the grid happen at
    # once: the history is reflowed to the new width while somebody is looking at a particular
    # place in it. Getting it wrong means the view jumps, or empties, or silently becomes the live
    # screen while you are still reading.
    only
    "$gz" add hist -start -- sh -c 'for i in $(seq 1 60); do echo "LINE-$i"; done; sleep 600' >/dev/null 2>&1
    sleep 2
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 20 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render hist'"
    sleep 3
    newest() { pane | grep -o 'LINE-[0-9]*' | tail -1 | sed 's/.*-//'; }

    live=$(newest)
    tmux -L "$tmuxSock" send-keys C-] 'b'
    sleep 2
    back=$(newest)
    if [ -n "$back" ] && [ -n "$live" ] && [ "$back" -lt "$live" ]; then
        ok "Ctrl-] b is showing older output than the live screen (line $back, live was $live)"
    else
        bad "scrolling back showed line [$back] where live showed [$live]"
    fi

    tmux -L "$tmuxSock" resize-window -x 50 -y 14 2>/dev/null
    sleep 3
    after=$(newest)
    if [ -n "$after" ] && [ "$after" -lt "$live" ]; then
        ok "and it is still showing the scrollback after the window resized under it (line $after)"
    else
        bad "after resizing while scrolled back the newest line on screen is [$after], live was [$live]"
    fi

    tmux -L "$tmuxSock" send-keys C-] 'g'
    sleep 2
    if [ "$(newest)" = "$live" ]; then
        ok "Ctrl-] g comes back to the live screen at the new size"
    else
        bad "after returning to live the newest line is [$(newest)], want $live"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm hist >/dev/null 2>&1

    # ---------------------------------------- two terminals on one service, both of them live
    #
    # The promise underneath `viewers` in `ls`, and underneath the read-only attach: you can be
    # attached from your laptop and your desk at once. Counting viewers was checked; that both of
    # them actually *see* anything was not, which is the difference between a number and a
    # feature.
    #
    # Typing is checked in one direction only on purpose. Both terminals send to the same pty, so
    # "which one typed it" is not a question the service can answer or that anybody should rely
    # on; what matters is that a keystroke from either arrives and that the answer reaches both.
    only
    "$gz" add pair -start -restart always -- sh -c 'PS1=""; export PS1; exec /bin/sh -i' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 20 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach pair'"
    tmux -L "$tmuxSock" split-window -d \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach pair'"
    sleep 3

    if [ "$("$gz" ls 2>/dev/null | awk '$1 == "pair" {print $6}')" = "2" ]; then
        ok "two terminals attached to one service are both counted"
    else
        bad "viewers reads [$("$gz" ls 2>/dev/null | awk '$1 == "pair" {print $6}')], want 2"
    fi

    # Typed into the first pane; the answer has to appear in both.
    tmux -L "$tmuxSock" send-keys -t 0 'echo BOTH-$((6*7))-SEE' Enter
    sleep 3
    first=$(tmux -L "$tmuxSock" capture-pane -p -t 0 2>/dev/null | grep -c 'BOTH-42-SEE')
    second=$(tmux -L "$tmuxSock" capture-pane -p -t 1 2>/dev/null | grep -c 'BOTH-42-SEE')
    if [ "${first:-0}" -gt 0 ] && [ "${second:-0}" -gt 0 ]; then
        ok "output reaches both terminals, not just the one that typed"
    else
        # Everything the hunt in scripts/pairrace.sh could not get, recorded here because this is
        # where it actually happens: the suite reproduces it and a hundred parallel copies of the
        # reproducer alone did not. The cursor is the one measurement still missing - a client
        # whose cursor sits on the reserved row would have every line it receives painted over by
        # the next status repaint, which is what a live status line above a blank body looks like.
        bad "the answer appeared in pane0=$first pane1=$second; both should have it$(printf '\n        cursor0=%s cursor1=%s of %s rows; scrollback0=%s scrollback1=%s\n        pane0: %s\n        pane1: %s' \
            "$(tmux -L "$tmuxSock" display-message -p -t 0 '#{cursor_y}' 2>/dev/null)" \
            "$(tmux -L "$tmuxSock" display-message -p -t 1 '#{cursor_y}' 2>/dev/null)" \
            "$(tmux -L "$tmuxSock" display-message -p -t 0 '#{pane_height}' 2>/dev/null)" \
            "$(tmux -L "$tmuxSock" capture-pane -p -S -200 -t 0 2>/dev/null | grep -c 'BOTH-42-SEE')" \
            "$(tmux -L "$tmuxSock" capture-pane -p -S -200 -t 1 2>/dev/null | grep -c 'BOTH-42-SEE')" \
            "$(tmux -L "$tmuxSock" capture-pane -p -t 0 2>/dev/null | grep -v '^$' | tail -3 | tr '\n' '|')" \
            "$(tmux -L "$tmuxSock" capture-pane -p -t 1 2>/dev/null | grep -v '^$' | tail -3 | tr '\n' '|')")"
    fi

    # And the other direction: typing in the second one also arrives.
    tmux -L "$tmuxSock" send-keys -t 1 'echo OTHER-$((6*8))-WAY' Enter
    sleep 3
    if tmux -L "$tmuxSock" capture-pane -p -t 0 2>/dev/null | grep -q 'OTHER-48-WAY'; then
        ok "and either terminal can type at it"
    else
        bad "what was typed in the second terminal never reached the service"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm pair >/dev/null 2>&1

    # -------------------------------------- two terminals starting a shell at the same moment
    #
    # You ssh in twice at once, or a dropped connection reconnects while the old one is still
    # closing. Both logins run `gozellij`, both find no shell, both create one - and the loser's
    # attach fails with "no such service" because the winner's service was replaced underneath it.
    #
    # That is why the client asks the daemon to Ensure rather than looking and then adding. The
    # comment in cmdShell says it was measured with three at once; it was measured by hand, and
    # then nothing checked it again. This is the same measurement, kept.
    only
    # Waited for by pid, not with a bare `wait`. This script starts its daemon in the background
    # and keeps it there, so `wait` with no arguments waits for the daemon too - which never exits
    # and wedges the suite at this line for ever.
    racers=""
    for i in 1 2 3; do
        ( timeout 8 "$gz" shell -name raced < /dev/null > "$work/race.$i" 2>&1 ) &
        racers="$racers $!"
    done
    for r in $racers; do wait "$r" 2>/dev/null; done
    made=$("$gz" ls 2>/dev/null | awk '$1 == "raced"' | wc -l)
    if [ "$made" = "1" ]; then
        ok "three terminals starting the same shell at once leave exactly one service"
    else
        bad "$made services called raced exist after three simultaneous starts"
    fi
    if grep -haq "no such service" "$work"/race.* 2>/dev/null; then
        bad "one of them lost the race: $(grep -ha 'no such service' "$work"/race.* | head -1)"
    else
        ok "and none of them was told its service had vanished"
    fi
    "$gz" rm raced >/dev/null 2>&1
    rm -f "$work"/race.*

    # ------------------------------------------------ doctor is itself a promise
    #
    # The document points at `gozellij doctor` four times - for linger, for the unit, for which
    # prefix key is in force, for what tree-kill can do here - and one line of this script ever
    # ran it. A diagnostic that crashes, or that reports failure on a healthy machine, is worse
    # than none: it is the thing you reach for when you already believe something is wrong.
    only
    "$gz" add doc -start -- sh -c 'sleep 60' >/dev/null 2>&1
    sleep 1
    out=$("$gz" doctor 2>&1); status=$?
    if [ "$status" = "0" ]; then
        ok "doctor exits zero on a healthy daemon"
    else
        bad "doctor exited $status on a working setup: $(printf '%s' "$out" | grep -E '^(FAIL|warn)' | head -1)"
    fi
    if printf '%s' "$out" | grep -q "^ok    daemon "; then
        ok "and it reports the daemon it just talked to"
    else
        bad "doctor did not report the daemon: $(printf '%s' "$out" | head -2 | tr '\n' '|')"
    fi
    # Every line it prints is one of its four levels. A malformed row means a check returned
    # something the printer did not expect, which is how a diagnostic starts lying.
    if [ -z "$(printf '%s' "$out" | grep -vE '^(ok|note|warn|FAIL)  |^$|^[a-z].*:$|^  ')" ]; then
        ok "and every line it prints is a level it knows"
    else
        bad "doctor printed a line in no known shape: $(printf '%s' "$out" | grep -vE '^(ok|note|warn|FAIL)  |^$|^[a-z].*:$|^  ' | head -1)"
    fi
    "$gz" rm doc >/dev/null 2>&1

    # ------------------------------------------------ logs are rotated, and only one is kept
    #
    # "appended as the service runs and rotated at 16 MiB with one generation kept" has been in the
    # document for a long time with nothing checking it. A log that never rotates fills a VPS disk
    # quietly; one that rotates without limit does the same thing more slowly. Both failures look
    # like nothing at all until the disk is full, which is the argument for checking it rather than
    # believing it.
    #
    # Sixteen mebibytes through a pty takes about eight seconds, measured, so this is affordable.
    only
    # `yes` rather than a shell loop: a printf per line spends its time in the shell rather than in
    # the pipe, and on a busy machine it did not reach the limit inside the wait - which reads as
    # "rotation is broken" when what is broken is the test's idea of how fast a shell is.
    "$gz" add noisy -start -- sh -c 'yes "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" | head -n 250000; sleep 300' >/dev/null 2>&1
    for _ in $(seq 240); do
        [ -f "$state/logs/noisy.log.1" ] && break
        sleep 0.5
    done

    if [ -f "$state/logs/noisy.log.1" ]; then
        ok "a log that grows past its limit is rotated"
    else
        bad "no rotated log after $(stat -c%s "$state/logs/noisy.log" 2>/dev/null) bytes"
    fi

    # The rotated one is the full-sized one and the live one is fresh, which is what says the
    # rotation happened at the limit rather than at some arbitrary moment.
    rotated=$(stat -c%s "$state/logs/noisy.log.1" 2>/dev/null || echo 0)
    if [ "$rotated" -gt 16000000 ] && [ "$rotated" -lt 17500000 ]; then
        ok "it rotated at about the size it promises (${rotated} bytes)"
    else
        bad "the rotated log is $rotated bytes, nowhere near the 16 MiB limit"
    fi

    # One generation. A .log.2 would mean it keeps everything, which fills the disk slowly instead
    # of quickly and is the failure nobody notices until it matters.
    if [ ! -f "$state/logs/noisy.log.2" ]; then
        ok "and only one generation is kept"
    else
        bad "a second generation exists, so nothing is ever thrown away"
    fi

    if "$gz" logs noisy 2>/dev/null | tail -1 | grep -q 'x'; then
        ok "and the log can still be read after rotating"
    else
        bad "logs returned nothing after the rotation"
    fi

    "$gz" stop noisy >/dev/null 2>&1
    "$gz" rm noisy >/dev/null 2>&1

    # ------------------------------------------- the status line in the title bar
    #
    # `where=title` is what this document recommends to anybody bothered by a full-screen program
    # drawing over the bottom row: nothing can draw over a title bar. It was recommended and never
    # checked, which is the wrong way round - it is the escape hatch, so it has to work when the
    # ordinary thing does not.
    #
    # Two halves, and the second is the point: the line has to appear in the title, and the bottom
    # row must *not* be reserved, or the mode costs a row without using it.
    only
    "$gz" add titled -start -- sh -c 'printf "TITLED-UP\r\n"; sleep 60' >/dev/null 2>&1
    printf 'where=title\n' > "$home/titlecfg"
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 12 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e GOZELLIJ_STATUS_CONFIG="$home/titlecfg" -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach titled'"
    sleep 4

    if [ "$(ask '#{pane_title}')" != "${title_before:-}" ] && ask '#{pane_title}' | grep -q 'titled'; then
        ok "where=title puts the status line in the terminal's title"
    else
        bad "the title is [$(ask '#{pane_title}')], which does not name the service"
    fi

    # Nothing is reserved, so the service has the whole screen. On a twelve-row terminal that is
    # 0-11; the bottom-row mode would say 0-10.
    if [ "$(ask '#{scroll_region_lower}')" = "11" ]; then
        ok "and it reserves no row, so the service has the whole screen"
    else
        bad "the scrolling region ends at $(ask '#{scroll_region_lower}'), want 11 (nothing reserved)"
    fi

    if pane | grep -q 'TITLED-UP'; then
        ok "and the service's output is still shown"
    else
        bad "the service's output is missing with where=title: $(pane | head -2 | tr '\n' '|')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm titled >/dev/null 2>&1
    rm -f "$home/titlecfg"

    # ------------------------------- the login shell actually lands in gozellij
    #
    # The chain a person meets on their first ssh in after `gozellij login-setup -install`:
    # bash -l reads ~/.profile, the block fires, gozellij creates the shell service and attaches.
    # Every part of that is checked somewhere else; this is the part where they are checked
    # together, which is the only place the ordering can be wrong.
    #
    # A throwaway HOME, carrying the shape of the problem: another multiplexer's autostart block,
    # pointed at a stand-in so nothing real can start.
    #
    # env -u matters more than it looks. tmux exports $TMUX in every pane, and this suite may
    # itself be running inside a multiplexer that exports $ZELLIJ. The block correctly stands down
    # for both - so without clearing them the check shows an ordinary shell and reads exactly like
    # a failure of the thing it is testing. It did, once, and the answer looked like a bug in the
    # program rather than in the harness.
    only
    lhome="$work/loginhome"
    rm -rf "$lhome"; mkdir -p "$lhome/bin"
    printf '#!/bin/sh\necho OTHER-MULTIPLEXER-STARTED\nsleep 30\n' > "$lhome/bin/othermux"
    chmod +x "$lhome/bin/othermux"
    cat > "$lhome/.profile" <<PROFILE
export EDITOR=vim
if [ -n "\$PS1" ] && [ -z "\${ZELLIJ:-}" ] && [ -z "\${TMUX:-}" ]; then
    tmux attach -t main 2>/dev/null || "$lhome/bin/othermux"
fi
PROFILE
    HOME="$lhome" "$gz" login-setup -install >/dev/null 2>&1

    # The invocation is "$GOZELLIJ_BIN" shell -name <service>, not the literal program name: the
    # block names the binary by path because /etc/profile resets PATH and a profile may not have
    # put it back by the time this line runs.
    if grep -q '#gz# ' "$lhome/.profile" && grep -q 'shell -name shell' "$lhome/.profile"; then
        ok "login-setup disables the other multiplexer and adds its own block"
    else
        bad "the profile does not look set up: $(grep -c . "$lhome/.profile") lines, no markers"
    fi

    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 20 \
        -e HOME="$lhome" -e PATH="$bin:/usr/bin:/bin" -e SHELL=/bin/bash \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e TERM=xterm-256color \
        "sh -c 'exec env -u TMUX -u ZELLIJ -u ZELLIJ_SESSION_NAME -u STY bash -l'"
    sleep 5
    tmux -L "$tmuxSock" send-keys 'echo INSIDE-[$GOZELLIJ]' Enter
    sleep 2
    if pane | grep -q 'INSIDE-\[shell\]'; then
        ok "a login shell lands inside gozellij, with \$GOZELLIJ set"
    else
        bad "the login shell did not land in gozellij: $(pane | tail -3 | tr '\n' '|')"
    fi
    if pane | grep -q 'OTHER-MULTIPLEXER-STARTED'; then
        bad "the other multiplexer started as well, so you would be in two at once"
    else
        ok "and the other multiplexer did not start alongside it"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm shell >/dev/null 2>&1
    rm -rf "$lhome"

    # ----------------------------------------------------- a new shell, from inside, where you are
    #
    # The new-tab key every other multiplexer has, and the thing there was otherwise no way to do
    # from inside gozellij: typing `gozellij shell -name x` in a shell is nesting, which is refused.
    # It has to open in the directory the shell you pressed it in had got to, and know its own name
    # rather than inherit the one it was made from.
    only
    mkdir -p "$home/deep/er"
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 100 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e SHELL=/bin/sh -e TERM=xterm-256color \
        "sh -c 'exec $gz'"
    sleep 3
    tmux -L "$tmuxSock" send-keys "cd $home/deep/er" Enter
    sleep 1
    tmux -L "$tmuxSock" send-keys C-] 'c'
    sleep 3
    if "$gz" ls 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx 'shell-2'; then
        ok "Ctrl-] c starts a new shell from inside an attach"
    else
        bad "no new shell appeared: $("$gz" ls 2>/dev/null | awk 'NR>1 {print $1}' | tr '\n' ' ')"
    fi
    tmux -L "$tmuxSock" send-keys 'echo "IN=[$GOZELLIJ] AT=[$(pwd)]"' Enter
    sleep 2
    seen=$(pane | grep -o 'IN=\[[^]]*\] AT=\[[^]]*\]' | tail -1)
    if [ "$seen" = "IN=[shell-2] AT=[$home/deep/er]" ]; then
        ok "and it opens where you were, knowing its own name"
    else
        bad "the new shell says $seen"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm shell shell-2 >/dev/null 2>&1

    # ----------------------------------------- watching without being able to touch
    #
    # Story B3: eyes on the live output, and my Ctrl-C does not reach the service. `attach -r`.
    #
    # The daemon drops this connection's keystrokes rather than the client promising not to send
    # them, because "cannot hurt it" is a claim about the far end. The check below passes either
    # way, which is the point of the sabotage rather than a hole in it: removing the *client's*
    # drop must leave this passing, and that is what says the daemon is doing the work.
    #
    # And a keystroke that goes nowhere has to say so, or it is rule 1's silent success with a
    # terminal attached.
    only
    "$gz" add watched -start -restart always -- sh -c 'trap "printf \"GOT-THE-INTERRUPT\r\n\"" INT; printf "WATCHED-IS-UP\r\n"; while :; do sleep 1; done' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 50 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -r watched'"
    sleep 3
    if ! pane | grep -q 'WATCHED-IS-UP'; then
        bad "a read-only attach shows nothing, so there is nothing to watch"
    else
        ok "a read-only attach still shows the service's output"
    fi

    tmux -L "$tmuxSock" send-keys C-c
    sleep 2
    if "$gz" logs watched 2>/dev/null | grep -q 'GOT-THE-INTERRUPT'; then
        bad "Ctrl-C reached the service through a read-only attach"
    else
        ok "Ctrl-C does not reach the service through a read-only attach"
    fi

    if pane | grep -q 'read-only'; then
        ok "a swallowed keystroke says so rather than going quiet"
    else
        bad "the screen never mentioned it was read-only: $(pane | tail -2 | tr '\n' '|')"
    fi

    # And it is still a usable client: the prefix key is how you leave.
    tmux -L "$tmuxSock" send-keys C-] 'd'
    sleep 2
    if tmux -L "$tmuxSock" list-panes -F '#{pane_dead}' 2>/dev/null | grep -q 1 ||
       ! tmux -L "$tmuxSock" capture-pane -p 2>/dev/null | grep -q 'WATCHED-IS-UP'; then
        ok "Ctrl-] d still detaches a read-only attach"
    else
        bad "the read-only attach would not detach"
    fi
    # And the other half of the story: which of the terminals attached can type.
    #
    # The number alone answers the wrong question. Three terminals showing a service is
    # reassuring; three that can type into it is a reason to find out whose they are before you
    # restart it.
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 50 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -r watched'"
    tmux -L "$tmuxSock" split-window -d \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach watched'"
    sleep 3
    seen=$("$gz" ls 2>/dev/null | awk '$1 == "watched" {print}')
    if printf '%s' "$seen" | grep -q '1+1r'; then
        ok "the list says which of the terminals attached can type"
    else
        bad "with one read-only and one ordinary attach, ls says: $seen"
    fi
    if "$gz" status watched 2>/dev/null | grep -q 'viewers:.*2 (1 read-only)'; then
        ok "and status spells it out"
    else
        bad "status says: $("$gz" status watched 2>/dev/null | grep viewers)"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm watched >/dev/null 2>&1

    # ----------------------------- the terminal is asked what colour it is, on the way in
    #
    # vim asks the terminal for its background colour and picks a light or a dark scheme from the
    # answer. A byte pipe passes the question through; a rendered attach is the terminal from the
    # program's side and knew nothing, so vim guessed. Inventing an answer would be worse than the
    # guess, so the client asks the real terminal the same question when it starts.
    #
    # Only that the question is asked is checked here. tmux answers it by asking *its* terminal,
    # and a detached tmux server has none - measured, before this check was written, by sending
    # the query into a tmux pane and reading nothing back. What happens to an answer when one
    # arrives is in internal/daemon/colourreply_test.go, where a pipe stands in for a terminal
    # that replies.
    only
    "$gz" add colourer -start -- sh -c 'printf "COLOUR-DEMO\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render colourer > $home/colour.bin'"
    sleep 3
    if grep -q "$(printf '\033')\]11;?" "$home/colour.bin" && grep -q "$(printf '\033')\]10;?" "$home/colour.bin"; then
        ok "a rendered attach asks the terminal what colour it is"
    else
        bad "the client never asked: $(cat -v "$home/colour.bin" | head -c 200)"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm colourer >/dev/null 2>&1

    # ------------------------- the arrow keys and the title stack have to reach the terminal
    #
    # Two things the survey in internal/vt/grid/probe_test.go found, both invisible on the screen.
    #
    # Every interactive program on this machine sets application cursor keys, which changes what
    # the arrow keys send. In a rendered attach the keys come from the user's real terminal, so
    # not passing it on means a program in that mode reads the other mode's bytes. It was being
    # dropped by the one branch of the parser that counted nothing, so nothing said so either.
    #
    # And less, vim, htop and nano all push the window title on the way in and pop it on the way
    # out. Without the stack the title a program set is the one you are left with afterwards.
    only
    "$gz" add keypadder -start -- sh -c 'printf "\033[?1h\033=KEYS-ON\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render keypadder > $home/keys.bin'"
    sleep 3
    if grep -q "$(printf '\033')\[?1h" "$home/keys.bin" && grep -q "$(printf '\033')=" "$home/keys.bin"; then
        ok "application cursor keys and keypad reach the terminal"
    else
        bad "the client never passed them on: $(cat -v "$home/keys.bin" | head -c 200)"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm keypadder >/dev/null 2>&1

    only
    # The waits are relative to the service starting, and the attach is up about a second and a
    # half after that. An earlier version of this check had the service run through all three
    # stages before the attach existed, and read FIRST-TITLE twice - a check that passes for the
    # wrong reason in the one direction that matters.
    "$gz" add stacker -start -- sh -c 'sleep 4; printf "\033]2;FIRST-TITLE\007"; sleep 2; printf "\033[22t\033]2;SECOND-TITLE\007"; sleep 3; printf "\033[23t"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render stacker'"
    sleep 6
    mid=$(ask '#{pane_title}')
    sleep 4
    if [ "$mid" = "SECOND-TITLE" ] && [ "$(ask '#{pane_title}')" = "FIRST-TITLE" ]; then
        ok "a popped title is the one that was pushed, not the one that replaced it"
    else
        bad "the title went [$mid] then [$(ask '#{pane_title}')], want SECOND-TITLE then FIRST-TITLE"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm stacker >/dev/null 2>&1

    # --------------------------------- mouse and paste modes have to reach the real terminal
    #
    # A byte pipe passes these through for free. A client that interprets the output has to hand
    # them on deliberately, and until it did, a rendered attach silently dropped every one: mouse
    # clicks did nothing and pasting into an editor misbehaved, on a screen that otherwise looked
    # perfect.
    #
    # This check reads the bytes the client writes rather than the screen, because none of it is
    # visible on the screen - which is exactly why it was missing.
    only
    "$gz" add moder -start -- sh -c 'printf "\033[?2004h\033[?1000hMODES-ON\r\n"; sleep 60' >/dev/null 2>&1
    "$gz" add noder -start -- sh -c 'printf "NO-MODES\r\n"; sleep 60' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render moder > $home/modes.bin'"
    sleep 3

    if grep -q "$(printf '\033')\[?2004h" "$home/modes.bin" && grep -q "$(printf '\033')\[?1000h" "$home/modes.bin"; then
        ok "a rendered attach passes bracketed paste and mouse reporting to the terminal"
    else
        bad "the modes never reached the terminal"
    fi

    # And withdrawn when the keyboard moves to a pane that did not ask for them: they are about
    # the mouse and the keyboard, and those go to one pane at a time.
    tmux -L "$tmuxSock" send-keys C-] 'n'
    sleep 3
    if grep -q "$(printf '\033')\[?2004l" "$home/modes.bin" && grep -q "$(printf '\033')\[?1000l" "$home/modes.bin"; then
        ok "and takes them back when the focused service does not want them"
    else
        bad "the modes were left switched on after switching service"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm moder noder >/dev/null 2>&1

    # ------------------------------------------------- a split that stacks instead of splitting
    #
    # Three panes in columns on an eighty-column terminal give twenty-six each, which is not a pane
    # but a margin. Ctrl-] - puts the new one underneath instead, which is what a narrow terminal
    # or a log pane wants.
    only
    for n in rowa rowb; do
        "$gz" add "$n" -start -- sh -c "i=0; while :; do printf '$n-%d\r\n' \$i; i=\$((i+1)); sleep 1; done" >/dev/null 2>&1
    done
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 12 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render rowa'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '-'
    sleep 3

    # One above the other, which is a statement about rows: the first service's output in the top
    # half and the second's in the bottom. Checking that both appear somewhere would also pass for
    # a side-by-side split.
    topRow=$(pane | head -5 | grep -c 'rowa-')
    lowRow=$(pane | sed -n '6,11p' | grep -c 'rowb-')
    if [ "$topRow" -gt 0 ] && [ "$lowRow" -gt 0 ]; then
        ok "Ctrl-] - stacks the new pane underneath instead of beside"
    else
        bad "the stacked split is not stacked: rowa in $topRow top lines, rowb in $lowRow lower ones"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm rowa rowb >/dev/null 2>&1

    # ----------------------------------- switching a service without throwing the layout away
    #
    # Ctrl-] n in a split used to end the session and start again with one pane: the other pane
    # vanished, silently, and the user's arrangement with it. It now changes what the focused pane
    # is showing and leaves the rest alone.
    only
    for n in swapa swapb swapc; do
        "$gz" add "$n" -start -- sh -c "i=0; while :; do printf '$n-%d\r\n' \$i; i=\$((i+1)); sleep 1; done" >/dev/null 2>&1
    done
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 70 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render swapa'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] 'n'
    sleep 4

    row=$(pane | head -1)
    if printf '%s' "$row" | grep -q 'swapa-' && printf '%s' "$row" | grep -q 'swapc-'; then
        ok "Ctrl-] n changes the focused pane's service and keeps the other pane"
    else
        bad "after switching, the top row is [$row]"
    fi

    # And the pane that was switched has to be live, not showing a replay and then nothing.
    #
    # Only its own columns. Comparing the whole row passed with the reader removed entirely,
    # because the row holds both panes and the other one was still running - the check was reading
    # the left pane and reporting on the right.
    rightOf() { pane | head -1 | cut -c36-; }
    first=$(rightOf)
    sleep 3
    if [ "$(rightOf)" != "$first" ]; then
        ok "the switched pane keeps receiving output"
    else
        bad "the switched pane stopped at [$first]"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm swapa swapb swapc >/dev/null 2>&1

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
    newscreen
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
    newscreen
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
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render tickone'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 3

    beforeUpgrade=$(pane | head -1)
    "$gz" upgrade >/dev/null 2>&1

    # The status row while the message is still fresh. A message lingers six seconds and the first
    # version of this check looked after sleeping six - so it passed or failed depending on which
    # side of the deadline the capture landed on, and did fail once.
    sleep 2
    statusAfterUpgrade=$(pane | sed -n '10p')
    sleep 4
    afterUpgrade=$(pane | head -1)

    # It has to say so. The panes come back working, but whatever was printed while the daemon was
    # being replaced is not on this screen and never will be; the byte-pipe path says exactly that
    # on its way back in, and silence about lost output is what this project keeps refusing to
    # ship.
    if printf '%s' "$statusAfterUpgrade" | grep -q 'reconnected'; then
        ok "the reconnection after an upgrade is said out loud"
    else
        bad "nothing on the status line mentions the reconnection: $statusAfterUpgrade"
    fi
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
    newscreen
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
    newscreen
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

    # --------------------------------- output reaches the screen without waiting for the clock
    #
    # A rendered attach paints at a rate rather than once per frame, because painting once per
    # frame was four fifths of the cost of a flood (see minRepaint, and the measurement in
    # internal/vt/render). The risk that comes with a rate is latency: a limit set too high is a
    # terminal that feels slow, and nothing here could see it. Setting the interval to ten minutes
    # and running this whole script left all sixty-five promises passing.
    #
    # The reason is the status line's own clock, which repaints everything every two seconds, so
    # any check that sleeps for two seconds is reading a screen the clock redrew. This one turns
    # that clock down to thirty seconds first, so the only thing that can put the marker on screen
    # is the output path being prompt.
    only
    mkfifo "$home/latch" 2>/dev/null || true
    printf 'every=30s\n' > "$home/slowstatus"
    "$gz" add latchy -start -- sh -c "cat $home/latch >/dev/null; printf 'ZZLATEZZ\r\n'; sleep 60" >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 40 -y 6 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e GOZELLIJ_STATUS_CONFIG="$home/slowstatus" -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render latchy'"
    sleep 3
    if pane | grep -q 'ZZLATEZZ'; then
        bad "the marker was on screen before it was asked for, so this check proves nothing"
    else
        echo go > "$home/latch"
        # Well inside the status clock's thirty seconds, and forty times the repaint interval.
        sleep 2
        if pane | grep -q 'ZZLATEZZ'; then
            ok "a service's output is drawn without waiting for the status line's clock"
        else
            bad "two seconds after the service spoke, the screen still says: $(pane | head -2 | tr '\n' '|')"
        fi
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm latchy >/dev/null 2>&1
    rm -f "$home/latch" "$home/slowstatus"

    # ------------------------------------------- you come back to the arrangement you left
    #
    # The point of a login multiplexer is that it holds your session while you are away. Panes
    # used to be rebuilt from nothing on every attach: you split your shell against your logs,
    # your ssh dropped, you came back and had one pane with no word about where the other went.
    #
    # The client is killed rather than detached on purpose. The usual way a login multiplexer's
    # client ends is the connection dropping, not somebody pressing Ctrl-] d, so a layout written
    # on the way out would be missing in exactly the case the feature exists for. kill -9 by
    # recorded pid, so nothing on the way out can run.
    only
    "$gz" add laya -start -- sh -c 'i=0; while :; do printf "LAYA-%d\r\n" $i; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
    "$gz" add layb -start -- sh -c 'i=0; while :; do printf "LAYB-%d\r\n" $i; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
    sleep 1
    rm -f "$state/layouts/laya.json"
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render laya'"
    sleep 2
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 2
    if ! pane | grep -q 'LAYA-' || ! pane | grep -q 'LAYB-'; then
        bad "the split this check depends on did not happen: $(pane | head -1)"
    else
        # The pane's command is exec'd, so the pane pid is the client itself.
        clientPid=$(tmux -L "$tmuxSock" list-panes -F '#{pane_pid}' | head -1)
        kill -9 "$clientPid" 2>/dev/null
        sleep 2

        if [ -f "$state/layouts/laya.json" ] &&
           grep -q '"service": "laya"' "$state/layouts/laya.json" &&
           grep -q '"service": "layb"' "$state/layouts/laya.json"; then
            ok "the arrangement is written down while the attach is running, not on the way out"
        else
            bad "no layout on disk after a split: $(cat "$state/layouts/laya.json" 2>/dev/null | tr -d '\n' | head -c 120)"
        fi

        newscreen
        tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
            -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
            -e TERM=xterm-256color \
            "sh -c 'stty -echo; exec $gz attach -render laya'"
        sleep 4
        if pane | grep -q 'LAYA-' && pane | grep -q 'LAYB-'; then
            ok "attaching again brings the second pane back without asking"
        else
            bad "the arrangement did not come back: $(pane | head -2 | tr '\n' '|')"
        fi

        # And both panes are live, not a replay painted once and then still. The check that
        # matters is the restored pane, which is the one a reconnection has to set up from
        # nothing rather than inherit.
        before=$(pane | grep -o 'LAYB-[0-9]*' | tail -1)
        sleep 3
        after=$(pane | grep -o 'LAYB-[0-9]*' | tail -1)
        if [ -n "$before" ] && [ "$before" != "$after" ]; then
            ok "the pane that came back is connected, not a picture of one"
        else
            bad "the restored pane is not moving: $before then $after"
        fi
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm laya layb >/dev/null 2>&1

    # ------------------------------------- a full-screen program in one pane of a split
    #
    # Every split check so far runs services that print a line and stop. A program that positions
    # the cursor absolutely - vim addresses row 1 column 1 and means it - is the case where pane
    # clipping has to do real work, and it is what a person actually puts in a pane.
    #
    # The failure this is for is not subtle once you see it: an editor that thinks it owns the
    # screen writes across the seam and over whatever is in the other pane, and what you lose is
    # the output you split the screen to watch.
    only
    printf 'ALPHAWORD\nsecond line\n' > "$home/split-edit.txt"
    "$gz" add quiet -start -- sh -c 'while :; do printf "QUIETMARK-%s\r\n" "$(stty size 2>/dev/null | cut -d" " -f2)"; sleep 1; done' >/dev/null 2>&1
    "$gz" add edity -start -- sh -c "TERM=xterm-256color exec vim -u NONE -N -n $home/split-edit.txt" >/dev/null 2>&1
    sleep 2
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 14 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render quiet'"
    sleep 3
    wide=$(pane | sed -n 's/.*QUIETMARK-\([0-9]*\).*/\1/p' | tail -1)
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 5

    # Being told the pane's width is what makes a full-screen program work in a pane at all. It
    # is checked on a window resize already; splitting the screen is the other way a service's
    # width changes, and nothing checked that one. A service that is not told draws to the width
    # it last heard - so vim lays out eighty columns of screen inside forty, and every line of it
    # wraps into the next.
    narrow=$(pane | sed -n 's/.*QUIETMARK-\([0-9]*\).*/\1/p' | tail -1)
    if [ -n "$wide" ] && [ -n "$narrow" ] && [ "$wide" -gt 70 ] && [ "$narrow" -lt "$wide" ]; then
        ok "splitting the screen tells the service its pane is narrower ($wide then $narrow columns)"
    else
        bad "the service heard [$wide] before the split and [$narrow] after it"
    fi

    # Both are on screen at all, or the rest proves nothing.
    if pane | grep -q 'QUIETMARK' && pane | grep -q 'ALPHAWORD'; then
        ok "a full-screen program and a plain service share one screen"

        # And each stays on its own side of the seam. The columns are what matters: an editor
        # writing across the split still *appears*, it just appears where the other service's
        # output should be.
        qcol=$(pane | awk '/QUIETMARK/ { print index($0, "QUIETMARK"); exit }')
        acol=$(pane | awk '/ALPHAWORD/ { print index($0, "ALPHAWORD"); exit }')
        if [ -n "$qcol" ] && [ -n "$acol" ] && [ "$qcol" -lt 40 ] && [ "$acol" -gt 40 ]; then
            ok "the editor stays in its own pane (columns $qcol and $acol of 80)"
        else
            bad "the panes overlap: QUIETMARK at column $qcol, ALPHAWORD at column $acol"
        fi
    else
        bad "one of them is missing: $(pane | head -3 | tr '\n' '|')"
    fi

    # And the status line comes back. Attaching to an editor says things - it asks the terminal
    # what colour it is, and says so when the terminal does not answer - and each of those takes
    # the status row for messageLinger seconds. Nothing checked that the row is ever given back,
    # which is the difference between a message and a status line that has been lost: read it too
    # early and a note looks like a permanent replacement. This waits the linger out first.
    #
    # Both of the notes an editor provokes were found here rather than reasoned about: the first
    # was gozellij calling vim's own DCS probe an unimplemented sequence, which is now fixed.
    sleep 8
    if pane | sed -n '14p' | grep -q 'quiet'; then
        ok "and the status line comes back after what gozellij had to say"
    else
        bad "row 14 still reads: [$(pane | sed -n '14p')]"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm quiet edity >/dev/null 2>&1

    # ------------------------------------- a split pane moved onto a stopped service stays open
    #
    # The half of the stopped-tab fix that the shared loop could not reach, done in
    # renderedsession.go by the other session working this tree (33bd43f), checked here because
    # this file is mine. A pane moved onto an already-stopped service used to close itself through
    # natural death, and the service it had been showing went with it - so `n` in a split could
    # cost you a pane as well as dropping you out.
    #
    # Three checks, and the third is the one that matters most: a pane whose service dies *while
    # it is shown* still closes. Fixing one case by swallowing the case it was carved out of is
    # the shape this pair of fixes was most at risk of, on both sides.
    only
    # Named so the order is deliberate rather than lucky. A split shows the next service that is
    # not on screen, and services are listed by name - so with stayer/mate/gone the split landed
    # on `gone` itself, the stopped one, and the `n` this is meant to exercise never happened.
    # a-, b- and z- put the running one next and the stopped one after it.
    "$gz" add astayer -start -restart no -- sh -c 'printf "STAYER-UP\r\n"; sleep 300' >/dev/null 2>&1
    "$gz" add bmate   -start -restart no -- sh -c 'printf "MATE-UP\r\n"; sleep 300' >/dev/null 2>&1
    "$gz" add zgone   -start -restart no -- sh -c 'printf "GONE-UP\r\n"; sleep 300' >/dev/null 2>&1
    sleep 1
    "$gz" stop zgone >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 70 -y 12 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render astayer'"
    sleep 3
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 3
    if pane | grep -q 'STAYER-UP' && pane | grep -q 'MATE-UP'; then
        # The focused pane is the new one; move it onto the stopped service.
        tmux -L "$tmuxSock" send-keys C-] 'n'
        sleep 3
        if pane | grep -q 'STAYER-UP' && pane | grep -q 'not running'; then
            ok "a split pane moved onto a stopped service stays open and says so"
        else
            bad "the screen after moving onto a stopped service: $(pane | grep -v '^$' | tail -4 | tr '\n' '|')"
        fi

        # u starts it in that pane, which is where it has to appear: a revive that started the
        # service somewhere else would read as working and leave the pane dead.
        tmux -L "$tmuxSock" send-keys C-] 'u'
        sleep 4
        if "$gz" status zgone 2>/dev/null | grep -q '^state: *running' &&
           [ "$("$gz" ls 2>/dev/null | awk '$1 == "zgone" {print $6}')" = "1" ]; then
            ok "and Ctrl-] u starts it in that pane, with the pane watching it"
        else
            bad "zgone is $("$gz" status zgone 2>/dev/null | grep '^state:' | tr -d '\n') with viewers [$("$gz" ls 2>/dev/null | awk '$1 == "zgone" {print $6}')]"
        fi

        # And the half that must not regress: a service that dies while its pane is showing it
        # still takes the pane with it.
        "$gz" stop zgone >/dev/null 2>&1
        sleep 4
        if ! pane | grep -q 'GONE-UP' && pane | grep -q 'STAYER-UP'; then
            ok "but a service that dies while its pane is showing it still closes the pane"
        else
            bad "the pane outlived its service: $(pane | grep -v '^$' | tail -4 | tr '\n' '|')"
        fi
    else
        bad "the split this needs did not happen: $(pane | grep -v '^$' | head -3 | tr '\n' '|')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop astayer bmate zgone >/dev/null 2>&1
    "$gz" rm astayer bmate zgone >/dev/null 2>&1

    # ------------------------------------------ the last two keys, and what they say without panes
    #
    # x closes a pane and f scrolls forward, and neither had ever been pressed. Both need panes,
    # so both have a second job: saying why they did nothing when there are none. A key that
    # silently does nothing is rule 1's exact case, and the one place a person is most likely to
    # meet it - pressing a key the help offers, in the mode they happen to be running.
    only
    "$gz" add pane1 -start -- sh -c 'i=1; while [ $i -le 40 ]; do printf "one-%02d\r\n" $i; i=$((i+1)); done; sleep 120' >/dev/null 2>&1
    "$gz" add pane2 -start -- sh -c 'printf "TWO-IS-HERE\r\n"; sleep 120' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 12 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach -render pane1'"
    sleep 3
    tmux -L "$tmuxSock" send-keys C-] '|'
    sleep 3
    if pane | grep -q 'TWO-IS-HERE'; then
        ok "two panes to press the last two keys in"

        # Focus lands on the *new* pane after a split - the status line marks it, [pane2] - so the
        # keys below would otherwise be pressed at the pane with one line in it while the check
        # read the other pane's column and saw nothing move. That is how the first version of this
        # failed, twice over: f looked broken and x looked like it closed the wrong pane, and both
        # were this.
        tmux -L "$tmuxSock" send-keys C-] 'o'
        sleep 2

        # f is the inverse of b: back into the scrollback, then forward out of it again. Only b
        # and g were ever pressed, so an f that did nothing - or that went the same way as b -
        # looked exactly like a working one.
        tmux -L "$tmuxSock" send-keys C-] 'b'
        sleep 2
        backTo=$(pane | grep -o 'one-[0-9][0-9]' | head -1)
        tmux -L "$tmuxSock" send-keys C-] 'f'
        sleep 2
        fwdTo=$(pane | grep -o 'one-[0-9][0-9]' | head -1)
        if [ -n "$backTo" ] && [ -n "$fwdTo" ] && [ "$fwdTo" \> "$backTo" ]; then
            ok "Ctrl-] f comes forward again from where Ctrl-] b went ($backTo then $fwdTo)"
        else
            bad "b then f showed $backTo then $fwdTo"
        fi

        # x closes the focused pane, leaving the other one and the attach.
        tmux -L "$tmuxSock" send-keys C-] 'g'
        sleep 1
        tmux -L "$tmuxSock" send-keys C-] 'x'
        sleep 3
        if pane | grep -q 'TWO-IS-HERE' && ! pane | grep -q 'one-40'; then
            ok "Ctrl-] x closes the pane you are in and leaves the other"
        else
            bad "after x the screen is: $(pane | grep -v '^$' | tail -3 | tr '\n' '|')"
        fi
        # Asked of the daemon rather than the screen. The first version looked for the status
        # line's usual text and failed, because closing a pane makes gozellij say something and a
        # message owns that row for messageLinger seconds - the same trap that had just been fixed
        # in goto(). A viewer the daemon still counts is the same question with no such window.
        if [ "$("$gz" ls 2>/dev/null | awk '$1 == "pane2" {print $6}')" = "1" ]; then
            ok "and closing one pane of two leaves the attach standing"
        else
            bad "after closing a pane, viewers on pane2 reads [$("$gz" ls 2>/dev/null | awk '$1 == "pane2" {print $6}')]"
        fi
    else
        bad "the split this needs did not happen: $(pane | head -2 | tr '\n' '|')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null

    # And in a byte pipe, where there are no panes at all, they say so rather than doing nothing.
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach pane2'"
    sleep 3
    tmux -L "$tmuxSock" send-keys C-] 'x'
    sleep 2
    if pane | grep -q 'would close a pane'; then
        ok "and in a byte pipe they say why they did nothing, rather than doing nothing"
    else
        bad "Ctrl-] x in a byte pipe said: $(pane | tail -2 | tr '\n' '|')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop pane1 pane2 >/dev/null 2>&1
    "$gz" rm pane1 pane2 >/dev/null 2>&1

    # --------------------------------------- exiting one shell of several moves you on, not out
    #
    # Joop's report, fixed by the other session working this tree (4cc0cf0, recorded in 6f06851),
    # checked here because this file is mine. Ctrl-] c then Ctrl-D dropped him out of gozellij
    # altogether: the shell he had just made was gone, so the attach ended, even with everything
    # else still running.
    #
    # The distinction that makes it safe is the same one 7e8a7de drew for stopped tabs: with
    # nothing else running, exit still gives the terminal back - that is the whole of what
    # somebody typing exit is asking for, and the very first check in this file covers it.
    only
    "$gz" add stayput -start -restart no -- sh -c 'printf "STAYPUT-UP\r\n"; sleep 300' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 10 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color -e SHELL=/bin/sh \
        "sh -c 'stty -echo; $gz attach stayput; printf \"OUT-OF-GOZELLIJ\\n\"; sleep 60'"
    sleep 3

    # A second service, made the way he made it: from inside, with Ctrl-] c.
    tmux -L "$tmuxSock" send-keys C-] 'c'
    sleep 3
    made=$("$gz" ls 2>/dev/null | awk 'NR>1' | wc -l)
    if [ "${made:-0}" -ge 2 ]; then
        ok "Ctrl-] c makes a second shell to exit out of"

        # Ctrl-D in it. The assertion is on where you end up, not on the exact words: the message
        # gains a clause in work that is held back, and a check pinned to the whole sentence would
        # break on somebody else's unrelated commit.
        tmux -L "$tmuxSock" send-keys C-d
        sleep 4
        if pane | grep -q 'now on stayput'; then
            ok "and Ctrl-D in it moves you to what is still running, naming where you landed"
        else
            bad "after Ctrl-D the screen says: $(pane | grep -v '^$' | tail -3 | tr '\n' '|')"
        fi
        if ! pane | grep -q 'OUT-OF-GOZELLIJ'; then
            ok "and does not drop you out of gozellij"
        else
            bad "exiting one shell of two ended the whole attach"
        fi
    else
        bad "Ctrl-] c did not make a second service: $("$gz" ls 2>/dev/null | awk 'NR>1{print $1}' | tr '\n' ' ')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop stayput >/dev/null 2>&1

    # ------------------------------------------- the prefix keys nobody had pressed
    #
    # Found by counting: the suite presses | o n g d b > < z r l c ? and -, and never k, u, x, f,
    # comma or p. Six of the keys the help offers had nothing driving them, including the one that
    # destroys something. A key that stopped working would be discovered by the person who pressed
    # it, which for `k` means discovering it on a service they wanted removed and still have, or
    # one they did not and no longer do.
    only
    "$gz" add first -start -restart no -- sh -c 'printf "FIRST-IS-UP\r\n"; sleep 300' >/dev/null 2>&1
    "$gz" add second -start -restart no -- sh -c 'printf "SECOND-IS-UP\r\n"; sleep 300' >/dev/null 2>&1
    "$gz" add doomed -start -restart no -- sh -c 'printf "DOOMED-IS-UP\r\n"; sleep 300' >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; $gz attach first; printf \"THE-ATTACH-ENDED\\n\"; sleep 120'"
    sleep 3

    # Not exec'd, unlike every other attach in this file, and on purpose: these checks press keys
    # that can end the attach, and with exec the client leaving takes the pane, the window and the
    # tmux server with it - so the next check reads an empty screen and reports "could not get to
    # X", which is true and says nothing about why. Leaving a shell behind means the screen can
    # still be read, and it says THE-ATTACH-ENDED.
    sleep 0

    # Go to a named service by pressing n until the status line says so, rather than counting
    # presses. The first version of these checks assumed positions, and when the rename below
    # changed the order every later check failed for a reason that had nothing to do with the key
    # it was testing.
    #
    # It waits for the status row to settle before pressing again, and that is not a nicety: a
    # message takes the whole row for messageLinger seconds, so a version that checked once and
    # pressed n again could not see [$1] while gozellij was saying something - and cycled straight
    # past the service it was looking for, every time, whenever a key had produced a message.
    goto() {
        for _ in 1 2 3 4 5 6; do
            for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14; do
                pane | tail -1 | grep -q "\[$1\]" && return 0
                sleep 0.6
            done
            tmux -L "$tmuxSock" send-keys C-] 'n'
            sleep 1.5
        done
        pane | tail -1 | grep -q "\[$1\]"
    }

    # p, the other half of n. Only n was ever pressed, so a p that had stopped moving - or that
    # moved the same way as n - would have looked perfectly healthy.
    tmux -L "$tmuxSock" send-keys C-] 'n'
    sleep 2
    afterN=$(pane | tail -1)
    tmux -L "$tmuxSock" send-keys C-] 'p'
    sleep 2
    afterP=$(pane | tail -1)
    if printf '%s' "$afterN" | grep -q '\[second\]' && printf '%s' "$afterP" | grep -q '\[first\]'; then
        ok "Ctrl-] p goes back the way Ctrl-] n came"
    else
        bad "n then p gave [$afterN] then [$afterP]"
    fi

    # comma: rename what you are looking at, from inside it.
    #
    # The prompt starts with the name you already have - tmux does the same for a window - and
    # Ctrl-U clears it. Written without the Ctrl-U first, which produced a service called
    # "firstrenamed-inside": the typing went on the end of what was already there. That is the
    # behaviour, not a bug, and now something says so.
    if goto first; then
        tmux -L "$tmuxSock" send-keys C-] ','
        sleep 1
        prompt=$(pane | tail -1)
        if printf '%s' "$prompt" | grep -q 'first'; then
            ok "Ctrl-] , offers the name you already have, to edit"
        else
            bad "the rename prompt said: $prompt"
        fi
        tmux -L "$tmuxSock" send-keys C-u
        sleep 1
        tmux -L "$tmuxSock" send-keys 'renamed-inside' Enter
        sleep 2
        if "$gz" status renamed-inside >/dev/null 2>&1 && ! "$gz" status first >/dev/null 2>&1; then
            ok "and Ctrl-U then a new name renames it"
        else
            bad "after renaming from inside, the services are: $("$gz" ls 2>/dev/null | awk 'NR>1{print $1}' | tr '\n' ' ')"
        fi
    else
        bad "could not get to first to rename it"
    fi

    # k: remove what you are looking at, which is the one that cannot be undone. With other
    # services still there, it removes this one and moves on rather than dropping you out.
    if goto doomed; then
        tmux -L "$tmuxSock" send-keys C-] 'k'
        sleep 3
        if ! "$gz" status doomed >/dev/null 2>&1; then
            ok "Ctrl-] k removes the service you are looking at"
        else
            bad "after k, doomed is still there: $("$gz" status doomed 2>/dev/null | head -2 | tr '\n' '|')"
        fi
        if ! pane | grep -q 'THE-ATTACH-ENDED'; then
            ok "and it moves on to another service rather than dropping you out"
        else
            bad "removing one service of three ended the attach"
        fi
    else
        bad "could not get to doomed to remove it: $(pane | tail -2 | tr '\n' '|')"
    fi

    # u, revive - which needed a fix before it could be checked at all.
    #
    # Arriving at a stopped service used to end the attach: the daemon sends EventFinished the
    # moment a client attaches to something already stopped, and outcomeFinished returned from the
    # whole loop. Pressing n onto a stopped tab dropped you back to your shell, and u - offered in
    # the help of both modes - could not be reached in the one case it exists for, because by then
    # the attach was over.
    #
    # Now it stays, which is what renderedsession.go already did on purpose for a pane in a split.
    # The two cases are told apart by whether the service was running when the client arrived: one
    # that ends *under* you is `exit` in the shell you were working in and still gives the terminal
    # back, and one that was already stopped is a tab you landed on.
    "$gz" stop second >/dev/null 2>&1
    sleep 1
    if goto second; then
        ok "Ctrl-] n onto a stopped service keeps you in gozellij"
        tmux -L "$tmuxSock" send-keys C-] 'u'
        sleep 4
        if "$gz" status second 2>/dev/null | grep -q '^state: *running'; then
            ok "and Ctrl-] u starts it again without leaving the attach"
        else
            bad "after reviving, second is: $("$gz" status second 2>/dev/null | grep '^state:')"
        fi
    else
        bad "landing on a stopped service lost the attach: $(pane | tail -2 | tr '\n' '|')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop renamed-inside second >/dev/null 2>&1
    "$gz" rm renamed-inside second doomed >/dev/null 2>&1

    # -------------------------------------- the status line as a command, and landing where you were
    #
    # Two things the document names that nothing here drove. `gozellij stats` is the status line as
    # a command - byobu-shaped, for a person who wants it in somebody else's bar - and it was
    # documented as **Verified** on the strength of a by-hand run. `gozellij shell -last` is the
    # answer to the rename question, and it was checked end to end by hand too. A promise nobody
    # re-checks is the same as a check that cannot fail: it is true until it is not, and nothing
    # says when that changed.
    only
    "$gz" add lastone -start -restart no -- cat >/dev/null 2>&1
    sleep 1

    # stats prints the line without a terminal at all, which is the whole point of it being a
    # command rather than only a row an attach draws.
    statline=$("$gz" stats 2>&1)
    if printf '%s' "$statline" | grep -q 'lastone'; then
        ok "stats prints the status line as a command, naming the service"
    else
        bad "stats said: $(printf '%s' "$statline" | head -2 | tr '\n' '|')"
    fi
    if printf '%s' "$statline" | grep -qE '[0-9]+/[0-9]+'; then
        ok "and it says which of how many, the way the attached line does"
    else
        bad "stats printed no service count: $(printf '%s' "$statline" | head -1)"
    fi

    # Now land somewhere, so there is a "where you were" to come back to. The record is written
    # when an attach starts showing something, not on the way out - people leave by closing the
    # window, and a client killed by a signal runs nothing on its way out.
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach lastone'"
    sleep 3
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    sleep 1

    # -last follows it.
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz shell -last'"
    sleep 4
    if pane | grep -q '\[lastone\]'; then
        ok "gozellij shell -last lands in what a terminal was last showing"
    else
        bad "-last landed elsewhere: $(pane | tail -2 | tr '\n' '|')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null

    # And without the flag nothing changed, which is the half that makes it safe to have: a login
    # still lands in the service called shell, whatever anybody was last looking at.
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz shell'"
    sleep 4
    if pane | grep -q '\[shell\]'; then
        ok "and a plain gozellij shell still lands in shell, as it always did"
    else
        bad "a plain shell landed elsewhere: $(pane | tail -2 | tr '\n' '|')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop lastone shell >/dev/null 2>&1
    "$gz" rm lastone shell >/dev/null 2>&1

    # ------------------------------------------ the prefix you configured is the prefix you get
    #
    # Every other check in this file presses Ctrl-], which is the default. The person this program
    # was written for does not use the default - he set prefix=C-b - so the whole suite has been
    # driving a configuration nobody runs, and the one that is actually in daily use was covered
    # only by a unit test of the parser. A parser that reads "C-b" correctly and an attach loop
    # that still watches for Ctrl-] would pass everything here and work for nobody.
    #
    # Three things have to be true, and the third is the one a unit test cannot see: the new key
    # works, the old key stops being special, and the help says the key you actually press.
    only
    mkdir -p "$home/.config/gozellij"
    printf 'prefix=C-b\n' > "$home/.config/gozellij/status"
    # cat, not a shell. Ctrl-] at a shell is readline's character-search, which eats the next
    # character all by itself - so a check that typed Ctrl-] at `sh -i` reported that gozellij was
    # still swallowing the key when what swallowed it was bash. cat has no keymap: what reaches it
    # comes back, which is the whole question here.
    "$gz" add prefixed -start -restart no -- cat >/dev/null 2>&1
    sleep 1
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; $gz attach prefixed; printf \"LEFT-THE-ATTACH\\n\"; sleep 60'"
    sleep 3

    # The help, which is the first thing a person presses when they are not sure. It has to name
    # the key they configured and must not name the old one: telling somebody to press Ctrl-]
    # when that now goes to their shell is worse than saying nothing.
    #
    # Written for "C-b" first, which is the spelling the config file takes, and it reads back as
    # "Ctrl-B" - PrefixLabel spells a key the way a person says it out loud rather than the way
    # they wrote it down. That is the better label, so the check moved rather than the code.
    tmux -L "$tmuxSock" send-keys C-b '?'
    sleep 2
    helpline=$(pane | grep 'detach' | tail -1)
    if printf '%s' "$helpline" | grep -qiE 'ctrl-b|c-b' && ! printf '%s' "$helpline" | grep -q 'Ctrl-]'; then
        ok "the configured prefix answers, and the help names it rather than the default"
    else
        bad "C-b ? said: $(printf '%s' "$helpline" | tr '\n' '|')"
    fi

    # The default must have stopped being special, or it is still being eaten and never reaches
    # the shell - which is exactly the bug somebody who rebinds would hit and nobody else would.
    #
    # Checked by typing rather than by detaching, and that is not a detail. The first version
    # pressed Ctrl-] d here: under a sabotage that ignored the configured prefix, that detached,
    # and the *next* check - that C-b d detaches - then found the attach already gone and passed
    # without testing anything. An earlier check's side effect making a later one pass is the
    # shadowing this suite has been caught by before. So: send Ctrl-] and then a command. If the
    # key is no longer special the shell sees a harmless control character and runs the command;
    # if it is still being eaten it swallows the "e" and the shell is handed "cho ...".
    #
    # cat echoes what it is given, so the marker comes back whole if the key was passed through
    # and missing its first letter if the client ate the key and took the M as a command.
    tmux -L "$tmuxSock" send-keys C-] 'MARKER-PASSED-THROUGH' Enter
    sleep 2
    if pane | grep -q 'MARKER-PASSED-THROUGH'; then
        ok "and Ctrl-] is no longer special, so it goes to the service"
    else
        bad "Ctrl-] was still eaten: $(pane | tail -3 | tr '\n' '|')"
    fi

    # Still attached, which the check above depends on and the one below needs.
    if pane | grep -q 'LEFT-THE-ATTACH'; then
        bad "the attach ended early, so what follows would prove nothing"
    else
        ok "and the attach is still running, so the next check means something"
    fi

    # Then the configured one actually does the thing.
    tmux -L "$tmuxSock" send-keys C-b 'd'
    for _ in $(seq 16); do
        pane | grep -q 'LEFT-THE-ATTACH' && break
        sleep 0.5
    done
    if pane | grep -q 'LEFT-THE-ATTACH'; then
        ok "and C-b d detaches, which is what it was rebound for"
    else
        bad "C-b d did not detach: $(pane | tail -2 | tr '\n' '|')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    rm -f "$home/.config/gozellij/status"
    "$gz" stop prefixed >/dev/null 2>&1
    "$gz" rm prefixed >/dev/null 2>&1

    # ------------------------------------- a connection that drops leaves nothing behind
    #
    # The way a login multiplexer's client usually ends is not Ctrl-] d. It is the ssh dying, the
    # laptop closing, the network going. So the daemon has to let go of a client that never said
    # goodbye - and "viewers" has to come back down, because the code counts viewers precisely so
    # that no viewers can mean nothing is attached. Anything that asks "is anybody looking at
    # this?" has no other signal.
    #
    # A leak here is the kind nobody notices for a week: every dropped connection leaves a
    # phantom, `ls` slowly counts higher, and the number stops meaning anything. Counting viewers
    # up was checked; nothing checked them coming back down.
    only
    "$gz" add dropped -start -restart no -- sh -c 'i=0; while :; do printf "DROP-%d\r\n" $i; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
    sleep 1
    droppid=$("$gz" status dropped 2>/dev/null | awk '/^pid:/{print $2}')
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach dropped'"
    sleep 3
    if [ "$("$gz" ls 2>/dev/null | awk '$1 == "dropped" {print $6}')" = "1" ]; then
        ok "an attached terminal is counted as one viewer"
    else
        bad "viewers reads [$("$gz" ls 2>/dev/null | awk '$1 == "dropped" {print $6}')] with one attached"
    fi

    # Killed, not detached: nothing on the way out gets to run, which is the whole point. The
    # pane's command is exec'd, so the pane pid is the client itself.
    clientPid=$(tmux -L "$tmuxSock" list-panes -F '#{pane_pid}' | head -1)
    kill -9 "$clientPid" 2>/dev/null
    for _ in $(seq 20); do
        [ "$("$gz" ls 2>/dev/null | awk '$1 == "dropped" {print $6}')" = "-" ] && break
        [ "$("$gz" ls 2>/dev/null | awk '$1 == "dropped" {print $6}')" = "0" ] && break
        sleep 0.5
    done
    left=$("$gz" ls 2>/dev/null | awk '$1 == "dropped" {print $6}')
    if [ "$left" = "0" ] || [ "$left" = "-" ]; then
        ok "a client killed without warning stops being counted"
    else
        bad "viewers still reads [$left] after the client was killed"
    fi

    # And the service does not care: same process, still running. A dropped connection that took
    # the service with it would be the opposite of the promise.
    if [ "$("$gz" status dropped 2>/dev/null | awk '/^pid:/{print $2}')" = "$droppid" ]; then
        ok "and the service it was watching is untouched, same pid"
    else
        bad "the service was $droppid and is now $("$gz" status dropped 2>/dev/null | awk '/^pid:/{print $2}')"
    fi

    # Then you come back, which is the part that matters to a person: reattaching after a drop
    # works, and shows the service still talking.
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach dropped'"
    sleep 4
    if pane | grep -q 'DROP-'; then
        ok "and you can attach again afterwards and see it running"
    else
        bad "reattaching after a drop showed nothing: $(pane | head -3 | tr '\n' '|')"
    fi
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop dropped >/dev/null 2>&1
    "$gz" rm dropped >/dev/null 2>&1

    # ------------------------------------------ you can always get out, however loud it is
    #
    # The accident everybody has had: cat a binary, or start something that never stops talking.
    # What matters then is not that the screen is a mess - it is whether the detach key still
    # works. A multiplexer you cannot leave while a service is shouting is worse than no
    # multiplexer, because the shouting is exactly when you need to leave.
    #
    # Nothing checked this. The flood further up exists to fill a log and watch it rotate; it
    # never had anybody attached to it. The parts are all there - the keystroke reader is its own
    # goroutine, and a client that falls behind is told so and cut loose rather than blocking the
    # daemon - but "the parts are there" is a claim about the code, not about what happens.
    only
    "$gz" add torrent -start -restart no -- sh -c 'yes "TORRENT-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"' >/dev/null 2>&1
    sleep 1
    torrentpid=$("$gz" status torrent 2>/dev/null | awk '/^pid:/{print $2}')
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 60 -y 8 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e TERM=xterm-256color \
        "sh -c 'stty -echo; $gz attach torrent; printf \"BACK-AT-THE-PROMPT\\n\"; sleep 60'"
    sleep 4

    # It is actually shouting, or the rest of this proves nothing about a flood.
    if pane | grep -q 'TORRENT-'; then
        ok "a service that never stops talking fills the screen"
    else
        bad "the flood never reached the screen: $(pane | head -2 | tr '\n' '|')"
    fi

    tmux -L "$tmuxSock" send-keys C-] 'd'
    # Longer than the other detaches wait. The point is that it gets out at all, and a client
    # working through a backlog is allowed to take a moment doing it.
    for _ in $(seq 20); do
        pane | grep -q 'BACK-AT-THE-PROMPT' && break
        sleep 0.5
    done
    if pane | grep -q 'BACK-AT-THE-PROMPT'; then
        ok "Ctrl-] d gets you out of it"
    else
        bad "the detach never completed under a flood: $(pane | tail -2 | tr '\n' '|')"
    fi

    # And the daemon is still a daemon afterwards: still answering, with the service still running
    # and still the same process. A flood that takes the daemon with it would take every other
    # service too, which is the failure that matters more than the one on screen.
    if "$gz" ls >/dev/null 2>&1; then
        ok "and the daemon still answers after all that"
    else
        bad "the daemon stopped answering"
    fi
    if [ -n "$torrentpid" ] && [ "$("$gz" status torrent 2>/dev/null | awk '/^pid:/{print $2}')" = "$torrentpid" ]; then
        ok "and the service is still running, still the same process"
    else
        bad "the service was $torrentpid and is now $("$gz" status torrent 2>/dev/null | awk '/^pid:/{print $2}')"
    fi

    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" stop torrent >/dev/null 2>&1
    "$gz" rm torrent >/dev/null 2>&1

    # --------------------------------- changing a service in place, and renaming a running one
    #
    # Story A5 and its neighbour. Both were reviewed adversarially at the fabric level and five
    # things came out of it; what nothing drives is the whole path - the command, the daemon, and
    # a process that is actually running while you change the definition underneath it.
    only
    "$gz" add setme -start -restart no -- sh -c 'echo FIRST; sleep 300' >/dev/null 2>&1
    sleep 2
    setpid=$("$gz" status setme 2>/dev/null | awk '/^pid:/{print $2}')

    # A set with nothing to change is refused rather than answered as though it had.
    if "$gz" set setme >/dev/null 2>&1; then
        bad "a set with no flags was accepted"
    else
        ok "a set that would change nothing is refused"
    fi

    # And one that does change something says the running process is still the old definition,
    # and what to type. Saying "done" while the service runs the previous command is the failure
    # this message exists to prevent.
    said=$("$gz" set setme -restart always 2>&1)
    if printf '%s' "$said" | grep -q 'previous definition' && printf '%s' "$said" | grep -q 'restart setme'; then
        ok "set says the running service is still the old definition, and what to type"
    else
        bad "set said: $(printf '%s' "$said" | tail -2 | tr '\n' '|')"
    fi
    # Setting it to what the running process already has is not a change, and must not be
    # answered as one. setme was started with -restart no, so this asks for nothing. The CLI
    # refuses a set with no flags at all; this is the case it cannot see - flags were given, and
    # their values match what the process is running. Being told to restart a service to pick up
    # a change that does not exist costs you the process for no reason.
    noop=$("$gz" set setme -restart no 2>&1)
    if printf '%s' "$noop" | grep -q 'previous definition'; then
        bad "setting a value the running process already has still asked for a restart"
    else
        ok "setting a value the running process already has asks for no restart"
    fi
    # And saying it again still says it. The process is still the old definition until it is
    # restarted, so there is still something a restart would pick up - the question the message
    # answers is about the running process, not about whether this particular command changed
    # anything. A first attempt at the no-op case above compared the definition before and after
    # the call, which got this exactly backwards: forget to restart, repeat the set, and be told
    # there is nothing to pick up while the old process is still running.
    repeat=$("$gz" set setme -restart always 2>&1)
    if printf '%s' "$repeat" | grep -q 'previous definition'; then
        ok "repeating a change still says the running process has not picked it up"
    else
        bad "the second set went quiet while the process was still the old definition: $(printf '%s' "$repeat" | tail -2 | tr '\n' '|')"
    fi
    if [ "$("$gz" status setme 2>/dev/null | awk '/^pid:/{print $2}')" = "$setpid" ]; then
        ok "and it did not restart the service behind your back"
    else
        bad "the service was restarted by a set: $setpid became $("$gz" status setme 2>/dev/null | awk '/^pid:/{print $2}')"
    fi

    # Renaming a *running* service: it keeps its pid, answers to the new name, and its log goes
    # with it. A rename that left the log behind would lose everything the service had said.
    "$gz" rename setme renamed >/dev/null 2>&1
    sleep 1
    if [ "$("$gz" status renamed 2>/dev/null | awk '/^pid:/{print $2}')" = "$setpid" ]; then
        ok "a running service keeps its pid across a rename"
    else
        bad "the pid changed across the rename: $setpid became $("$gz" status renamed 2>/dev/null | awk '/^pid:/{print $2}')"
    fi
    if "$gz" status setme >/dev/null 2>&1; then
        bad "the old name still answers after a rename"
    else
        ok "and the old name is gone"
    fi
    if [ -f "$state/logs/renamed.log" ] && [ ! -f "$state/logs/setme.log" ]; then
        ok "and its log went with it"
    else
        bad "logs after the rename: $(ls "$state/logs" 2>/dev/null | tr '\n' ' ')"
    fi
    if "$gz" logs renamed 2>/dev/null | grep -q FIRST; then
        ok "and what it said before the rename is still readable"
    else
        bad "the output from before the rename is gone"
    fi
    "$gz" rm renamed >/dev/null 2>&1

    # ------------------------------------------- Ctrl-C stops watching, not the service
    #
    # The README promises this in its first twenty lines, and nothing checked the half that
    # matters. The suite runs `logs -f` three times and every one of them ends it with `timeout`,
    # which sends SIGTERM and asks a different question. "Ctrl-C killed the thing I was only
    # looking at" is the kind of surprise that ends somebody's trust in a tool immediately.
    only
    "$gz" add watched_f -start -restart no -- sh -c 'i=0; while :; do echo "WATCHED-$i"; i=$((i+1)); sleep 1; done' >/dev/null 2>&1
    sleep 2
    before_pid=$("$gz" status watched_f 2>/dev/null | awk '/^pid:/{print $2}')

    # A real SIGINT to the follower, the way the terminal would deliver it.
    "$gz" logs -f watched_f > "$work/follow.out" 2>&1 &
    follower=$!
    sleep 2
    kill -INT "$follower" 2>/dev/null
    wait "$follower" 2>/dev/null
    sleep 2

    after_pid=$("$gz" status watched_f 2>/dev/null | awk '/^pid:/{print $2}')
    if [ -n "$before_pid" ] && [ "$before_pid" = "$after_pid" ]; then
        ok "Ctrl-C at a follower leaves the service running, with the same pid"
    else
        bad "the service went from pid [$before_pid] to [$after_pid] when the follower was interrupted"
    fi
    if [ -s "$work/follow.out" ]; then
        ok "and the follower had been showing output before it stopped"
    else
        bad "the follower printed nothing, so this proves nothing about stopping it"
    fi
    "$gz" rm watched_f >/dev/null 2>&1
    rm -f "$work/follow.out"

    # ------------------------------------- the same comparison, with a pager rather than an editor
    #
    # vim and less stress different parts of an emulator. vim paints absolutely, positioning the
    # cursor for every change; less scrolls, draws a reverse-video prompt on the bottom row and
    # rewrites it in place as you move. One of them agreeing with tmux says less than both do.
    #
    # The same file under the same *basename* in two directories, so the prompt line - which
    # contains the name less was given - is identical on both sides. Pointing them at wide-one and
    # wide-two would make the screens differ by the filename and nothing else, which is the kind of
    # check that gets "fixed" by loosening it until it proves nothing.
    only
    mkdir -p "$home/g" "$home/t"
    # Wide characters on every line, not only the first few. The first version put them at the top
    # and then paged down past them, so the guard below - "the reference did not draw the wide
    # characters" - fired on a screen that was scrolled somewhere else entirely. The guard was
    # right and the file was wrong.
    seq 1 40 | sed 's/$/ \xe6\x97\xa5\xe6\x9c\xac\xe8\xaa\x9e caf\xc3\xa9/' > "$home/g/doc.txt"
    cp "$home/g/doc.txt" "$home/t/doc.txt"
    "$gz" add pager -dir "$home/g" -start -- sh -c "TERM=xterm-256color LANG=C.UTF-8 exec less doc.txt" >/dev/null 2>&1
    sleep 1
    plain2="${tmuxSock}-plain2"
    newscreen
    tmux -L "$tmuxSock" new-session -d -x 80 -y 24 \
        -e GOZELLIJ_RUNTIME_DIR="$run" -e GOZELLIJ_STATE_DIR="$state" -e HOME="$home" \
        -e GOZELLIJ_RENDER=1 -e TERM=xterm-256color \
        "sh -c 'stty -echo; exec $gz attach pager'"
    tmux -L "$plain2" new-session -d -x 80 -y 23 -e TERM=xterm-256color -e LANG=C.UTF-8 \
        "sh -c 'cd $home/t && stty -echo && exec less doc.txt'"
    sleep 3
    # Down a page and back up one line: scrolling is the part vim never exercises.
    tmux -L "$tmuxSock" send-keys Space; tmux -L "$plain2" send-keys Space
    sleep 1
    tmux -L "$tmuxSock" send-keys k; tmux -L "$plain2" send-keys k
    sleep 2

    tmux -L "$tmuxSock" capture-pane -pe | head -22 > "$home/pager-gozellij.txt"
    tmux -L "$plain2" capture-pane -pe | head -22 > "$home/pager-tmux.txt"
    if ! grep -q '日本語' "$home/pager-tmux.txt"; then
        bad "the reference pager did not draw the wide characters, so this comparison proves less than it claims"
    elif diff -q "$home/pager-gozellij.txt" "$home/pager-tmux.txt" >/dev/null; then
        ok "gozellij's emulator and tmux's draw a scrolling pager identically"
    else
        bad "the two emulators disagree on the pager: $(diff "$home/pager-gozellij.txt" "$home/pager-tmux.txt" | head -4 | tr '\n' ' ')"
    fi

    tmux -L "$plain2" kill-server 2>/dev/null
    tmux -L "$tmuxSock" kill-server 2>/dev/null
    "$gz" rm pager >/dev/null 2>&1

    # Not checked here: that attaching does not overwrite the line you typed the command on.
    #
    # It is a real bug when it happens - reserving the bottom row used to land the cursor on the
    # last line of your content, and the replayed prompt printed over it, leaving it in neither
    # the screen nor the scrollback - but two attempts to reproduce it from this script did not,
    # and a check that passes when the fix is removed is worse than no check. It reproduces by
    # hand; the recipe is in docs/REPLACING_GEZELLIJ.md so that anyone can repeat it.
fi

say
skipped=""
[ "$noscreen" = 1 ] && skipped=" (the screen checks were skipped, so this is not the whole suite)"
if [ "$fail" -eq 0 ]; then
    say "all $pass promises held.$skipped"
else
    say "$pass held, $fail did not.$skipped"
fi
exit $(( fail > 0 ? 1 : 0 ))
