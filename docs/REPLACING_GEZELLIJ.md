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
- **Watching without being able to touch.** `gozellij attach -r <name>` puts eyes on a service's
  live output with your keystrokes and your window size going nowhere near it - user story B3. The
  *daemon* drops that connection's input and its resizes rather than the client promising not to
  send them, because "my Ctrl-C does not reach it" is a claim about the far end. Resizes too: two
  people attached, one watching, and the watcher's terminal must not reshape the screen of the one
  working. **Verified** on a real screen: the output arrives, `Ctrl-C` produces nothing in the
  service's own log where the same service traps and prints on `INT`, the first swallowed keystroke
  says so on screen, and `Ctrl-] d` still leaves. Removing the *client's* half leaves all of it
  passing, which is what says the daemon is the thing enforcing it; removing both makes the `Ctrl-C`
  check fail.

  `gozellij ls` says which of the terminals attached can type - `2+1r` is two that can and one that
  cannot - and `gozellij status` spells it out. `logs -f` counts as read-only, because a follower
  has no way to send anything. **Verified** with one of each attached at once. What it does not say
  is *whose* they are: the daemon knows a connection, not a person.

- **Survives a daemon crash, not only an upgrade.** `kill -9` the daemon and the services keep
  running with their pids unchanged: each service's terminal is held by systemd's file-descriptor
  store while the daemon is gone, and the one that comes back takes it out and adopts the process
  on the other end. This was the last thing in this program that could lose your work without
  anybody doing anything wrong. **Verified** against a real transient user unit by
  `scripts/fdstore.sh`: the terminal is in the store beside the socket, the pid is the same
  afterwards, the newest line of output carries the pid of the process that was there before the
  crash, and stop still works on a process this daemon is not the parent of. Only under the systemd
  unit - started by hand from a shell there is nowhere to keep the descriptors, and the daemon says
  so at startup.

  **And then it happened for real**, which is worth more than the check. On 2026-09-23 the daemon
  died on its own - `NRestarts=1`, no test harness, nobody killing anything on purpose - and the
  journal says:

  ```
  gozellijd.service: Found left-over process 633264 (bash) in control group while starting unit.
  recovering services from a daemon that did not shut down cleanly  count=2
  ```

  Both shells kept their pids, including the one the person was working in. It also exposed a
  defect no test had: the recovered services reported their uptime from the adoption rather than
  from when they started, so a shell up for an hour and a half said twenty minutes. Fixed by
  reading the kernel's own answer, and `scripts/fdstore.sh` now compares against `ps`.

  Including the person who was attached when it happened, which is the scenario the rest of it is
  for: an ssh session on a shell, the daemon killed under it, and the client noticing, waiting for
  the replacement and attaching again by itself - to the same shell, which never stopped. **Verified**
  by setting a variable in that shell before the crash and reading it back afterwards, because a
  client that reattaches to a *fresh* shell looks identical from the outside.

- **You are told how to get out.** The first thing a multiplexer owes you is the key that gives
  your terminal back, and for a long time nothing said it: the status line reported which service
  you were in and how much memory the machine had, and the only way to learn `Ctrl-]` was to read
  the README, which you cannot do from inside. The first attach on a machine now says the whole
  key table once, and `Ctrl-] ?` sits permanently at the left of the status line. The key itself is
  configurable - `prefix=C-b` for anybody whose fingers know tmux - and every message that names it
  is built from the key in force rather than the compiled-in default. `gozellij doctor` says which
  one is active and where it came from. **Verified** on a real screen for both the greeting and its
  absence on the second attach, and by rebinding to Ctrl-B and checking that Ctrl-] then does
  nothing.

- **You can see that there is more than one of anything.** The line used to say `[shell 2/3]`,
  which tells you others exist and nothing about what they are, so switching meant opening the
  picker to find out - every time. It reads `Ctrl-] ? logs shell [web] 3/3 up` now. Asked for from
  use, which is where it should have come from.

- **The byte pipe can talk at all.** Until recently every message in the default mode - a service
  exiting, a keystroke swallowed by a read-only attach, the reattach notice after an upgrade - went
  to standard error, where the service's next repaint wrote over it. Only the rendered attach had a
  voice. The byte-pipe status row carries messages now, and falls back to standard error when the
  terminal's size is unknown and the row cannot be drawn.

