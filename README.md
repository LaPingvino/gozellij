# gozellij

**Your processes keep running.** Through logouts, through the UI crashing, through upgrading the
daemon underneath them, through you rebooting the laptop you were sshed in from.

```
$ gozellij upgrade
daemon upgraded: v1 -> v2
1 process(es) kept their pid     # same process; it never noticed
```

> A daemon that *crashes* no longer takes its services with it, when it is run from the systemd
> user unit in [packaging/](packaging/): each service's terminal is held by systemd, and the daemon
> that comes back picks the processes up with their pids unchanged. Started by hand from a shell
> there is nowhere to keep the descriptors, and a crash is still a crash. `scripts/fdstore.sh`
> checks it; [DESIGN.md](DESIGN.md) says how it works.

A host-native process fabric with a terminal UI on top — aiming to be lighter than Docker, and
easier to live with, for the job most people actually use Docker for on one server: run a handful
of long-lived things, keep them alive, look at them, restart them, give them addresses.

**On "safer":** today that means no root daemon, a 0600 socket in your own runtime directory, and
service definitions that are 0600 because they carry environment variables. It does **not** yet
mean isolation. Services run as you, on your filesystem, with no resource limits, and they inherit
the daemon's environment — so if you start `gozellijd` from a shell, every service sees your
`SSH_AUTH_SOCK` and anything else you had exported. That is a smaller claim than Docker's and it
is the honest one; the gap is [tracked in the stories](docs/USER_STORIES.md).

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

$ gozellij logs clock -n 40      # what it printed, from disk; survives the daemon dying
$ gozellij logs -f clock         # follow it live; Ctrl-C stops watching, not the service
$ gozellij attach clock          # watch it live; Ctrl-] d detaches, it keeps running
                                 # Ctrl-] n / p switch services; Ctrl-] l lists them and picks one
                                 # the first attach on a machine spells the keys out once, and
                                 # `Ctrl-] ?` is on the status line from then on
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

### Where the output goes

Everything a service prints is appended to `$XDG_STATE_HOME/gozellij/logs/<name>.log`, rotated at
16 MiB with one generation kept. `gozellij logs` reads that file, so it answers "what did that
build print last night" even though the daemon has been replaced twice since; `gozellij logs -f`
follows the daemon's live buffer instead, which is a few hundred KiB of RAM and is the thing that
does *not* survive.

The consequence is worth stating plainly rather than discovering: for an interactive shell, that
file holds everything you typed at it and everything it answered — including whatever you `cat`.
`gozellij rm` deletes it along with the service and says which file went; `gozellij rm -keep-logs`
keeps it.
The files and their directory are 0600/0700, and `gozellij add <name> -log off -- ...` turns the
file off for one service, which then keeps the in-memory ring and nothing else. `gozellijd -logs
off` turns it off for everything.

