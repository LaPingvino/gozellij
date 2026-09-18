# Can this replace gezellij yet?

The acceptance test for this project is not a story list: it is whether Joop can stop using
[gezellij](https://github.com/LaPingvino/gezellij) — a working Zellij fork, in daily use as a login
multiplexer on a VPS — and use this instead. That question decides what counts as a nice-to-have.

Everything below marked **verified** was run, on this machine, and the transcript is in the session
that added the line. Nothing is marked verified because it looks like it should work.

## What already works

- **An interactive shell.** `gozellij add shell -restart always -start -- bash -i`, then
  `gozellij attach shell`. **Verified:** `tty` returns a real pts, `$-` is `himBHs` (interactive,
  job control on), `sleep 5 &` and `jobs` behave, bracketed paste is negotiated.
- **Terminal size, including resize.** **Verified:** attaching from a 132x43 terminal gives the
  shell 132x43; resizing to 160x50 and sending SIGWINCH gets `tput cols` to 160 on the far end.
- **Full-screen programs.** **Verified:** escape sequences pass through untouched (`\033[2J\033[H`,
  `top` output). They have to: the attach is a byte pipe and your terminal does the emulating.
- **Detach without stopping.** `Ctrl-] d`, and the service keeps running. **Verified.**
- **Tabs: several services in one terminal.** `Ctrl-] n` and `Ctrl-] p` move to the next and
  previous service without leaving the terminal, and `Ctrl-] l` lists them and picks one by number.
  **Verified** on a real pty: switching repaints, wraps round, keystrokes typed after two switches
  arrived at the right service and only at that one, and the list marks where you are, selects by
  number and leaves you where you were on any other key.
- **Survives a daemon upgrade.** `gozellij upgrade` replaces the binary with service pids
  unchanged, and an attached client reattaches by itself with a notice. **Verified**, twice, by
  comparing pids.
- **Output that outlives the daemon.** `gozellij logs <name>` reads
  `$XDG_STATE_HOME/gozellij/logs/<name>.log`, appended as the service runs and rotated at 16 MiB
  with one generation kept; `gozellij logs -f` follows the live buffer. **Verified:** a service
  printed a unique string, the daemon was `kill -9`'d, a new daemon was started, and `gozellij
  logs` still had the string. Each daemon writes a line into the file when it opens it, so two
  runs of the same service do not read as one.
- **Typing `gozellij` puts you in a shell.** No arguments means the `shell` service: reattached if
  it is running, defined fresh from *your* terminal if it is not. **Verified** on a real pty with
  the daemon started under `env -i`: `TERM`, `LANG` and `$HOME` are the client's, `exit` returns you
  to your own prompt in 0.3s with status 0, and `GOZELLIJ=shell` is set in the environment.
- **Survives a reboot**, for services marked enabled — `Enabled` is the desired state on disk and
  `Load` starts them. **Verified** across a simulated restart, not yet across a real reboot.

One accidental advantage over gezellij: because the attach is a byte pipe rather than a grid, your
*own terminal's* scrollback, search and copy/paste all work normally. A multiplexer with its own
grid has to reimplement those and then be worse at them.

## What is missing, in the order it would bite

1. **Splitting the screen.** Two things *visible at once* still needs a terminal emulator, and
   that has not changed. What has changed is that the common case did not need one: gezellij's
   daily value is tabs, and tabs are switching which service this terminal shows — which a byte
   pipe does for free, because the escape sequences that drew the screen are in the bytes being
   replayed. `Ctrl-] n` does it today.

   Whether to build the grid at all is now a real question rather than an assumed yes:

   - **For:** side-by-side is genuinely different from switching — watching a log while you type
     in a shell is the case tabs cannot cover.
   - **Against:** it is the expensive 98.8% this project exists to avoid (see DESIGN.md), the Go
     VTE landscape is poor enough that the one Go multiplexer with real users ships two emulators
     behind one interface, and owning the grid *costs* the accidental advantage below — your own
     terminal's scrollback, search and copy/paste stop working and have to be reimplemented worse.
     Meanwhile gozellij already composes with tmux, precisely because the attach is a byte pipe.

   The recommendation is to leave it here until the lack of splitting actually bites in daily use,
   and if it does, to do Phase 2 (the oracle) first as DESIGN.md says. This is Joop's call, and it
   is no longer blocking the replacement.

2. ~~**Scrollback is 256 KiB of RAM and dies with the daemon.**~~ Done — see above. What it cost:
   for a shell you live in, the log file now holds everything you typed at it and everything it
   answered, including whatever you `cat`. gezellij never wrote that anywhere. 0600 on the file and
   0700 on the directory are necessary and not sufficient, so `add -log off` exists per service and
   `gozellijd -logs off` for the lot. The honest summary is that the daily-driver gap is closed and
   a new decision has been handed to the user, which is better than closing it quietly.
3. ~~**A shell to attach to by default.**~~ Done — see above. Three things it turned up, all of
   which would have bitten on day one:
   - an attach never ended when its service did, so `exit` left you in a stream that would never
     close. Fixed for every service, not just the shell;
   - the client then still hung, because its keystroke pump was blocked in `read()` on the
     *terminal* and closing the socket does not wake that. It never showed up in tests, whose stdin
     is not a tty and reaches EOF at once;
   - a service inherits the *daemon's* environment, which under the systemd unit has no `TERM` and
     no locale — so `less` and `vim` would have been broken in a shell that otherwise looked fine.
     The shell service now captures `TERM`, `COLORTERM`, `LANG` and `LC_*` from the terminal you
     typed in. They are frozen at creation, which is why a shell that is not running is defined
     again rather than merely restarted.

   Also worth knowing: a login shell runs your profile, and profiles start multiplexers. The first
   by-hand run dropped straight into gezellij. `GOZELLIJ` is now set in the shell's environment, the
   way tmux sets `$TMUX`, so an autostart can guard on it.
4. ~~**Nothing checks that the promise holds on this box.**~~ Done — `gozellij doctor` asks the
   host the questions `packaging/README.md` answers: socket path length, daemon reachable, the
   `gozellijd` on PATH versus the one running, state directory and its mode, `loginctl` linger,
   the systemd user unit, whether the running shell's `TERM` matches the terminal you are in, and
   any service whose output is not reaching disk. It exits non-zero only for things that are
   broken now, so a warning does not break a script that calls it. **Verified** against a scratch
   fabric in each state, and on this machine — where it correctly reports that **linger is off**,
   so nothing here currently survives a logout.
5. ~~**A service that daemonises cannot be stopped.**~~ Done, in two halves.

   `stop` signals the whole process group, and — where the daemon has a cgroup it may subdivide —
   sweeps the service's cgroup as well, which nothing can escape: cgroup membership is inherited by
   every descendant and cannot be left from inside. **Verified** on this machine, A/B, with a
   service whose child calls `setsid`: under `systemd-run --user --scope -p Delegate=yes` nothing
   survived `stop`; under a plain login shell exactly that child survived. `gozellij doctor` reports
   which of the two is in force, because a tree-kill that is silently only a process-group kill is
   the worst of both.

   That is why `Delegate=yes` is in the systemd unit. Started from a login shell the daemon lands in
   a *session scope*, which is root-owned and cannot be subdivided at all, so the cgroup path does
   not exist there and the process group is the fallback.

   Going after the orphan turned up something worse. A grandchild holding the pty slave open wedged
   the reader that `reap` waits for — and closing an `os.File` does not interrupt a read already in
   flight on a descriptor the runtime cannot poll — so `Process.Wait` never returned and
   **`gozellij stop` hung for ever**, the client's thirty second timeout being the only thing that
   ended it. **Verified** by goroutine dump, and fixed by bounding that wait.

6. **No session concept.** gezellij has named sessions you attach to; here there are services, and
   a shell is just a service. That may be the better model — but it is a different model, and
   swapping daily drivers means the muscle memory has to land somewhere.

## Known gaps, written down rather than papered over

- Nothing outstanding here at the moment. The gaps that used to be listed — a swallowed keystroke
  after a reattach, `rm` leaving a service's transcript on disk, and two terminals racing over the
  shell definition — are fixed rather than documented.

## What is deliberately not on this list

- **Per-service ULA addresses.** The package exists (`internal/ula`) and is not wired in. An
  address you must paste into two config files is worse than a port; Docker's networking win is the
  *name*. Wiring waits for resolution.
- **cgroup freeze/thaw.** No story needs `docker pause`. The same cgroup plumbing serves tree-kill,
  which does.
- **The Phase 2 oracle**, until Phase 3 is actually happening. It earns its place only as the
  safety net under an emulator.
