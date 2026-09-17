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

## Restarting the daemon (solved)

The first draft of this document promised more than the architecture delivered, and running it
proved the point:

```
pid before daemon restart: 978659
pid after  daemon restart: 978766     <- a different process; the service had been killed and restarted
```

The daemon holds every pty master. When it exits those close, the slaves get SIGHUP, the children
die, and the next daemon reads `enabled: true` and starts *new* ones — which looks like it worked
unless you compare pids.

**Now fixed, by exec-in-place.** On `SIGUSR1` the daemon clears `FD_CLOEXEC` on every live pty
master, writes a manifest naming `{service, pid, fd, started}`, and calls `syscall.Exec` on the
binary now at its own path. The exec keeps the pid, so:

- the children are still *its* children and can still be waited on, and
- the pty masters stay open — the process on the other end never sees a hangup, because the master
  was never closed.

The successor finds the manifest in its environment, adopts each pty and pid, and supervises them
without spawning anything. Measured across a real binary swap:

```
before: service=1018245 daemon=1018230 version=dev
after:  service=1018245 daemon=1018230 version=upgraded-2.0
uptime: 29s   starts: 1               <- never restarted
```

What this does *not* cover, stated plainly so nobody discovers it the hard way:

- **A daemon that crashes** takes its services with it. Exec-in-place is a planned upgrade, not a
  safety net. Surviving a crash needs the descriptors held somewhere else — `SCM_RIGHTS` to a
  keeper process, or systemd's `FDSTORE`.
- **Only running processes are handed over.** A service that is backing off or stopped is started
  from its definition by the successor, like any other.
- **A manifest from a version the successor does not speak is refused**, and the services are
  started fresh. That loses the processes, but it loses them loudly, which beats adopting
  descriptors with the wrong field meanings and wiring services to the wrong ptys.
- `os.Executable()` is not used directly for the exec target: on Linux it reads `/proc/self/exe`,
  which after a package upgrade points at the old, deleted inode. Exec'ing that would faithfully
  reinstall the version being replaced.

## What `stop` does not stop (measured)

`Process.Stop` signals one pid: the session leader. The obvious conclusion - that a service's
children survive - turns out to be wrong for the common case, and it is worth writing down which
case is which, because the difference decides how urgent this is.

Measured 2026-09-17, on this machine:

- **An ordinary child dies.** The child is a session leader with the pty as its controlling
  terminal, so closing the pty master sends `SIGHUP` to the session and the whole tree goes. A
  shell that backgrounds another shell loses both. So `npm start` leaving a stray `node` - the
  example this was first reported with - does not happen, because node stays in the session.
- **A child that escapes the session survives.** `setsid`, a daemonising process, anything that
  detaches from the controlling terminal. Verified with a service that reports its own child's
  pid: after `gozellij stop`, the parent was gone and `sleep 600` was still running.

So the gap is real and narrower than "stop does not kill the tree". The fix is a process-group or
cgroup kill rather than a single `kill(pid)`, and the systemd unit already sets `Delegate=yes` for
the cgroup route. Until then, a service that deliberately daemonises is a service gozellij can
start and cannot fully stop, which is worth knowing before trusting it with one.

## Client transports, and the mosh idea

The inversion above says the UI is replaceable. That invites a question with a very attractive
answer: what if some of the UIs are ones we do not have to write?

**mosh** is the interesting case. Its Terminal app ships on ChromeOS with mosh support, and mosh
clients exist on essentially everything. If the fabric could speak mosh's protocol, then attaching
from a Chromebook, or from a phone, would need no gozellij client at all — and you would inherit
mosh's two properties that matter here for free:

- **Roaming.** UDP with a session key, so the connection survives your IP changing, your laptop
  sleeping, and your network moving. That is exactly the failure this project cares about; the
  reason the fork grew client-parking and ghost tabs was an ssh client vanishing when a laptop
  rebooted.
- **Predictive local echo**, which is what makes mosh feel better than ssh on a bad link.

The structural point is more important than the convenience, though: **mosh synchronises screen
state, not bytes.** `mosh-server` contains a terminal emulator so it can compute frame diffs. So
"speak mosh" is only possible once the grid lives in the fabric — it is a Phase 3+ idea, not a
shortcut around Phase 1. It does, however, argue for a particular shape:

- The fabric should eventually be able to hand out **screen state**, not only raw PTY bytes.
- A raw-PTY attach (Phase 1) and a state-synchronising attach (later) are then two transports over
  the same fabric, not two different programs.

Unverified and worth checking before committing to this: whether a usable Go implementation of
mosh's State Synchronization Protocol exists, or whether it would mean implementing SSP and its
AES-OCB framing ourselves, plus the out-of-band key exchange mosh does over ssh. That is real
cryptographic protocol work and the sort of thing to reuse rather than write.

Lower-effort relatives of the same idea, for comparison: exposing the fabric over plain ssh with a
forced command (works everywhere, no roaming), or a small web UI over the socket (works on
ChromeOS trivially, no terminal fidelity).

## The wire format (decided)

Length-prefixed frames on a unix socket:

```
+--------+------+------------------+
| uint32 | kind |     payload      |
| length | byte |  length-1 bytes  |
+--------+------+------------------+
```

`kind` is request, response, data or event. Control traffic (request/response/event) is **JSON**;
`data` frames carry **raw bytes**, because pty output is high volume and base64 inside JSON would
be both slower and larger. One framing, two payload styles.

This started as "leaning protobuf", for the reason Zellij uses it: a client and a daemon can be
different versions and the protocol has to survive that. JSON has the same property — unknown
fields are ignored, new optional fields are free, and there is a test that an older peer can read a
newer peer's message — without a codegen step, a committed-generated-file rule, or a protoc in the
build. For "add a service, list services, tell me the status", the encoding cost is irrelevant, and
being able to read a socket dump with your eyes is worth a lot while a protocol is young.

If the stream path ever needs it, `data` frames are already raw bytes and nothing about the framing
would have to change.

Decisions inside the protocol that are load-bearing rather than incidental:

- **Every request gets exactly one response, including the failures.** A daemon that answers only
  when things went well leaves a client on a timeout to discover a typo.
- **An unknown frame kind is an error, not a skip.** Quietly discarding frames is how a version
  mismatch becomes "it just does nothing sometimes".
- **A frame size limit**, so a peer that is buggy or hostile cannot announce four gigabytes and
  have us allocate it.
- **`ListReply.Problems` travels with the list**, so a service file the daemon could not load
  neither hides the nine working ones nor is hidden by them.
- **An explicit `lagged` event**, so a client whose stream fell behind is told its view is
  incomplete and can re-attach, rather than displaying something subtly wrong.

## Not yet decided

- Which VTE backend leads in Phase 3, and whether cgo is acceptable for release builds.
- Whether to be drop-in compatible with Zellij's KDL layouts and keybindings. Attractive for
  migration, a large surface to commit to.
- Config format. KDL is nice to read; Go's ecosystem support is thinner than for TOML.
