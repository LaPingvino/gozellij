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
- **Detach without stopping.** `Ctrl-]`, and the service keeps running. **Verified.**
- **Survives a daemon upgrade.** `gozellij upgrade` replaces the binary with service pids
  unchanged, and an attached client reattaches by itself with a notice. **Verified**, twice, by
  comparing pids.
- **Survives a reboot**, for services marked enabled — `Enabled` is the desired state on disk and
  `Load` starts them. **Verified** across a simulated restart, not yet across a real reboot.

One accidental advantage over gezellij: because the attach is a byte pipe rather than a grid, your
*own terminal's* scrollback, search and copy/paste all work normally. A multiplexer with its own
grid has to reimplement those and then be worse at them.

## What is missing, in the order it would bite

1. **More than one thing at a time in one terminal.** gezellij has tabs. Here, each service is its
   own attach, so today the answer is a second terminal, or running gozellij inside tmux (which
   works, because the attach is a byte pipe). This is the story that reopens Phase 3 — a
   product-owner pass argued panes should be cut because no user story needed them, and it was
   right about its four personas and wrong about the actual user.
2. **Scrollback is 256 KiB of RAM and dies with the daemon.** For a shell you live in, that is the
   difference between "what did that build print" and "I have no idea". Logs need to reach disk,
   and `logs -f` needs to exist. This is the top of the work queue.
3. **A shell to attach to by default.** Replacing a login multiplexer means `gozellij` with no
   arguments should land you somewhere sensible, not print usage.
4. **Nothing checks that the promise holds on this box.** "Through logouts" depends on
   `loginctl enable-linger`, documented in `packaging/README.md` and enforced by nothing. A
   `gozellij doctor` closes that and several like it.
5. **A service that daemonises cannot be fully stopped.** Ordinary children die with the pty's
   SIGHUP; one that calls `setsid` survives. **Verified.** Needs a process-group or cgroup kill.
6. **No session concept.** gezellij has named sessions you attach to; here there are services, and
   a shell is just a service. That may be the better model — but it is a different model, and
   swapping daily drivers means the muscle memory has to land somewhere.

## What is deliberately not on this list

- **Per-service ULA addresses.** The package exists (`internal/ula`) and is not wired in. An
  address you must paste into two config files is worse than a port; Docker's networking win is the
  *name*. Wiring waits for resolution.
- **cgroup freeze/thaw.** No story needs `docker pause`. The same cgroup plumbing serves tree-kill,
  which does.
- **The Phase 2 oracle**, until Phase 3 is actually happening. It earns its place only as the
  safety net under an emulator.
