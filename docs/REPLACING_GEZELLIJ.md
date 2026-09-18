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

1. **More than one thing at a time in one terminal.** gezellij has tabs. Here, each service is its
   own attach, so today the answer is a second terminal, or running gozellij inside tmux (which
   works, because the attach is a byte pipe). This is the story that reopens Phase 3 — a
   product-owner pass argued panes should be cut because no user story needed them, and it was
   right about its four personas and wrong about the actual user.
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
