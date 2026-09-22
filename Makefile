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

.PHONY: all build test race vet fmt conform record fuzz acceptance install clean check

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
check: fmt vet test race conform acceptance

install: build
	install -Dm755 bin/gozellijd $(BIN)/gozellijd
	install -Dm755 bin/gozellij  $(BIN)/gozellij

clean:
	rm -rf bin
