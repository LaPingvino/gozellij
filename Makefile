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

.PHONY: all build test race vet fmt conform record acceptance install clean check

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

# check is what to run before pushing.
check: fmt vet test race conform acceptance

install: build
	install -Dm755 bin/gozellijd $(BIN)/gozellijd
	install -Dm755 bin/gozellij  $(BIN)/gozellij

clean:
	rm -rf bin
