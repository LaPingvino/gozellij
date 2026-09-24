#!/usr/bin/env bash
# package.sh: the Arch package that replaces gezellij still says what it must.
#
# What was left of scripts/acceptance.sh when everything with a screen or a daemon in it moved to
# Go tests (internal/daemon/screen_test.go and the port_*_test.go beside it): these read the
# PKGBUILD through makepkg, offline and without sudo, which only a script can.
#
#   ./scripts/package.sh
set -u
repo=$(cd "$(dirname "$0")/.." && pwd)
pass=0
fail=0
say()  { printf '%s\n' "$*"; }
ok()   { pass=$((pass + 1)); printf '  PASS  %s\n' "$*"; }
bad()  { fail=$((fail + 1)); printf '  FAIL  %s\n' "$*"; }

# ------------------------------------------ the package that replaces the other multiplexer
#
# The PKGBUILD is the only thing standing between a commit here and the machine somebody
# actually uses. A typo in it is not a small bug: it is "I cannot upgrade", and it is found at
# the worst moment, by the person who wanted the fix.
#
# Nothing checked it. makepkg --printsrcinfo sources the file and prints what it declares,
# offline and without sudo, so the parts that matter can be asserted rather than assumed - and
# the parts that matter are the ones that let it take a machine over from gezellij. Getting
# `replaces` wrong does not fail loudly; it leaves both installed, two login blocks, and a
# fight over the terminal.
if ! command -v makepkg >/dev/null 2>&1; then
    say "  SKIP  the packaging checks, because makepkg is not installed"
else
    srcinfo=$(cd "$repo/packaging/arch" && timeout 60 makepkg --printsrcinfo 2>&1)
    if [ -n "$srcinfo" ] && printf '%s' "$srcinfo" | grep -q '^pkgbase = gozellij-git'; then
        ok "the PKGBUILD parses and declares itself"
    else
        bad "makepkg could not read the PKGBUILD: $(printf '%s' "$srcinfo" | head -3 | tr '\n' '|')"
    fi

    # The whole point of the package: it takes over from the other one rather than sitting
    # beside it.
    if printf '%s' "$srcinfo" | grep -q 'replaces = gezellij-git' &&
       printf '%s' "$srcinfo" | grep -q 'conflicts = gezellij-git'; then
        ok "and it replaces and conflicts with gezellij-git, so pacman swaps them"
    else
        bad "the package does not replace gezellij-git: $(printf '%s' "$srcinfo" | grep -E 'replaces|conflicts' | tr '\n' '|')"
    fi

    # Every file the package build reaches for, checked from the recipe rather than believed.
    # A rename in the repo that nobody carried into the PKGBUILD fails at `makepkg`, on the
    # machine of the person upgrading.
    missing=""
    for f in packaging/systemd/gozellijd.service packaging/arch/gozellij-git.install LICENSE; do
        [ -f "$repo/$f" ] || missing="$missing $f"
    done
    if [ -z "$missing" ]; then
        ok "and every file its package() step installs is in the repository"
    else
        bad "the PKGBUILD installs files that are not here:$missing"
    fi

    # The unit it generates is the repo's with ExecStart rewritten to the installed path. If
    # that sed stops matching, the unit ships pointing at somebody's build directory.
    generated=$(sed 's|^ExecStart=.*|ExecStart=/usr/bin/gozellijd|' "$repo/packaging/systemd/gozellijd.service")
    if printf '%s' "$generated" | grep -q '^ExecStart=/usr/bin/gozellijd$'; then
        ok "and the unit it generates starts the installed daemon, not a build directory"
    else
        bad "the generated unit says: $(printf '%s' "$generated" | grep -i execstart | tr '\n' '|')"
    fi
fi


if [ "$fail" -gt 0 ]; then
    say "$pass held, $fail did not."
    exit 1
fi
say "all $pass held."
