#!/usr/bin/env bash
#
# Reproduce the two-terminals-on-one-service failure, in isolation and under load.
#
#   ./scripts/pairrace.sh [runs] [parallel]     # default: 24 runs, 6 at a time
#
# WHAT IT REPRODUCES
#
# Two terminals attach to one service. Output typed in the first appears in the second and never
# in the first. The acceptance suite sees it as
#
#   FAIL  the answer appeared in pane0=0 pane1=1; both should have it
#   FAIL  what was typed in the second terminal never reached the service
#
# and it is not a flake in the suite: the same arrangement, on its own, fails at about the same
# rate. It needs load to show up at all, which is why this runs several copies at once - twenty-four
# runs on an idle machine found nothing, and six at a time found two in eighteen.
#
# WHAT IS KNOWN, from the instrumented failures this kept
#
#   - the first client is still attached: `viewers` reads 2, and its status line is still being
#     repainted with a current clock, so the process is alive and writing to its terminal;
#   - its keystrokes still reach the service - the second terminal shows the echo of what was typed
#     in the first;
#   - it received the *replay*: a marker the service printed before either client attached is in
#     its scrollback. So the attach, the snapshot and the subscription all worked;
#   - after that it receives nothing. Not late, not scrolled off - `capture-pane -S -200` does not
#     find it either;
#   - nothing is reported. Its stderr is empty, and the daemon logs nothing.
#
# WHAT HAS BEEN RULED OUT
#
#   - lagging. Both paths in Subscriber.offer close the channel, which ends the client's pump and
#     hangs up the connection; this client stays connected, so it was never marked lagged.
#   - a subscription to a superseded OutputBuffer. replaceSupervisor carries the buffer across, so
#     there is only ever one.
#   - the resize. split-window resizes the first pane, but one attach resized six times over loses
#     nothing: 0 failures in 10.
#
# WHAT THE DAEMON DOES, MEASURED
#
# Atomic counters on the daemon side - cheap enough not to perturb it, unlike everything else
# tried - said "offered=199 wrote=199" and "offered=118 wrote=118", neither lagged, in a failing
# run. Every byte the daemon had, it wrote to both clients. So the daemon is not dropping anything
# and the bytes reach the client that does not show them.
#
# AND NOT A DATA RACE
#
# Twenty-four runs of both binaries built with -race, GORACE=log_path so the reports survive the
# per-run cleanup: no report at all. Together with the counters that places the fault in the
# *terminal's* state - where the cursor is, which scrolling region is in force - and not in Go
# memory. That is a class the race detector can never report, so its silence is a result rather
# than a disappointment, and it matches the shape of the thing: a permanently blank body under a
# status line that is still ticking is what a cursor parked on the reserved row looks like, every
# repaint painting over what just arrived.
#
# On a passing run the cursor ends at row 8 of a 10-row pane, the status line being row 9. A
# failing run has never been captured with the cursor query in place, which is the measurement
# this is waiting for.
#
# WHY IT IS HARD TO SEE
#
# Every attempt to observe it from inside the client made it stop: per-frame logging, an
# in-memory ring, and a single atomic add, 0 in 96 between them. Daemon-side instrumentation did
# *not* hide it, which is part of why the client is the suspect.
#
# And then it stopped anyway. About five failures were seen in the first hundred runs and none in
# the two hundred after, with the same pre-fix binary, at six- and ten-way parallelism, idle and
# alongside a running acceptance suite. So the conditions are not pinned down, and one honest
# possibility is that the earlier reproductions owed something to whatever else this machine was
# doing at the time - several full acceptance suites were running through that period.
#
# A confound worth remembering: running thirty copies of this script at once is a
# *self-synchronising* load. Same code, same sleeps, same points reached together. The acceptance
# suite is heterogeneous - tmux servers starting and dying, daemons upgrading, ptys opening, vim
# redrawing - and a better generator of odd interleavings. Eighty runs went into learning that the
# two are not the same thing.
#
# 04fda77 fixed a real defect found while looking for this - the status line built its scrolling
# region from a size read outside the lock guarding the write - but there is no evidence that it
# was this. It cannot be told apart from perturbation by counting, which is why it was proved
# deterministically instead and claimed narrowly.
set -u