By default, several services in one terminal are *tabs* — `Ctrl-] n` switches — which needs no
terminal emulator, because the attach is a byte pipe and replaying the bytes repaints the screen.
Two things visible at once needs a grid, which is what `-render` is for; see [Splitting the
screen](#splitting-the-screen).

### Commands

| | |
|---|---|
| `gozellij add <name> [flags] -- <cmd>` | define a service (`-restart no\|on-failure\|always`, `-start`, `-dir`, `-env`, `-log on\|off`) |
| `gozellij` | land in your shell — reattached if it is running, fresh if not |
| `gozellij ls` | what exists, what it is doing, who is watching it and how much log it has |
| `gozellij status <name>...` | one service in detail, including why it will not start; several if you name several |
| `gozellij start\|stop\|restart <name>...` | change the state of one or several; `stop` stops what the service started too, and means it stays stopped across a reboot |
| `gozellij logs <name> [-n bytes]` | what it printed, read from disk, so it outlives the daemon |
| `gozellij logs -f <name>...` | follow the live output; several services interleave with a name on every line, and `Ctrl-C` stops watching, not the service |
| `gozellij attach <name> [-render]` | connect your terminal; `Ctrl-] d` detaches without stopping anything, `Ctrl-] n`/`p` switch services, `Ctrl-] l` picks one from a list, `Ctrl-] ?` lists the keys |
| `gozellij upgrade` | replace the daemon binary, keeping every process |
| `gozellij doctor` | check the promises that depend on the host, and say what to type |
| `gozellij attach -r <name>` | watch without touching: your keystrokes and your window size do not reach the service, and the daemon enforces it rather than the client |
| `gozellij login-setup` | say how to make gozellij what your login shell starts; `-install` does it, `-undo -install` puts your files back |
| `gozellij takeover` | what another multiplexer is holding, what is running in each of its terminals, and the commands to recreate them here |
| `gozellij rm <name>... [-keep-logs]` | stop them, forget them, delete their logs |

### Splitting the screen

By default an attach is a byte pipe: the service's output goes straight to your terminal, which
does the emulating. That cannot show two services at once - they would draw over each other - so
there is a second mode where gozellij interprets the output itself and paints the screen:

```sh
gozellij attach -render web       # or: export GOZELLIJ_RENDER=1
```

Then `Ctrl-] |` opens the next service beside the current one and `Ctrl-] -` opens it underneath,
`Ctrl-] <` and `Ctrl-] >` change how much of the screen the focused pane gets,
`Ctrl-] o` moves the keyboard
between panes, and `Ctrl-] x` closes one. The status line says which pane your keystrokes are going
to. Because gozellij owns the screen rather than borrowing it, the status line no longer needs the
terminal's cursor-save slot or a scrolling region - the two things it could not do properly from a
byte pipe.

`gozellij attach -r <name>` watches without touching: your keystrokes and your window size do not
reach the service, so you can put eyes on something without your Ctrl-C ending it. The daemon
enforces it, not the client, and `gozellij ls` says which of the terminals attached can type:
`2+1r` is two that can and one that cannot.

`Ctrl-]` is gozellij's own key while attached, and it is configurable: put `prefix=C-b` in
`~/.config/gozellij/status` if that is what your fingers already know. `gozellij doctor` says which
key is in force, and every message gozellij prints about keys names the one you configured rather
than the default.

The arrangement is remembered. Attach to the same service again - after a detach, or after your
connection dropped - and the panes, their orientation and their sizes come back. It is kept in
`$XDG_STATE_HOME/gozellij/layouts/<service>.json`, which you can read and edit.

Painting the whole screen means your terminal's own scrollback stops filling up, so gozellij keeps
2000 lines per pane and gives you `Ctrl-] b` back, `Ctrl-] f` forward and `Ctrl-] g` to return to
the live screen.

It is not the default, and the reason is worth knowing: a byte pipe cannot corrupt a screen it
never interprets, while this interprets every escape sequence a service emits. One it gets wrong is
a screen `Ctrl-L` will not fix. What it understands is pinned by a corpus of screens recorded from
a real terminal (`make conform`), and `make acceptance` checks that a real `vim` drawing wide
characters looks identical through gozellij and through tmux. `-no-render` gets you the byte pipe
back for one command if a screen ever looks wrong.

### A status line

```sh
$ gozellij stats
[shell 1/3] 2/3 up    up 6d4h 0.42 0.31 0.28 8cpu 3.1G/7.7G 18G free 2026-09-18 15:34
```

The same line is drawn at the bottom of an attach. The widget names and the configuration follow
byobu's, because that is what the fingers of anyone who wants this already know: a leading `#`
switches one off, and the defaults are the set byobu enables for tmux, plus the two things only
this program knows — which service you are looking at, and how many are up.

```sh
gozellij stats -example > ~/.config/gozellij/status   # the defaults, written out
gozellij stats -list                                  # every widget name
```

`where=bottom` (the default) reserves the last row with a scrolling region. That needs no terminal
emulator, which is why it is possible here at all — and it is also why it is not perfect: a
full-screen program such as `vim` or `top` sets its own region and draws over the line, which comes
back by itself when the program exits. `where=title` puts the line in the terminal's title instead,
where nothing can draw over it; `where=off` draws nothing.

## Does it still do what it says?

```sh
./scripts/acceptance.sh
```

Twenty-one checks, on a real pty, against a throwaway daemon: an interactive shell, the environment
it gets, detaching without stopping, switching services, an upgrade that keeps every pid *with a
client attached across it*, logs outliving the daemon, enabled services returning, and a stop that
takes a service's children with it. Exits non-zero if any promise has stopped holding.

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
make build        # bin/gozellij and bin/gozellijd, version stamped from git
make test         # go test ./...
make race         # go test -race ./...
make acceptance   # scripts/acceptance.sh: every promise, on this machine, now
make fuzz         # generate terminal streams nobody wrote and diff them against tmux
make check        # fmt, vet, test, race, acceptance - what to run before pushing
```

All of them are expected to pass. `make acceptance` is the slow one and the one that matters
most: it drives real ptys, a real daemon under `env -i` and a real tmux screen, and it re-checks
each promise in [docs/REPLACING_GEZELLIJ.md](docs/REPLACING_GEZELLIJ.md) rather than taking the
document's word for it. Several of the last bugs found were invisible to the unit tests, because
they were about what the terminal ends up showing.

See [packaging/](packaging/) for the systemd user unit, and `packaging/arch/PKGBUILD` on Arch -
that one installs the unit too, and `replaces` gezellij, so installing it *is* the migration.

## Name

Zellij is Moroccan tilework. `gezellij` added Dutch *gezellig* — cosy, convivial, the pleasure of
being somewhere comfortable. `gozellij` keeps that and adds the gopher.

## Licence

MIT.
