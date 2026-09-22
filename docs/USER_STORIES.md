# User stories

Who uses gozellij, what they will type, and what has to be true for it to work. Written from the
outside in, against the code as of `da5e0ff` plus the uncommitted supervisor edits, by someone who
did not build it.

Every story has the same shape:

- **Story** - one line, in the user's words.
- **Today** - what the current code does, with the file that does it. "From reading" means it was
  traced in the source and not run; the suite was red at the time of writing.
- **Acceptance** - shell a developer can paste. If it cannot be pasted it is not acceptance.

Anything not verified in the code is marked *(speculation)*.

The four people:

| | who | what they already trust |
|---|---|---|
| **A** | moving one service off Docker onto this | `docker run --restart`, `docker logs -f`, `docker stop` |
| **B** | sshing in at 3am because something broke | `journalctl -u x --since`, `systemctl status x` |
| **C** | wants it to survive a reboot and a `pacman -Syu` | systemd, `enable`, not thinking about it |
| **D** | twenty years of tmux/screen, being asked to switch | `tmux a`, `C-b d`, panes |

A concrete host is assumed throughout: a personal VPS running a Matrix homeserver (Synapse),
its Postgres, and a reverse proxy, all under one unprivileged user.

---

## A. Moving a service off Docker

### A1. Define it, start it, and it comes back when it dies

**Story.** I had `docker run -d --restart unless-stopped synapse`. I want the same one-liner.

**Today.** Works, with two flags that both default to off: `gozellij add synapse -restart always
-start -- python -m synapse.app.homeserver -c homeserver.yaml`. Without `-start` the service is
defined and not running (`fabric.go: Add` sets `Enabled = start`); without `-restart` it runs once.
Docker's `run` starts by definition; here the person has to know both flags. Paper cut, not a bug.

Restart-with-backoff is real and tested (`restart.go`: 1s doubling to 30s, forgiven after 60s up;
`supervisor_test.go: TestBackoffGrowsAcrossRestarts`). A missing binary is reported in `status`
rather than retried silently (`supervisor.go: LastError`; `TestMissingBinaryIsReportedNotSilent`).

**Acceptance.**
```sh
gozellij add a1 -restart always -start -- sh -c 'sleep 1; exit 1'
sleep 5
gozellij status a1 | grep -E 'state: *(running|backing-off)'
gozellij status a1 | grep -E 'restarts: *[1-9]'
gozellij rm a1
```

### A2. Stop it, then start it again

**Story.** `gozellij stop synapse`, do maintenance, `gozellij start synapse`. The most basic pair.

**Today. Broken, from reading.** `Supervisor.Start` sets `started = true` and nothing ever clears
it (`supervisor.go`, around line 180). After `Stop` the loop has exited but the flag stays.
`Fabric.Start` then calls `s.Start(f.ctx)` and deliberately ignores the answer:

```go
// fabric.go, Fabric.Start
if err := s.Start(f.ctx); err != nil && !errors.Is(err, ErrAlreadyStarted) {
```

So `start` after `stop` writes `enabled: true` to disk, starts nothing, and returns success. The
daemon's `settledStatus` (`server.go`) then polls for `SettleWait` (2s) waiting for the state to
leave `stopped`, gives up, and the CLI prints `state: stopped` with exit code 0. The same path
makes `start` on a `failed` service a no-op.

Three things in the tree say this is the case together:

- `supervisor_adversarial_test.go: TestAdvStartAfterStopIsRefusedNotSilent` pins that
  `Supervisor.Start` after `Stop` returns `ErrAlreadyStarted` - "Pinned as documentation of the
  contract, not judged."
- `Fabric.Start` swallows exactly that error.
- `Fabric.Restart` already contains the fix: `fresh := NewSupervisor(s.Service(), f.opts)` and
  swap it in. `restart` works; `start` was never wired to the same thing.

