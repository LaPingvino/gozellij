#!/usr/bin/env bash
#
# Ask tmux what it answers to the questions a program asks a terminal, and print what came back.
#
#   ./scripts/queryprobe.sh
#
# The emulator has to decide what to reply to DECRQSS ("what is the current setting of ...") and
# XTGETTCAP ("what does your terminfo say about ..."), and those formats are easy to write from
# memory and get wrong - the validity digit is 0 in one reading of the spec and 1 in the other,
# and whether a refused XTGETTCAP echoes the capability names back differs between terminals. A
# format written from memory and then pinned by a test is a guess promoted to a specification.
#
# So: measure. tmux is the oracle this project already uses for screens; this asks it about
# replies. What it answered when internal/vt/grid/parser.go was written:
#
#   DECRQSS, every setting including valid ones -> \x1bP0$r\x1b\\
#   XTGETTCAP, known and unknown capabilities   -> nothing at all
#
# Run it again before changing what finishStr replies, and make the code match what it prints.
set -u

command -v tmux >/dev/null || { echo "no tmux, nothing to ask" >&2; exit 1; }
command -v python3 >/dev/null || { echo "no python3" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cat > "$work/probe.py" <<'PY'
import os, sys, termios, tty, time, select

QUERIES = [
    ("DECRQSS, a setting that is not one", b"\x1bP$qzz\x1b\\"),
    ("DECRQSS, the current SGR",           b"\x1bP$qm\x1b\\"),
    ("DECRQSS, the scrolling region",      b"\x1bP$qr\x1b\\"),
    ("XTGETTCAP, an unknown capability",   b"\x1bP+q7a7a\x1b\\"),
    ("XTGETTCAP, the terminal name",       b"\x1bP+q544e\x1b\\"),
    ("XTGETTCAP, several at once",         b"\x1bP+q544e;7a7a\x1b\\"),
]

fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
tty.setraw(fd)
lines = []
try:
    for name, q in QUERIES:
        while select.select([fd], [], [], 0.05)[0]:
            os.read(fd, 4096)
        os.write(1, q)
        got = b""
        deadline = time.time() + 0.6
        while time.time() < deadline:
            if select.select([fd], [], [], 0.1)[0]:
                got += os.read(fd, 4096)
        lines.append("  %-36s %s" % (name, repr(got) if got else "(no answer)"))
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
open(os.environ["PROBE_OUT"], "w").write("\n".join(lines) + "\n")
PY

sock="queryprobe-$$"
echo "asking tmux $(tmux -V | cut -d' ' -f2):"
PROBE_OUT="$work/out" tmux -L "$sock" new-session -d -x 80 -y 24 \
    -e PROBE_OUT="$work/out" "python3 $work/probe.py"
# Long enough for six queries at 0.6s each, plus room.
sleep 6
tmux -L "$sock" kill-server 2>/dev/null
if [ -s "$work/out" ]; then
    cat "$work/out"
else
    echo "  the probe wrote nothing - it may not have run at all" >&2
    exit 1
fi