- **What is trapped in the other multiplexer, and how to get it back.** `gozellij takeover` reads
  `/proc` and reports which terminals another multiplexer is holding, what is running in each and
  in which directory, then prints the `gozellij add` lines that put you back in those directories
  and the kill that ends the old session. It marks the session you are reading it in, because the
  first version cheerfully told you to kill that one. It is **not** a handover and says so: the pty
  master lives inside that process and the kernel will not copy it out without that process's help
  or ptrace permission over it, neither of which a program arriving afterwards has.

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

## What would have to be true for the rendered attach to be the default

It is off by default because it interprets every escape sequence a service emits, where the byte
pipe cannot corrupt a screen it never reads. Turning that around is a judgement about evidence, so
here is the evidence and what is still missing from it.

What is true now: thirty-odd screens recorded from a real tmux agree with this emulator cell by
cell, including styles and scrollback; the same screens agree when the bytes arrive one at a time;
a generator produces streams nobody wrote and the disagreements it finds are down to four named
tmux quirks; a real `vim` on a file of wide characters draws identically through gozellij and
through tmux, colours included; and everything that belongs to the terminal rather than the grid -
mouse reporting, bracketed paste, the title, the cursor shape, and the answers a program waits for -
is carried through.

How to give it the week it needs, which is the only thing left on this list:

```sh
gozellij login-setup -render -install
```

Every login then lands in the attach that owns the screen, with panes. `Ctrl-] r` repaints from the
grid and `gozellij attach -no-render <name>` gets the byte pipe back for one command, so neither
kind of failure needs a restart to escape.

What is missing:

- **Use.** None of the above is a week of somebody's actual work. The failures that matter now are
  the ones that need a person to notice, and five of the last ten bugs were exactly that: a message
  written where it could not be read, a menu drawn and then painted over, a mode silently dropped.

  This has started, in the *byte pipe* rather than the rendered attach: gozellij replaced gezellij
  as this machine's login multiplexer on 2026-09-23. The first hours of it produced, in order: a
  panic from `gozellij add` with no arguments, no way to discover any key at all, no indication
  that more than one service existed, two goroutines reading one connection after a pane swap, a
  finaliser closing recovered terminals, a shell that would not die on `stop`, and an uptime that
  restarted at every crash. Not one of those came from the test suite. The rendered attach has not
  had its week yet, and on this evidence it should expect a similar list.
- **A reason to think an unknown sequence is harmless.** Anything the emulator does not implement is
  dropped rather than passed on. For a byte pipe that question does not arise. This is now *visible*
  rather than silent - a pane that emits something unimplemented says so on the status line, once,
  with the way back to the byte pipe - which turns it from a mystery into a limitation.

  And the limitation has been measured rather than imagined. `internal/vt/grid/probe_test.go` runs
  real programs on a real pty and reports what each of them sends that this emulator does not
  implement. The first eleven were bash, vim, nvim, less, htop, top, man, git, python3, nano and
  mc. They sent five things between them, and all five are handled now - control strings read to
  their end instead of drawn on the screen, application cursor keys and the keypad passed to the
  terminal, autowrap, the title stack, insert mode, the colour queries answered from what the real
  terminal said when the attach started, and OSC 8 hyperlinks carried through the grid and drawn
  again - `man` emits thirty-odd of them on a page and every one used to be lost. **All eleven**
  now report that everything they sent is understood.

  Which meant the survey had stopped surveying, so twelve more were added, chosen to be unlike the
  first eleven rather than more of them: emacs, helix, w3m, weechat, whiptail, screen, fish, zsh,
  gdb, sqlite3, node, nethack. Eleven of those are silent too. What is left is named rather than
  unknown, and almost all of it comes from one program:

  - **OSC 133**, semantic prompt marks (fish). Deliberately not carried. They say where a prompt
    begins and ends on the screen they were sent for, so passing them out of a pane would describe
    rows that are not where the outer terminal thinks they are.
  - **CSI ?2031**, ask to be told when the colour scheme changes (fish). A promise this terminal
    cannot make.
  - **XTGETTCAP**, `DCS +q` (fish, nvim - vim does not send it). Unanswered here, as it is under
    tmux, which was measured: `scripts/queryprobe.sh` asks and prints what comes back. Its sibling
    DECRQSS *is* answered, with the same bytes tmux sends.
  - **Sixel and kitty graphics**, which nothing surveyed here sends but which are reported rather
    than dropped in silence, because an image that does not arrive leaves a hole with nothing to
    explain it.

  OSC 7, the working directory, was on that list until it was carried: a byte pipe hands it to your
  terminal and that is why a new tab opens where the last one was, so an attach that reads the
  stream had to pass it on or be the path that quietly takes the feature away.