No test does `Stop` then `Start` through the fabric; `server_test.go:
TestAddListStartStopRemove` stops and then removes. This is design rule 1 ("never silently
succeed at doing nothing") in the command a Docker user types first.

**Acceptance.**
```sh
gozellij add a2 -restart always -start -- sleep 300
gozellij stop a2   && gozellij status a2 | grep -q 'state: *stopped'
gozellij start a2  && gozellij status a2 | grep -q 'state: *running'
gozellij rm a2

# and from failed:
gozellij add a2f -start -- false; sleep 1
gozellij status a2f | grep -q 'state: *failed'
gozellij start a2f && gozellij status a2f | grep -qE 'state: *(running|failed)' \
  && gozellij status a2f | grep -q 'starts: *2'
gozellij rm a2f
```
The second block is the real check: `starts` must go to 2. A `start` that returns 0 and leaves
`starts: 1` is the bug.

### A3. `stop` stops the whole thing, not one pid

**Story.** My service is `npm start` (which execs node), or a shell script that launches
workers. `docker stop` took the whole container down. `gozellij stop` must not leave orphans
holding my port.

**Today. Gap, from reading.** `Process.Stop` sends SIGTERM then SIGKILL to *one pid*
(`process.go: Signal` -> `cmd.Process.Signal(sig)` or `syscall.Kill(p.pid, ...)`). The child is a
session leader (creack/pty does `Setsid`), so nothing here signals the process group or a cgroup.
There is no cgroup code at all - `grep -rn cgroup internal/` finds only comments in `restart.go`
and `service.go`. Killing `sh -c '...'` leaves its children running; `DrainGrace` in `process.go`
exists precisely because "a grandchild holding the slave open keeps the read alive", i.e. the
author has already met the orphan.

The systemd unit already requests `Delegate=yes` "so the daemon can freeze and thaw individual
services later" (`packaging/systemd/gozellijd.service`). The same delegation is what makes
kill-the-tree possible; tree-kill is the part a user needs.

**Acceptance.**
```sh
gozellij add a3 -start -- sh -c 'sleep 1000 & sleep 1000 & wait'
sleep 1
gozellij stop a3
sleep 6                                  # StopGrace is 5s
! pgrep -f 'sleep 1000'                  # nothing left behind
gozellij rm a3
```

### A4. What "safer than Docker" actually means

**Story.** The README says safer. I am giving up a container. What am I getting?

**Today.** Honest list, from the code:

- No root daemon and no root-equivalent socket: `server.go: Listen` makes the directory 0700 and
  the socket 0600, tested in `TestSocketIsNotReadableByOthers`. This is a real advantage over the
  Docker socket and the README should say it in those words.
- Nothing else. Services run as the daemon's user, on the host filesystem, with no resource
  limit, and with the daemon's *entire* environment: `process.go: Start` does
  `cmd.Env = append(os.Environ(), s.Env...)`. Under the systemd unit that environment is small.
  Under the README quickstart (`gozellijd &` from an interactive shell) every service inherits
  `SSH_AUTH_SOCK`, `TERM`, and anything the operator ever exported in that shell.

The word "safer" is currently doing work the code does not do. Either the README narrows it to
"no root, no root socket, no image supply chain", or the fabric starts stripping the environment
and limiting resources. The first is a one-line change.

**Acceptance (the narrow claim).**
```sh
# started from an interactive shell with a marker exported:
export CANARY=leaked; gozellijd &
gozellij add a4 -start -- sh -c 'env; sleep 30'; sleep 1
! gozellij logs a4 | grep -q CANARY     # currently fails: the canary is there
gozellij rm a4
```

### A5. Change a service's command without losing it

**Story.** I got a flag wrong. In Docker I `rm` and `run` again, which is annoying, so I moved to
compose and edited a file. Here the files are JSON I am told I can edit.

**Today. Surprising, from reading.** There is no `edit`, `set`, or `apply`; `Registry.Add`
refuses an existing name (`service.go: ErrServiceExists`), so the path is `rm` (which stops it)
then `add`. Fine. The trap is the alternative the design invites: DESIGN rule 5 says the JSON is
there so someone "can fix a service by hand at 3am with a text editor". If they do, and then run
`gozellij restart`, the edit is ignored: `Fabric.Restart` builds the new supervisor from
`s.Service()`, the copy captured at construction (`fabric.go`), not from disk. `gozellij
upgrade`, on the other hand, *does* pick it up, because `AdoptAll` calls `f.reg.Get(h.Name)`.
So hand edits take effect on one command and not the other, which is worse than either
consistently.

**Acceptance.**
```sh
gozellij add a5 -start -- sleep 100
sed -i 's/"sleep"/"true"/' ~/.local/state/gozellij/services/a5.json   # or $GOZELLIJ_STATE_DIR
gozellij restart a5
gozellij status a5 | grep -q 'command: *true'
gozellij rm a5
```
Or: document that files are read only at load, and make `restart` say so when the file differs.
Either passes; silence does not.

### A6. Give it an address

**Story.** Synapse listens on 8008, the proxy talks to it. In Docker I wrote
`reverse_proxy synapse:8008` and never thought about ports.

**Today.** `internal/ula` exists (274 lines, 312 of tests) and is imported by nothing:
`grep -rln 'internal/ula' --include='*.go' .` returns only itself. Even wired in, what it
provides is a hash-derived IPv6 address per service after a one-time `ip -6 route add local`
(design rule 4, correctly). What it does not provide is the half Docker users actually use:
a *name*. Synapse's `bind_addresses` and Caddy's `reverse_proxy` would both need the literal
`fdxx:...` pasted in, and there is no mechanism (env var, hosts entry, resolver) by which the
service or the proxy learns it. Docker's win is embedded DNS - `synapse` resolves - not the IP.

**Acceptance (if this ships).**
```sh
gozellij add a6 -start -- python -m http.server 80 --bind "$(gozellij addr a6)"   # or via env
curl -s "http://a6.gozellij/"      # some name, resolvable by the proxy, without root at runtime
```
If the second line cannot be made true, the feature is a harder-to-type port and should wait.

---

## B. Sshing in to find out why it broke

### B1. What is down, and why

**Story.** Riot says the homeserver is unreachable. I ssh in. First command.

**Today.** Good. `gozellij ls` shows state and restart count; `gozellij status synapse` shows
`last exit`, `last error`, `next try` (`cmd/gozellij/main.go: printStatus`). A definition file
that failed to load travels alongside the list rather than hiding it (`ipc.ListReply.Problems`).
An empty list says "no services defined" rather than printing an empty table.

Two small gaps: `ls` has no column for *why* (you must run `status`), and the COMMAND column is
the bare command word, so the README's own example shows `sh` for a `sh -c '...'` service.

**Acceptance.**
```sh
gozellij add b1 -start -- sh -c 'echo fatal: cannot bind; exit 3'; sleep 1
gozellij ls | grep b1 | grep -q failed
gozellij status b1 | grep -q 'last exit: *code 3'
gozellij rm b1
```

### B2. Read what it said before it died

**Story.** I want the last few hundred lines from before the crash, with times, and I want to
follow new output while I try a fix.

**Today. This is the weakest story in the product.** `gozellij logs` returns `out.Snapshot()`
from an in-memory ring of `DefaultOutputBytes = 256 KiB` (`output.go`; `server.go: logs`).
Consequences, all from reading:

- Nothing is ever written to disk. A daemon crash, `systemctl --user restart`, or a reboot
  leaves *zero* history for the debugging session that follows - exactly the session this
  story is about.
- 256 KiB of Synapse output is minutes, not hours *(speculation about volume; the size is not)*.
- There is no `-f`. The only way to watch is `attach`, which puts the terminal in raw mode and
  forwards every keystroke including Ctrl-C to the service (`attachclient.go: attachOnce`,
  "including Ctrl-C, which belongs to the program you are attached to"). The absence of
  `logs -f` pushes a tired person into the one command that can kill the thing they came to
  look at.
- No timestamps, and the bytes are raw PTY output, escape sequences included
  (`ipc.LogsReply` doc). `grep` works; `--since` does not exist.

`journalctl -u synapse --since -1h -f` is the bar. `docker logs -f --since 1h` is the same bar.

**Acceptance.**
```sh
gozellij add b2 -restart always -start -- sh -c 'echo started $(date +%T); sleep 2; exit 1'
sleep 7
systemctl --user restart gozellijd          # the bad way, on purpose; or kill -9 the daemon
sleep 2
gozellij logs b2 | grep -c started | grep -qvE '^0$'   # history survived the daemon
timeout 5 gozellij logs -f b2 | grep -q started        # follow exists and exits cleanly
gozellij rm b2
```

### B3. Watch it without being able to hurt it

**Story.** I want eyes on the live output. I do not want my Ctrl-C to reach Synapse.

**Today.** Half built. `gozellij attach -r <name>` watches without touching: the daemon drops that
connection's keystrokes and its resizes, so "my Ctrl-C does not reach Synapse" is a property of the
far end rather than a promise the client makes to itself. It also does not resize the service -
somebody watching must not reshape the screen of the person working - and a swallowed keystroke
says so on the status line, once, because a key that quietly goes nowhere is rule 1 with a terminal
attached. The prefix key still works, which is how you leave.

And the other half: `gozellij ls` now says which of the terminals attached can type. `2+1r` is two
that can and one that cannot; `3r` is three that cannot; `gozellij status` spells it out. The number
alone answered the wrong question - three terminals showing a service is reassuring, three that can
type into it is a reason to find out whose they are before you restart it. `logs -f` counts as
read-only too, and not as a policy: a follower has no way to send anything, so it is the same thing
`attach -r` asks to be, arrived at from the other direction.

What is still not there: *whose* they are. The daemon knows a connection, not a person, and saying
more would mean recording who opened it.

**Acceptance.**
```sh
gozellij add b3 -start -- sh -c 'trap "echo got INT" INT; while :; do sleep 1; done'; sleep 1
printf '\003\035' | gozellij attach -r b3       # Ctrl-C then Ctrl-]
! gozellij logs b3 | grep -q 'got INT'
gozellij rm b3
```

---

## C. Surviving a reboot and a package upgrade

### C1. Reboot

**Story.** The VPS reboots for a kernel update. Everything I had running is running again, and
the thing I had stopped on purpose is still stopped.

**Today.** Correct in the fabric: `Enabled` is desired state, `Load()` starts what is enabled
(`fabric.go`), `stop` clears it (`Fabric.Stop`, "a service you stopped on purpose must not come
back"), tested in `TestLoadRestoresWhatWasRunningAndLeavesTheRestAlone` and
`TestStopSurvivesAReload`. Definitions are written atomically with fsync (`service.go: write`).
This part is good.

The part that is not in the code: the daemon only starts at boot if the systemd *user* unit is
enabled **and** `loginctl enable-linger` is on. Without linger the user manager stops at logout
and the README's headline "through logouts" is false. Linger is a root step, correctly kept out
of the binary (rule 4), documented only in `packaging/README.md`, and checked by nothing.
Nothing on the CLI can tell you whether this box is set up to keep the promise.

There is also no ordering between services: Synapse and Postgres start concurrently and Synapse
crash-loops with backoff until Postgres answers. That works by accident and inflates `restarts`.
Low priority; note it.

**Acceptance.**
```sh
gozellij doctor            # does not exist; should say:
#   daemon: running (systemd user unit, enabled)     or how to enable it
#   linger: on                                        or the exact loginctl line
#   versions: client v, daemon v (match)              or "run gozellij upgrade"
#   state dir: ~/.local/state/gozellij (3 services, 0 problems)
```
And the real test, on a throwaway VM: `add` two services, `stop` one, `reboot`, `ls` shows one
running and one stopped, with `restarts: -`.

### C2. Package upgrade

**Story.** `pacman -Syu` replaced `/usr/bin/gozellijd`. I did not run anything. What happens?

**Today.** Nothing happens, which is safe: the old daemon keeps running the old image until
someone sends SIGUSR1 (`gozellij upgrade` or `systemctl --user reload`). The exec-in-place
itself is the best-built thing in the repo: it resolves `/proc/self/exe (deleted)` correctly
(`upgrade.go: ExecSelf`), refuses a manifest version it does not speak, and the CLI compares pids
before and after and fails loudly on a changed one (`cmd/gozellij/main.go: cmdUpgrade`).
The `attach` client survives it (`attachclient.go: AttachLoop`).

Gaps:

- Now the client is newer than the daemon. `cmdList` never calls `PingVersion`, so nothing says
  so until a new op fails with "this daemon may be older than your client" (`server.go:
  dispatch`). A skew warning on `ls` costs one line.
- A distro postinst that runs `systemctl restart` (common) kills every service.
  `packaging/README.md` says "Do **not** use restart" in bold, which is a footgun with a label
  on it, not a fixed footgun.

**Acceptance.**
```sh
# build a second binary with a different -X main.Version into the install path, then:
gozellij ls 2>&1 | grep -q 'daemon .* older .* gozellij upgrade'   # skew is said, not hidden
gozellij upgrade | grep -q 'kept their pid'
```

### C3. Daemon crash

**Story.** The daemon hits a nil pointer, or the OOM killer. Synapse should not care.

**Today.** Not covered, and the README and DESIGN both say so plainly ("a daemon that crashes
takes its services with it"). Credit for honesty. But this is the one failure that breaks the
headline promise in normal life, and it is the reason Docker has `containerd-shim` and
`live-restore`. The systemd unit's `Restart=on-failure` turns a crash into "everything
restarted from scratch", which for Postgres is a crash recovery on every daemon bug.

DESIGN.md names the two fixes: a keeper process, or systemd's fd store. The unit file already
exists, so `FileDescriptorStoreMax=` plus `sd_pid_notify_with_fds` for each pty master is the
short path, and it also makes `systemctl restart` *safe*, which deletes the C2 footgun.

**Verified, on this machine, systemd 261, with a throwaway transient user unit.** The line that
used to be here said "to verify: fdstore behaviour for user units; not checked here". It is checked
now, and three of the four things it turned up were not what I expected:

- **The fd store works for a user unit.** A descriptor handed over with `FDSTORE=1\nFDNAME=x`
  comes back on the next start as `LISTEN_FDS=1 LISTEN_FDNAMES="x"`, with its contents intact,
  after the process exited non-zero and `Restart=on-failure` started it again.
- **`KillMode=process` is required, and this is the whole ball game.** With the default
  `control-group`, systemd kills every process in the cgroup when the unit restarts - so the
  services die anyway and the fd store rescues descriptors to nothing. The first run of the probe
  showed exactly that and looked like the store not working at all.
- **`SendSIGKILL=no` deadlocks it.** With `KillMode=mixed` the store *is* preserved, but the unit
  sits in `auto-restart` for ever waiting for the orphaned services to exit, which they never do.
  `KillMode=process` is the one that both keeps them and lets the unit come back.
- **A stored descriptor is closed on POLLHUP.** The first version of the probe stashed the read end
  of a pipe whose only writer was the crashing process, so systemd dropped it the instant it died -
  a probe measuring its own mistake. For gozellij this is the behaviour we want: a pty master hangs
  up when its last slave closes, so the store drops the descriptors of services that are gone.
- **`FileDescriptorStorePreserve=yes` pins a stopped unit** in `dead-resources-pinned` until
  `systemctl clean --what=fdstore`. The default, `restart`, is what is wanted: keep the store across
  an automatic restart, drop it on an explicit `stop`, where losing the descriptors is correct.

And the part that decides the shape of the code rather than the unit file: **the adopted processes
cannot be reaped.** After a crash the services are reparented, so the new daemon is not their parent.
Measured: `pidfd_open` on an orphan works, `poll` on it returns nothing while the process lives and
`POLLIN` the moment it dies, and `wait4` from a non-parent fails with ECHILD. So an adopted service
can be watched and signalled and its output is intact, but **"it exited" arrives without "with what
code"**. That is a thing `status` has to be able to say, decided now rather than after the first
report of a wrong exit code.

**Acceptance.**
```sh
gozellij add c3 -restart always -start -- sleep 1000; sleep 1
pid=$(gozellij status c3 | awk '/^pid:/{print $2}')
kill -9 "$(systemctl --user show -p MainPID --value gozellijd)"
sleep 5
[ "$(gozellij status c3 | awk '/^pid:/{print $2}')" = "$pid" ]
gozellij rm c3
```

---

## D. Twenty years of tmux

### D1. A shell that is still there tomorrow

**Story.** `tmux new -s work`, do things, `C-b d`, come back next week.

**Today.** Works, awkwardly: `gozellij add work -start -- zsh`, `gozellij attach work`, `Ctrl-]`.
Scrollback survives detach (the ring), output is replayed on attach (`AttachRequest.Replay`),
resize follows the client (`SIGWINCH` -> `OpResize`). The name must match
`^[a-z0-9][a-z0-9_-]{0,63}$` (`service.go`), so `work.2` is refused with a clear message.

What they will notice in the first hour:

- No windows, no panes, no status line. One service, one terminal. The README says so.
- Two people (or two terminals) attached to the same service: each attach resizes the pty to
  *its* size (`attach.go: attach`, "Size the pty to the attaching client before replaying"), so
  the second attach reshapes the first's screen, last-wins, with no notice. tmux uses the
  smallest client and says so in the status bar.
- The detach key is fixed at `Ctrl-]` (`attachclient.go: DetachKey`) and cannot be changed.
  Emacs users have opinions about `Ctrl-]`.
- No `list-clients`, so no way to know whether someone else is typing into the same shell.
- Replay writes up to 256 KiB of raw bytes into the terminal. Once the ring has wrapped, the
  replay starts at an arbitrary byte, possibly inside an escape sequence *(speculation: the
  existence of `-no-replay` suggests someone has seen the result; not verified)*.

**Acceptance.**
```sh
gozellij add d1 -start -- sh; sleep 1
printf 'echo hello-from-shell\n\035' | gozellij attach d1     # type, then Ctrl-]
gozellij status d1 | grep -q 'state: *running'                 # detach did not kill it
gozellij logs d1 | grep -q hello-from-shell
gozellij rm d1
```
And: two `attach`es at different sizes; the first client is told the size changed, or the pty
keeps the smaller size. Currently neither.

### D2. Panes

**Story.** I want Synapse's log on the left and a shell on the right.

**Today.** Not built; this is Phase 3, which requires a terminal emulator, which is the 98.8% the
README describes leaving behind. DESIGN.md: "It is *not* an attempt to be a better tmux. The
multiplexer is the face, not the product."

The honest answer to user D in the meantime is: open two terminals, or run gozellij *inside*
tmux. That works today with zero code, because attach is a byte pipe. The `attach` client
should probably say so in `-h`.

No acceptance criteria are offered for panes here, deliberately - see the priority notes in
the report that accompanied this file. If panes are built, the acceptance test is the oracle in
Phase 2, and that is the correct order.

---

## What is good and should not be touched

One line each, because the point of this file is the gaps.

- Atomic, fsynced, human-readable service files, one per service (`service.go: write`).
- `ListReply.Problems`: broken definitions neither hide nor are hidden by the working ones.
- `gozellij upgrade` compares pids and refuses to round a partial success up to success.
- `LastError` on a service that cannot start, surfaced last in `status` so it is never buried.
- The restart notice written into the output stream itself (`supervisor.go: waitBackoff`), so a
  watcher sees why the program started over.
- Every request gets exactly one answer, including the failures (`server_test.go`).
- The crash disclaimer in the README. Keep it until C3 is done, then delete it with pleasure.

## Order these were found in, for whoever picks this up

1. A2 - `stop` then `start` is a silent no-op. Zero features, one bug, first.
2. A3 - `stop` orphans the process tree. The safety claim depends on it.
3. B2 - logs to disk and `logs -f`. The 3am story does not work without it.
4. C1 - `gozellij doctor`, or any command that says whether this box keeps the promise.
5. C3 - survive a daemon crash via the fd store. Also fixes C2's footgun.
6. A4 - narrow the "safer" claim in the README to what the code does, today.
7. A5 - hand-edited files and `restart` disagree.
8. C2 - version skew warning on `ls`.
9. D1 - multi-attach resize policy; configurable detach key.
10. A6 - ULA, only once there is a name to go with the address.
