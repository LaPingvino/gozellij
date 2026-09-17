# gozellij

**Your processes keep running.** Through upgrades, through logouts, through the UI crashing,
through you rebooting the laptop you were sshed in from.

A host-native process fabric with a terminal UI on top — aiming to be lighter and safer than
Docker, and easier to live with, for the job most people actually use Docker for on one server:
run a handful of long-lived things, keep them alive, look at them, restart them, give them
addresses.

> **Status: early. Nothing here is usable yet.** The design is written down
> ([DESIGN.md](DESIGN.md)); the code is a foundation, not a product.

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

## Build

```sh
go build ./...
go test ./...
```

## Name

Zellij is Moroccan tilework. `gezellij` added Dutch *gezellig* — cosy, convivial, the pleasure of
being somewhere comfortable. `gozellij` keeps that and adds the gopher.

## Licence

MIT.