runs=${1:-24}
par=${2:-6}
here=$(cd "$(dirname "$0")/.." && pwd)
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT

echo "building..."
(cd "$here" && go build -o "$bin/gozellij" ./cmd/gozellij && go build -o "$bin/gozellijd" ./cmd/gozellijd) || exit 1

one() {
    n=$1
    W=$(mktemp -d)
    mkdir -p "$W/run" "$W/state" "$W/home"
    env -i PATH=/usr/bin:/bin HOME="$W/home" \
        GOZELLIJ_RUNTIME_DIR="$W/run" GOZELLIJ_STATE_DIR="$W/state" \
        "$bin/gozellijd" > "$W/daemon.log" 2>&1 &
    dpid=$!
    for _ in $(seq 40); do [ -S "$W/run/fabric.sock" ] && break; sleep 0.25; done

    export GOZELLIJ_RUNTIME_DIR="$W/run" GOZELLIJ_STATE_DIR="$W/state" HOME="$W/home"
    "$bin/gozellij" add pair -start -restart always -- \
        sh -c 'printf "READY-MARK\r\n"; PS1=""; export PS1; exec /bin/sh -i' >/dev/null 2>&1
    sleep 1

    S="pairrace-$$-$n"
    E="-e GOZELLIJ_RUNTIME_DIR=$W/run -e GOZELLIJ_STATE_DIR=$W/state -e HOME=$W/home -e TERM=xterm-256color"
    tmux -L "$S" new-session -d -x 80 -y 20 $E "sh -c 'stty -echo; exec $bin/gozellij attach pair 2>$W/c0.err'"
    tmux -L "$S" split-window -d $E "sh -c 'stty -echo; exec $bin/gozellij attach pair 2>$W/c1.err'"
    sleep 3
    viewers=$("$bin/gozellij" ls 2>/dev/null | awk '$1 == "pair" {print $6}')
    tmux -L "$S" send-keys -t 0 'echo BOTH-$((6*7))-SEE' Enter
    sleep 3
    # With scrollback: "never arrived" and "arrived and scrolled out of view" are different bugs.
    p0=$(tmux -L "$S" capture-pane -p -S -200 -t 0 2>/dev/null | grep -c 'BOTH-42-SEE')
    p1=$(tmux -L "$S" capture-pane -p -S -200 -t 1 2>/dev/null | grep -c 'BOTH-42-SEE')
    r0=$(tmux -L "$S" capture-pane -p -S -200 -t 0 2>/dev/null | grep -c 'READY-MARK')
    ok=0
    [ "${p0:-0}" -gt 0 ] && [ "${p1:-0}" -gt 0 ] && ok=1
    if [ "$ok" = 1 ]; then
        echo "run $n ok"
    else
        echo "run $n FAILED  viewers=$viewers first=$p0 second=$p1 replay-reached-first=$r0  kept in $W"
        { echo "--- pane 0:"; tmux -L "$S" capture-pane -p -S -200 -t 0
          echo "--- pane 1:"; tmux -L "$S" capture-pane -p -S -200 -t 1
          echo "--- first client stderr:"; cat "$W/c0.err" 2>/dev/null
          echo "--- second client stderr:"; cat "$W/c1.err" 2>/dev/null
          echo "--- status:"; "$bin/gozellij" status pair 2>&1
        } > "$W/evidence.txt" 2>&1
    fi
    tmux -L "$S" kill-server 2>/dev/null
    "$bin/gozellij" stop pair >/dev/null 2>&1
    kill "$dpid" 2>/dev/null
    [ "$ok" = 1 ] && rm -rf "$W"
    return $((1 - ok))
}

fails=0
done_runs=0
while [ "$done_runs" -lt "$runs" ]; do
    pids=""
    i=0
    while [ "$i" -lt "$par" ] && [ "$done_runs" -lt "$runs" ]; do
        one "$done_runs" & pids="$pids $!"
        i=$((i + 1)); done_runs=$((done_runs + 1))
    done
    for p in $pids; do wait "$p" || fails=$((fails + 1)); done
done
echo
echo "$fails of $runs failed"
[ "$fails" -eq 0 ]