- **The cost.** It was two to three times the byte pipe on a flood, and this entry said the reason
  was not the number of repaints. Counting them said otherwise: twenty thousand lines arrived as
  378 frames and drew 379 whole screens, which was 4.7 of the 5.9 seconds. The earlier experiment
  had batched events that were ready at the same instant and found nothing to batch, because the
  client painted between every pair of them - it measured its own premise. Two changes since: the
  scrollback is a ring rather than a slice with its front dropped per line (1.2-2.5s of emulation
  becomes 0.3-0.7s), and output paints at most twenty times a second (379 repaints become 11-22,
  and 4.7s of painting becomes 0.15-0.9s). The end-to-end figure is not restated here, because
  measuring it again spread from 0.5s to 4.7s on a machine where the byte pipe spread just as
  wide; what is quoted is what could be counted.

`Ctrl-] r` repaints from the grid and `-no-render` gets the byte pipe back for one command, so
neither failure needs a restart to escape - which is the least a mode should offer before it is
the one you get without asking.

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

- **The set that draws boxes draws boxes.** A program selects the DEC line-drawing set and sends
  `lqqqk`, which is the top of a frame rather than five letters; ncurses uses it whenever the
  terminal description says to. **Verified** on a real screen, because the corpus cannot see it:
  tmux's `capture-pane` reports the underlying letter whether or not the set was applied, so a
  recording of a terminal that draws boxes and one that ignores it entirely are identical.

- **A program that asks the terminal gets an answer.** A cursor-position query is a question a
  program waits for. A byte pipe gets this for free - the query reaches the real terminal and the
  reply comes back the same way - but a rendered attach *is* the terminal and answered nothing at
  all, which is a hang rather than a cosmetic difference. Cursor position, device status and both
  device-attribute forms are answered now, and the replies go back to that pane's service as input.
  **Verified** through the service's own log, where its pty echoes what it received.

- **The terminal is asked what colour it is.** `vim` asks with OSC 11 and picks a light or a dark
  scheme from the answer; a rendered attach is the terminal from the program's side and knew
  nothing, so `vim` guessed. The client asks the real terminal the same question when it starts and
  answers panes from what came back - and does not wait for it, because the link here is somebody's
  ssh and a window long enough for that is a pause on every attach. The reply is caught whenever it
  arrives, out of the user's own input, which is where a terminal's answers land. **Verified**
  against a pipe standing in for a terminal that replies: an answer never reaches the pane, typing
  on either side of one does, and a half-finished answer is handed over as typing rather than held,
  bounded by both a two-second window and a kilobyte. Verified separately on a real screen that the
  question is asked at all - tmux cannot answer it with no terminal of its own, which was measured
  before the check was written rather than assumed.

- **The cursor shape reaches the terminal.** A program that asks for a bar while editing and a
  block otherwise is doing something the user can see. A rendered attach kept it to itself, so the
  shape stayed whatever the last program had set. **Verified** by the bytes the client writes, both
  that the shape is passed on and that it is reset on the way out - a cursor left as a blinking bar
  after a detach outlives the program that asked for it.

- **The window title reaches the terminal.** The same gap as the modes below, found on the same
  list: a byte pipe hands `OSC 2` to the real terminal so a shell's title tracks what it is
  running, and a rendered attach swallowed it - the title froze at whatever it said when the attach
  started. The focused pane's title is applied, and a pane whose program has not set one is named
  after its service. **Verified** through tmux's own `#{pane_title}`.

- **Mouse and paste reach the terminal.** A byte pipe passes a service's mode changes through for
  free; a client that interprets the output has to hand them on, and this one did not - mouse
  reporting, its encoding, focus events and bracketed paste were all absorbed, so clicking did
  nothing and pasting into an editor misbehaved on a screen that otherwise looked perfect. The
  focused pane's modes are applied, withdrawn when the keyboard moves to a pane that did not ask
  for them, and released on the way out, because mouse reporting left switched on after a detach
  outlives the program that caused it. **Verified** by reading the bytes the client writes, since
  none of this is visible on the screen - which is why it was missing for as long as it was.

- **A split that stacks.** `Ctrl-] -` puts the new pane underneath instead of beside: three panes
  in columns on an eighty-column terminal give twenty-six each, which is not a pane but a margin.
  One orientation for the whole screen, not a tree of splits - a tree means resizing, moving panes
  between branches and layouts to save, which is a different piece of work. **Verified** on a
  forty-column screen: the first service's output in the top half, the second's below it, checked
  by which rows each lands in rather than by both appearing somewhere.

