# Can this replace gezellij yet?

The acceptance test for this project is not a story list: it is whether Joop can stop using
[gezellij](https://github.com/LaPingvino/gezellij) — a working Zellij fork, in daily use as a login
multiplexer on a VPS — and use this instead. That question decides what counts as a nice-to-have.

Everything below marked **verified** was run, on this machine, and the transcript is in the session
that added the line. Nothing is marked verified because it looks like it should work.

That was still not enough. A line verified once is a claim about the past, and this program has been
rewritten underneath several of them — "an attached client reattaches by itself" was an argument
rather than a measurement from the moment the attach client was restructured. So the promises are
now re-checkable on demand:

```sh
make acceptance     # or: ./scripts/acceptance.sh
```

It builds the binaries, runs its own daemon in a throwaway directory under `env -i`, drives a real
pty, and reports PASS or FAIL for each promise below, exiting non-zero if any of them has stopped
being true.

It drives a real terminal through tmux for the screen checks, because the rest of it uses `script`,
whose pty has no size when its own stdin is a pipe — and a terminal of unknown size is one the
status line refuses to draw on. Three screen bugs shipped straight through that blind spot.

Three of its checks originally passed for the wrong reason — including the one promise that had
never been measured at all. Each check is now shown to *fail* when the behaviour it names is
deliberately broken, which is the only thing that makes a passing check worth anything. What the
script does **not** cover is listed here so it is not mistaken for a complete account: resize and
SIGWINCH, escape-sequence passthrough for full-screen programs, `Ctrl-] p` and `Ctrl-] l`, and the
shell's `LANG`/`HOME`. Those remain verified-by-hand only.

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
- **A byobu-shaped status line.** `gozellij stats` prints it; an attach draws it at the bottom by
  default. byobu's widget names, byobu's `#`-to-disable, byobu's tmux defaults, plus which service
  you are looking at and how many are up. **Verified** against a pty opened at a known size: the
  scrolling region is set, the line lands on the last row, and the region is released on detach.

  The row is reserved by telling the service its screen is one row shorter, so `vim` and `less`
  never address the status row at all — an earlier version only set a scrolling region and left
  the service believing it had the whole screen, and then fought it every tick: `less` lost its
  prompt, and vim's `:` disappeared from under the user's fingers two seconds after they typed it.
  **Verified** in tmux at 80x24 by reading the pane back: `less` keeps its prompt row, typing in
  the shell scrolls normally, and detaching leaves the region released and the cursor where it
  was. `where=title` avoids the question entirely because nothing can draw over a title bar.

  One case is verified by hand rather than by the script, because two attempts to reproduce it
  there did not and a check that passes without the fix is worse than none. Attaching used to
  overwrite the line you typed the command on — the reserved row was taken by putting the cursor
  on the row above it, which on a terminal you have been using is the last line of your content.
  To repeat it:

  ```sh
  tmux -L probe new-session -d -x 60 -y 12 'sh -c "PS1=\"o$ \"; export PS1; exec /bin/sh -i"'
  tmux -L probe send-keys 'seq 20' Enter
  tmux -L probe send-keys ": LINEHEAD-42; gozellij shell" Enter
  sleep 3; tmux -L probe capture-pane -p -S -60 | grep -c LINEHEAD-42   # 1 = intact, 0 = eaten
  ```

  Before the fix that printed 0 and the screen showed `[joop@host ~]$ p/gzbin/gozellij shell`
  where the typed line had been; after it, 1.

  Two limits remain, and neither can be fixed from outside a grid. There is a single cursor-save
  slot in a VT100, and the status line has to borrow it to draw on a row the cursor is not on: a
  program using `DECSC`/`DECRC` across a span of its own can have its saved position overwritten
  by a tick landing in the middle — **verified**, deterministically, with a three-second span
  against a two-second tick. And the scrolling region is equally single: the line re-asserts its
  own on every repaint, because a full-screen program that exits resets it, so a program that
  *holds* a region of its own — anything keeping a progress row at the bottom — has it replaced
  within a tick. **Verified** by reading the pane back: an application region of rows 1–10 became
  1–23 and its output escaped into the rows below. A multiplexer that owns the grid keeps its own copy of the screen and
  never touches the terminal's cursor state; this one is a byte pipe, and that is the price. It is
  the clearest concrete argument this project has for building the emulator — and DESIGN.md is
  explicit that the oracle comes first when that day arrives. Doing it properly at the bottom
  is what owning a grid buys, and is the clearest argument this project has yet produced for the
  terminal emulator DESIGN.md defers. `gozellij doctor` says which placement is configured, and
  names anything in the config it could not understand, so the first time you see it is not the
  first time you hear about it.
- **Survives a daemon upgrade.** `gozellij upgrade` replaces the binary with service pids
  unchanged, and an attached client reattaches by itself with a notice. **Verified**, twice, by
  comparing pids.
- **Output that outlives the daemon.** `gozellij logs <name>` reads
  `$XDG_STATE_HOME/gozellij/logs/<name>.log`, appended as the service runs and rotated at 16 MiB
  with one generation kept; `gozellij logs -f` follows the live buffer, and takes several services
  at once with a name on every line. **Verified:** a service printed a unique string, the daemon
  was `kill -9`'d, a new daemon was started, and `gozellij logs` still had the string. Each daemon
  writes a line into the file when it opens it, so two runs of the same service do not read as one.
  **Verified** separately for following two services at once: each line carried its own service's
  name and no line carried the other's, one service alone stayed byte-identical and unprefixed, and
  several names without `-f` were refused rather than run together. All three were shown to fail
  when the behaviour was deliberately broken.
- **A rendered attach that owns the screen.** `GOZELLIJ_RENDER=1 gozellij attach <name>` keeps a
  grid of its own, feeds the service's output through the emulator in `internal/vt/grid`, and
  paints the result. The status line is then a row the service was never given, rather than a row
  painted over one it was - so nothing saves and restores the terminal's cursor and nothing sets a
  scrolling region. Those were the two defects that could not be fixed from a byte pipe.
  **Verified** on a real tmux screen: the service's output is drawn, the status line is on the last
  row, and the scrolling region is `0-11` on a twelve-row terminal - the whole screen - where the
  default attach sets `0-10`. All three checks were shown to fail when the behaviour was removed;
  switching the mode off reports the region as `0-10`, which is the default path's signature.
  Off by default: it interprets every escape sequence the service emits, so a sequence the emulator
  gets wrong is a screen `Ctrl-L` cannot fix, whereas a byte pipe's failures are the terminal's own.

- **A split screen, and an emulator checked against another one.** In a rendered attach,
  `Ctrl-] |` opens the next service as a second pane, `Ctrl-] o` moves the keyboard and `Ctrl-] x`
  closes a pane. Two services drawing on one terminal is the thing a byte pipe cannot do at all.
  **Verified** on a real tmux screen: both services' output side by side, the status line naming
  which pane has the keyboard, and the scrolling region still `0-11` with two panes up.
  The emulator itself is **verified** against a different one end to end: the same `vim`, the same
  file of wide characters and accents, the same keystroke, one through a rendered gozellij attach
  and one straight into tmux at the size gozellij gives the service - captured with escape
  sequences on both sides, so colours are compared and not just text. The screens match exactly.
  Breaking cursor positioning or bright colours in the emulator makes it fail; an earlier version
  of this check used a pager over a file of numbers and caught neither.

- **A split screen survives the daemon being replaced.** `gozellij upgrade` keeps every process
  running, which is worth less than it sounds if the screen showing them has to be rebuilt
  afterwards. Each pane reconnects where it stands, keeping its grid - no replay, so nothing
  redraws over what is already there. **Verified:** a split screen with two chattering services,
  upgraded mid-session, with both panes still advancing afterwards. The failure this replaced was
  silent: both panes froze at the instant of the upgrade and stayed frozen, which looks exactly
  like two idle shells.

- **Scrollback in a rendered attach.** Painting the whole screen stops the user's own terminal
  history from filling up, so the lines that scrolled past live in the emulator's grid and nowhere
  the terminal can show them. `Ctrl-] b` goes back half a screen, `Ctrl-] f` forward, `Ctrl-] g`
  returns to live. A view rather than a mode: the service keeps running and its grid keeps being
  written to while you read. **Verified** on a real screen - scrolling back changes what is drawn
  and `Ctrl-] g` restores exactly the live screen - with three sabotages, each failing one of the
  two checks.

- **One command, several services.** `status`, `start`, `stop`, `restart` and `rm` each take as many
  names as you give them, and keep going past a name that fails rather than abandoning the rest —
  the exit status carries a count. `attach` still takes exactly one, because a terminal does.
  **Verified:** three services stopped by one command, a typo in the middle of three reported while
  the other two were still started and the command exited non-zero, three removed with their logs.
  Each of those was shown to fail when the behaviour was broken.
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

   A later pass found the grace was not reliable — two Stops run concurrently and the second
   swept the cgroup at once, so a child given half a second got sixteen milliseconds. Fixed and
   now verified twelve runs out of twelve; the earlier "verified" here rested on one lucky run,
   which is worth recording because it is the failure this document exists to prevent.

   Going after the orphan turned up something worse. A grandchild holding the pty slave open wedged
   the reader that `reap` waits for — and closing an `os.File` does not interrupt a read already in
   flight on a descriptor the runtime cannot poll — so `Process.Wait` never returned and
   **`gozellij stop` hung for ever**, the client's thirty second timeout being the only thing that
   ended it. **Verified** by goroutine dump, and fixed by bounding that wait.

6. **No session concept — mostly answered, and worth deciding on.** gezellij has named sessions you
   attach to; here a shell is just a service, and the pieces that make that usable now exist:
   `gozellij` lands you in one, `gozellij shell -name work` gives you another, `Ctrl-] l` picks
   between them, and `gozellij ls` shows which one your other terminal is sitting in. What is left
   is a naming question rather than a missing feature: whether "session" should exist as a word at
   all, or whether a shell being an ordinary service — supervised, logged, restartable, stoppable —
   is simply the better model and the muscle memory should move. That is Joop's call to make by
   using it.

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
