# gozellij
#
# The point of this file is that `make acceptance` is one word. The acceptance script is the thing
# that re-checks every promise in docs/REPLACING_GEZELLIJ.md on this machine, and a check nobody
# runs is a claim - so it should be as cheap to run as the tests are.

GOFLAGS ?=
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local
BIN     ?= $(PREFIX)/bin
LDFLAGS  = -X main.Version=$(VERSION)

.PHONY: all build test race vet fmt conform record fuzz acceptance quick fdstore install clean check

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

# acceptance drives real ptys, a real daemon under `env -i`, and a real tmux screen. It is slower
# than the tests and it is meant to be: the last several bugs it caught were ones no unit test
# could see, because they were about what the terminal ends up showing.
acceptance:
	bash scripts/acceptance.sh

# quick runs the promises that do not need a terminal: sixty-two of them in under a minute,
# against a hundred and ninety-two in about fourteen. For the loop you are in while changing the
# daemon or the fabric, where waiting fourteen minutes to learn you mistyped something is its own
# kind of bug.
#
# Put a new check here unless it needs a terminal. Appending to the end of acceptance.sh lands it
# inside the block -noscreen skips, which is where several checks that needed no terminal sat
# unrun until somebody noticed.
#
# Not part of `check`, and it says on the way out that it skipped the rest. A fast mode that
# reports the same line as the real one is how a suite starts lying: somebody says "all green"
# meaning a fifth of it.
quick:
	bash scripts/acceptance.sh -noscreen

# fdstore checks what survives a daemon crash, against a real systemd user manager: the listening
# socket, the services and their pids, the terminals they are talking through, and the person who
# was attached when it happened.
#
# Its own script rather than part of `acceptance`, because that one runs the daemon under `env -i`
# with no service manager anywhere - on purpose, it is how TERM=dumb was found - and this needs the
# opposite. Out of `check` for a different reason, and worth being clear about which: not because
# it is unreliable, but because it starts and kills transient units in the user's own systemd, and
# a suite that is run reflexively should not have side effects outside the directory it is run in.
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
check: fmt vet test race conform acceptance fdstore

install: build
	install -Dm755 bin/gozellijd $(BIN)/gozellijd
	install -Dm755 bin/gozellij  $(BIN)/gozellij

clean:
	rm -rf bin
