# gozellij
#
# The point of this file is that `make check` is one word, and cheap enough to run every time. The
# promises in docs/REPLACING_GEZELLIJ.md are Go tests now - the screen ones read what the attach
# client draws with gozellij's own terminal emulator (internal/daemon/screen_test.go) - so
# `make test` covers them in about a minute, where the tmux script they replaced took forty.

GOFLAGS ?=
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local
BIN     ?= $(PREFIX)/bin
LDFLAGS  = -X main.Version=$(VERSION)

.PHONY: all build test race vet fmt conform record fuzz package fdstore upgrade-from install clean check

all: build

build:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/gozellijd ./cmd/gozellijd
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/gozellij  ./cmd/gozellij

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l cmd internal

# package checks the PKGBUILD through makepkg, offline and without sudo: that it parses, replaces
# gezellij-git, installs only files that exist, and generates a unit that starts the installed
# daemon. What was left of acceptance.sh that only a script can do.
package:
	bash scripts/package.sh

# fdstore checks what survives a daemon crash, against a real systemd user manager: the listening
# socket, the services and their pids, the terminals they are talking through, and the person who
# was attached when it happened.
#
# It starts and kills transient units in the user's own systemd, which no in-process test can do.
# It is a no-op with nothing to say on a machine without systemd.
fdstore:
	bash scripts/fdstore.sh

# upgrade-from is the upgrade somebody actually does: from the build they have installed to the
# one just built, which can span weeks of changes to the handover. Every other suite upgrades a
# binary to *itself*, so nothing else here tests two different builds talking to each other.
#
#   make upgrade-from FROM=14ae127
#
# Not in `check`: it needs a revision to come from, and which one is a fact about somebody's
# machine rather than about this tree. Run it before telling anybody an upgrade is safe.
FROM ?=
upgrade-from:
	@test -n "$(FROM)" || { echo "give a revision: make upgrade-from FROM=<rev>"; exit 2; }
	bash scripts/upgrade-from.sh $(FROM)

# conform runs the corpus against tmux: every case is driven through a real terminal emulator and
# compared with the recording checked in beside it. It is separate from `test` because it needs
# tmux, and it is in `check` because a corpus that is not run is a corpus that is not true.
#
# `make record` re-records the recordings from tmux, which is how the corpus grows. It is never
# run automatically: a harness that records whatever it finds can never fail.
conform:
	go test ./internal/vt/conform/ -count=1

record:
	go test ./internal/vt/conform/ -count=1 -record -v

# fuzz runs the generator wide: streams nobody wrote, checked against a real tmux, with any
# disagreement shrunk to something readable and printed as a ready-made corpus case.
#
# Not part of `check`, and the reason is that it is a different kind of tool. `check` answers "did
# I break something", takes a few minutes and has to be trustworthy enough to gate a push. This
# answers "what else is wrong", takes as long as you let it, and finding something is the good
# outcome rather than the bad one. Twelve streams run as part of `make test` so that a regression
# in an ordinary sequence is still caught by an ordinary run.
fuzz:
	go test ./internal/vt/grid/ -count=1 -run TestAgainstTmux -fuzz 200 -fuzzseed $$RANDOM -timeout 60m

# check is what to run before pushing.
# check runs everything that can run here. fdstore is in it because leaving it out is how it went
# ninety-odd commits without being run at all: it is the only thing covering what survives a daemon
# restart, and nothing else fails when that breaks. It costs nothing on a machine without a systemd
# user manager - it says so and exits 0.
check: fmt vet test race conform package fdstore

install: build
	install -Dm755 bin/gozellijd $(BIN)/gozellijd
	install -Dm755 bin/gozellij  $(BIN)/gozellij

clean:
	rm -rf bin
