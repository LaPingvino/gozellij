# gozellij

**Your processes keep running.** Through logouts, through the UI crashing, through upgrading the
daemon underneath them, through you rebooting the laptop you were sshed in from.

```
$ gozellij upgrade
daemon upgraded: v1 -> v2
1 process(es) kept their pid     # same process; it never noticed
```

> A daemon that *crashes* still takes its services with it — exec-in-place is a planned upgrade,
> not a safety net. [DESIGN.md](DESIGN.md) says what that would take.

A host-native process fabric with a terminal UI on top — aiming to be lighter and safer than
Docker, and easier to live with, for the job most people actually use Docker for on one server:
run a handful of long-lived things, keep them alive, look at them, restart them, give them
addresses.

> **Status: Phase 1 works.** You can define services, supervise them, watch them, and upgrade the
> daemon underneath them without restarting anything. There is no multiplexing yet — one service,
> one terminal, no panes. That is [Phase 3](#plan), and it is the expensive part.

## The idea

Most multiplexers own the processes they show you. That makes "keep the processes alive while
replacing the thing that shows them" into an engineering project — exec-in-place that preserves
the pid, file-descriptor passing, adoption plans, recovery for sessions stranded mid-upgrade. We
built all of that in [gezellij](https://github.com/LaPingvino/gezellij) and it works.

Turn it around instead:

```
  ┌────────────────────────────────────────────────┐
  │  gozellijd  — the fabric (owns everything)     │
  │    processes, PTYs, cgroups, supervision,      │
  │    addresses, on-disk state                    │
  └────────────────────────────────────────────────┘
                     ▲  unix socket
        ┌────────────┼────────────┐
        │            │            │
   gozellij      gozellij     anything else
   attach        (mux UI)     (scripts, a web UI)
```

The UI holds nothing worth keeping. Restart it, upgrade it, kill it — the fabric never noticed.
Processes surviving a UI restart stops being a feature and becomes structural, because the UI was
never holding the PTYs.

## Quickstart

```sh
go build -o ~/.local/bin/gozellijd ./cmd/gozellijd
go build -o ~/.local/bin/gozellij  ./cmd/gozellij
gozellijd &                       # or install the systemd user unit, see packaging/
```

Define something and let the fabric look after it:

```sh
$ gozellij add clock -restart always -start -- sh -c 'while :; do date +%T; sleep 1; done'
service:  clock
state:    running
pid:      1027290

$ gozellij ls
NAME   STATE    PID      UPTIME  RESTARTS  COMMAND
clock  running  1027290  9s      -         sh

$ gozellij logs clock -n 40      # recent output, without attaching
$ gozellij attach clock          # watch it live; Ctrl-] detaches, it keeps running
```

Now upgrade the daemon under it:

```sh
$ go build -o ~/.local/bin/gozellijd ./cmd/gozellijd   # a new binary
$ gozellij upgrade
upgrading the daemon, carrying 1 process(es)...
daemon upgraded: v1 -> v2
1 process(es) kept their pid
```

`clock` never restarted. Its pid, uptime and scrollback are unchanged, and anything it had open is
still open — the daemon replaced its own image with `syscall.Exec`, which keeps the pid, so the
ptys stayed open and the children stayed children. `gozellij upgrade` compares the pids itself and
fails loudly if any process did not survive, because a promise like that is worth checking rather
than asserting.

What it does **not** do yet: split a terminal into panes. A single attach is a byte pipe — your
terminal does the emulating. See [the plan](#plan).

### Commands

| | |
|---|---|
| `gozellij add <name> [flags] -- <cmd>` | define a service (`-restart no\|on-failure\|always`, `-start`, `-dir`, `-env`) |
| `gozellij ls` | what exists and what it is doing |
| `gozellij status <name>` | one service in detail, including why it will not start |
| `gozellij start\|stop\|restart <name>` | change its state; `stop` means it stays stopped across a reboot |
| `gozellij logs <name> [-n bytes]` | recent output without attaching |
| `gozellij attach <name>` | connect your terminal; `Ctrl-]` detaches without stopping anything |
| `gozellij upgrade` | replace the daemon binary, keeping every process |
| `gozellij rm <name>` | stop it and forget it |

## Why start again, in Go

`gozellij` follows [gezellij](https://github.com/LaPingvino/gezellij), a Zellij fork that is in
daily use. The reason to start over is what we measured in that fork:

- **332,962** lines of Rust, of which **~4,100 (1.2%)** were this project's actual idea
- **~42 minutes** for a full rebuild; **30s–2m39s** to see a one-line change
- a **227 MiB** debug binary

We were maintaining a terminal emulator, a layout engine, a WASM plugin host and a renderer in
order to ship 1.2%. And the iteration tax lands hardest on the one genuinely hard part of this
project — terminal emulation — where correctness comes from thousands of fast cycles against an
oracle, not from careful reading.

No claim that Rust was the problem. Every bug fixed on the fork's last day was a design bug —
one-second timeouts, a 100 ms budget that blanked a column silently, a function returning success
for a message it had thrown away. The type system encodes none of that.

## Plan

1. **The fabric, and single-pane attach — no terminal emulator.** Proxy the child's PTY bytes
   straight through; your terminal is the emulator. Supervision, restart policy with backoff,
   cgroup freeze/thaw, per-service addresses, in-place upgrade. Useful on its own.
2. **The oracle.** A `vt.Terminal` interface, a differential harness that diffs two
   implementations cell by cell, and a conformance corpus replayed from real programs. Built
   *before* the emulator, so emulator work is machine-checked from its first line.
3. **Multiplexing.** Panes, layouts, UI.
4. **Plugins, if wanted.** Cheaper than it looks: Zellij's whole plugin ABI is one wasm host
   function plus protobuf, and its default plugins are prebuilt `wasm32-wasip1` artifacts that
   `wazero` can host with no cgo.

See [DESIGN.md](DESIGN.md) for the reasoning, the Go terminal-emulator survey behind step 2, and
the design rules.

## Build and test

```sh
go build ./...
go test ./...
go test -race ./...
```

Both suites are expected to pass. See [packaging/](packaging/) for the systemd user unit.

## Name

Zellij is Moroccan tilework. `gezellij` added Dutch *gezellig* — cosy, convivial, the pleasure of
being somewhere comfortable. `gozellij` keeps that and adds the gopher.

## Licence

MIT.
