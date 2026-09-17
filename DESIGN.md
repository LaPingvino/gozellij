# Gozellij — design

## What this is

A host-native process fabric with a terminal UI on top. The promise, in one line:

> Your processes keep running. Through upgrades, through logouts, through the UI crashing,
> through you rebooting the laptop you were sshed in from.

It is meant to be **lighter and safer than Docker, and easier to live with**, for the job most
people actually use Docker for on a single server: run a handful of long-lived things, keep them
alive, look at them, restart them, give them addresses.

It is *not* an attempt to be a better tmux. The multiplexer is the face, not the product.

## Why it exists (and why in Go)

This is a reimplementation of [gezellij](https://github.com/LaPingvino/gezellij), which is a fork
of [Zellij](https://github.com/zellij-org/zellij). That fork works and is in daily use. The reason
to start again is not that Rust is bad; it is what the numbers said when we measured the fork:

| | |
|---|---|
| Total Rust in the fork | 332,962 lines |
| The part that is *this project's idea* | ~4,100 lines (1.2%) |
| Full rebuild | ~42 minutes |
| Touch one file in `zellij-server` | 30s–2m39s |
| Debug binary | 227 MiB |

98.8% of the code we were maintaining was terminal emulator, layout engine, WASM plugin host and
renderer — the parts we did not come for. And every one-line change to the 1.2% cost between half
a minute and three minutes to see.

That tax matters most exactly where this project is hardest (see "the grid problem" below), because
the only tractable way to get a terminal emulator right is thousands of fast iterations against an
oracle. At 90s a cycle you batch fixes and lose attribution. At 1s you bisect every disagreement.

Worth being honest about the other direction too: every bug we fixed in the Rust fork on the last
day of work — five one-second timeouts, a `100ms` budget that blanked a column silently, a
`send_to_client` that returned `Ok(())` for a message it discarded, a reply lost in a thread race —
was a *design* bug. Rust's guarantees were never in play. The borrow checker will not save you from
"silently succeed at doing nothing".

## The core inversion

In Zellij, the multiplexer owns the processes. Keeping processes alive across an upgrade therefore
took real engineering in the fork: an exec-in-place that preserves the pid, `SCM_RIGHTS` fd passing,
PTY adoption plans, a manifest on disk, and a rescue path for stranded sessions.

Invert it:

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

The UI holds no state worth keeping. Restart it, upgrade it, kill it — the fabric never noticed.
"Processes survive a UI restart" stops being a feature we engineer and becomes structural, because
the UI was never holding the PTYs.

This is also what makes the UI replaceable, which matters because the UI is the part chosen on
taste.

## The grid problem

Splitting a terminal into panes means owning a terminal emulator. We researched what Go has
(2026-09-17). The short version:

- **`charmbracelet/x/vt`** — a real grid emulator: grapheme clustering, scrollback, damage
  tracking, alt screen, SGR mouse (?1006), bracketed paste, DECSTBM/DECSLRM. ~2.9k lines.
  But: repo labelled "experimental", **no tagged release**, **no reflow on resize**, no sixel or
  kitty graphics, a fixed **4 MiB parser buffer per emulator** (50 panes = 200 MB), and open
  correctness bugs including a **panic on scroll-after-shrink** (#974) and **missing IRM** (#949).
- **`hinshun/vt10x`** — dead since 2023, 1,522 lines, zero dependencies, therefore no wide-char or
  grapheme handling at all. Not a candidate.
- **`go.mitchellh.com/libghostty`** — cgo bindings to Ghostty's VT library. Full semantics
  including reflow, for free. Costs `CGO_ENABLED=0`, easy cross-compilation, and needs Zig in the
  build. API explicitly not stable yet.

The decisive data point: **tuios**, the only Go multiplexer with real users, forked `x/vt`, roughly
tripled it, added ~9x its test volume — and still ships its *releases* on libghostty via cgo,
keeping both behind one interface with a differential fuzz harness between them.

So the grid is not cheap the way the plugin ABI is cheap. Which leads directly to the plan.

## Plan: the useful thing first, the hard thing last

**Phase 1 — the fabric, and single-pane attach. No terminal emulator at all.**

A single-pane attach does not need an emulator: the child's PTY output goes to your terminal
verbatim and your keystrokes go back, like `dtach`. Your terminal is the emulator. This is
already the whole product for "run things on a server and keep them alive" — supervision, restart
policy with backoff, cgroup freeze/thaw, per-service ULA addresses, in-place upgrade.

Shippable, useful, and it carries no VTE risk.

**Phase 2 — the oracle.** Before any emulator work: a `vt.Terminal` interface, a differential test
harness, and the conformance corpus. Drive identical byte streams into two implementations, diff
grid + cursor + attributes, and treat every disagreement as an auto-generated bug report. Replay
captures from real programs (neovim, tmux, htop, CJK-heavy `ls`, resize mid-render). Build this
*first* so that emulator work is machine-checked from its first line rather than eyeballed.

**Phase 3 — multiplexing.** Panes, layouts, the UI. By now the grid has a safety net.

**Phase 4 — plugins, if wanted.** The cheapest part, surprisingly: Zellij's entire plugin ABI is
**one** wasm host function (`host_run_plugin_command`) with everything else carried as protobuf
over it (14 `.proto` files, ~3,000 lines). The 13 default plugins are committed `wasm32-wasip1`
artifacts — they would run unmodified. `wazero` hosts WASI preview1 in pure Go, no cgo. That their
source is Rust is irrelevant; we consume the binaries.

## Design rules

Earned the hard way in the Rust fork; these are not aspirations.

1. **Never silently succeed at doing nothing.** An empty answer must be distinguishable from "no
   data". A discarded message must not return success. A query that gives up must say so, on a
   channel the caller actually reads. Most of the last day of fork work was this one bug, five
   times over.
2. **No magic-number timeouts.** Named constants, in one place, generous by default. A short
   timeout is a bet that the machine is idle and the build is optimised; servers are neither.
3. **Nested timeouts go inner-shorter-than-outer**, so the error names the thing that actually
   hung rather than the outermost wrapper.
4. **Anything needing root is a one-time documented setup step**, never something the binary does
   at runtime.
5. **State the fabric must not lose goes on disk**, in a format a human can read and repair.
6. **Measure before claiming.** The fork's history includes a linker change committed with the
   measurements that *failed* to support it, and a confident theory implemented, measured,
   refuted, and reverted. Both are better outcomes than a plausible story.

## Not yet decided

- Which VTE backend leads in Phase 3, and whether cgo is acceptable for release builds.
- Wire format on the fabric socket. Leaning length-prefixed protobuf for the same reason Zellij
  uses it — it survives version skew between a new UI and an older running daemon, which is the
  whole point of the inversion.
- Whether to be drop-in compatible with Zellij's KDL layouts and keybindings. Attractive for
  migration, a large surface to commit to.
- Config format. KDL is nice to read; Go's ecosystem support is thinner than for TOML.