- **You come back to the arrangement you left.** A rendered attach used to rebuild its panes from
  nothing every time: you split your shell against your logs, your ssh dropped, you came back and
  had one pane with no word about where the other went. Which for a login multiplexer - the thing
  that is supposed to hold your session while you are away - is the failure it exists to prevent.
  The panes, the orientation and the weights are written to
  `$XDG_STATE_HOME/gozellij/layouts/<service>.json`, keyed by the name you typed, in JSON meant to
  be repaired with an editor. Written after every change rather than on the way out, because the
  usual way this client ends is a connection dropping and not somebody pressing `Ctrl-] d`.
  **Verified** by killing the client with `kill -9` so that nothing on the way out can run:
  the file is on disk with both services in it, attaching again puts both panes back, and the
  restored pane goes on producing output rather than being a picture of one.

  What it will not do: restore a layout that no longer names the service you asked for, since
  `gozellij attach shell` that gives you two panes with no shell in either has ignored the
  question; write a smaller arrangement back over a larger one, so a service stopped for an
  afternoon is not forgotten permanently; or restore more panes than the terminal has room for -
  splitting by hand cannot make a six-column pane without somebody watching it happen, and a
  restore onto a smaller terminal can. It says which panes it left out, and says so about a
  service in the layout that could not be opened.

- **Everything gozellij says goes through one sink.** Standard error while the byte pipe owns the
  terminal, the status line once something is painting over it. Five separate messages had been
  written to a screen that erased them: a service's exit, the picker's menu, the help key, the
  "that key does nothing" reply, and the errors around switching. A rendered attach also says when
  a pane has reconnected after an upgrade - it comes back working, but what the service printed
  meanwhile is not on that screen and never will be, and the byte-pipe path has always said so.
  **Verified** on a real screen, including that the reconnection notice appears.

- **The help key answers where you are looking.** `Ctrl-] ?` lists the keys and `Ctrl-] <anything
  else>` says that key does nothing. Both wrote to standard error, which a rendered attach covers
  within milliseconds - a help key that helps nobody, and a prefix key that eats a keystroke in
  silence, which this project's own comment calls indistinguishable from a dropped one. Both go to
  the status line now. **Verified** on a real screen.

- **The picker is visible.** `Ctrl-] l` prints the services and waits for a keystroke. In a
  rendered attach the repaint that keeps the status clock moving drew the last frame over it within
  two seconds, so the question was invisible and the answer still worked - a menu answered by
  guesswork. Painting is suspended while something else owns the screen. **Verified:** the menu is
  still on screen three seconds after being asked for, and choosing an entry switches to it.
  Removing the suspend makes the first of those fail with the pane's output where the menu should
  be.

- **Switching a service does not throw the layout away.** `Ctrl-] n` in a split changes what the
  focused pane is showing and leaves the other pane alone; with one pane it reattaches as before.
  **Verified:** after switching, both panes are still there and the switched one is still receiving
  output. That second check is the one that matters - removing the reader for the new connection
  leaves a pane that draws its replay and then never moves again, and the first version of the
  check missed it because it compared a whole row that the *other* pane was still updating.

- **Two shells side by side, each getting its own keystrokes.** The core of what a multiplexer
  does, and for several commits only the drawing of it was checked. **Verified** with two real
  interactive shells in a split: a command typed after the split answers in the right-hand pane, and
  after `Ctrl-] o` the next one answers in the left. Checked by which side of the seam each answer
  lands on, since both shells reply at once and their output shares a row. Sending every keystroke
  to the first pane makes it fail with both answers in the same column.

- **What gozellij says is readable.** In a rendered attach, standard error is covered by the next
  repaint within milliseconds, so a service exiting wrote "exited with code 3" to a screen that
  erased it before anyone could read it. Messages take the status line for six seconds - the whole
  line, pane marker included, because a message truncated to make room for a marker is not worth
  having - and then the widgets come back. Detaching clears the status row, so a bar left along the
  bottom does not suggest gozellij is still running. **Verified** with a failing service in one
  pane of a split, and shown to fail when the message is not drawn.

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

   One piece of the session vocabulary did turn out to be missing rather than a naming question:
   renaming. `gozellij rename <old> <new>` works on a running shell while you are attached to it -
   same process, same scrollback, the log follows, and a daemon crash afterwards recovers it under
   the new name (checked against a real systemd unit in `scripts/fdstore.sh`). Inside the process,
   `GOZELLIJ` keeps the old name until it restarts, which is why the nesting guard asks the
   process tree instead.

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
