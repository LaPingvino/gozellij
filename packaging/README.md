# Packaging

## Arch: the package, which is also the migration

`arch/PKGBUILD` builds `gozellij-git`. It installs `/usr/bin/gozellij`, `/usr/bin/gozellijd` and the
systemd user unit to `/usr/lib/systemd/user/`, so there is nothing to copy into your home.

It carries `conflicts` and `replaces` for `gezellij-git`, which makes installing it the migration:
pacman removes gezellij and puts this in its place.

```sh
cd packaging/arch
makepkg -si
```

That is safe in one specific way worth knowing. Removing `gezellij-git` takes `/usr/bin/gezellij`
with it, and the login block that package's setup script wrote into your profile guards on
`[ -x "$GEZELLIJ_BIN" ]`. The moment the binary is gone that block stops doing anything, **by its
own condition**, with no dotfile edited. You are left with a login that starts nothing - which is a
state you can log in from - until you run:

```sh
gozellij login-setup            # says what it would change; changes nothing
gozellij login-setup -install
```

Two things the package deliberately does not do, because neither belongs to a package manager:
`loginctl enable-linger` needs root and changes how your session behaves after logout, and
`login-setup` edits your profile. The `.install` file says both at the moment you would otherwise
have to go looking.

The unit it ships is `systemd/gozellijd.service` with `ExecStart` rewritten to `/usr/bin/gozellijd`,
generated during `package()` rather than kept as a second copy - two files saying the same thing
drift apart.

`check()` runs `go vet` and the tests that do not need a terminal. The emulator's tests drive a real
tmux and the acceptance script drives a real pty; both are right to run on a machine somebody uses
and wrong to require of a build host. A passing build is not a checked emulator - `make check` is.

## systemd user unit, without a package

`systemd/gozellijd.service` runs the fabric daemon as your own user — no root, no system unit.

```sh
go build -o ~/.local/bin/gozellijd ./cmd/gozellijd
go build -o ~/.local/bin/gozellij  ./cmd/gozellij

mkdir -p ~/.config/systemd/user
cp packaging/systemd/gozellijd.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now gozellijd
```

To keep it running after you log out:

```sh
sudo loginctl enable-linger $USER
```

`Delegate=yes` in the unit is not decoration: it gives the daemon a cgroup it may subdivide, which
is what lets `gozellij stop` stop *everything* a service started, including a child that called
`setsid` to get away. A daemon started by hand from a login shell lands in a root-owned session
scope instead, and falls back to killing the process group — which that child escapes.

Then check it took, along with everything else on this page that this program depends on but does
not control:

```sh
gozellij doctor
```

### Upgrading

```sh
go build -o ~/.local/bin/gozellijd ./cmd/gozellijd   # new binary in place
gozellij upgrade                                      # or: systemctl --user reload gozellijd
```

Both send `SIGUSR1`, which makes the daemon replace its own image while keeping every supervised
process — same pids, no restart. `gozellij upgrade` additionally compares the pids before and
after and fails loudly if any process did not survive.

Prefer `reload` to `systemctl --user restart gozellijd`. A restart used to take every service down
with the daemon; it no longer does - the unit's `KillMode=process` leaves the services alone, systemd
keeps the listening socket and every service's terminal across the restart, and the new daemon
adopts them (checked in `scripts/fdstore.sh`). But a restart still ends every attached terminal's
connection for a moment, where `reload` hands the socket straight across. An explicit `stop` is
different on purpose: it lets systemd drop what it was holding.

## Where things live

| | |
|---|---|
| socket | `$XDG_RUNTIME_DIR/gozellij/fabric.sock` (0700 dir, 0600 socket) |
| service definitions | `$XDG_STATE_HOME/gozellij/services/*.json` |

Both can be overridden with `GOZELLIJ_RUNTIME_DIR` and `GOZELLIJ_STATE_DIR`, which is also how the
tests keep out of your real fabric.

Service definitions are plain JSON, one file per service, written atomically. They are meant to be
readable and repairable with a text editor when something has gone wrong.
