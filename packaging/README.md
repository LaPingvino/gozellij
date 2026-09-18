# Packaging

## systemd user unit

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

Do **not** use `systemctl --user restart gozellijd` for an upgrade. That stops the daemon, which
takes its services down with it, and starts them again from scratch. `reload` is the one that
keeps them.

## Where things live

| | |
|---|---|
| socket | `$XDG_RUNTIME_DIR/gozellij/fabric.sock` (0700 dir, 0600 socket) |
| service definitions | `$XDG_STATE_HOME/gozellij/services/*.json` |

Both can be overridden with `GOZELLIJ_RUNTIME_DIR` and `GOZELLIJ_STATE_DIR`, which is also how the
tests keep out of your real fabric.

Service definitions are plain JSON, one file per service, written atomically. They are meant to be
readable and repairable with a text editor when something has gone wrong.
