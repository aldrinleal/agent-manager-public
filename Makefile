.PHONY: all build build-arm64 cli test lint fmt vet run serve image install uninstall

GOARCH ?= amd64
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

all: build

# --- local dev -------------------------------------------------------------

# Static linux binaries (both), as the Containerfile expects them.
# CGO_ENABLED=0 is load-bearing: it is why registry/ uses modernc.org/sqlite
# and not mattn/go-sqlite3.
build:
	-mkdir -p bin/
	env CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ./bin/agentmgrd ./cmd/agentmgrd
	env CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" -o ./bin/agentmgr  ./cmd/agentmgr

build-arm64:
	$(MAKE) build GOARCH=arm64

# Host-native CLI, for using it from this machine.
cli:
	-mkdir -p bin/
	go build -ldflags="$(LDFLAGS)" -o ./bin/agentmgr ./cmd/agentmgr

test:
	go test ./...

vet:
	go vet ./...

lint: vet
	gofmt -l . | tee /dev/stderr | (! read)

fmt:
	gofmt -w .

# make run ARGS="ls --active"
run:
	go run ./cmd/agentmgr $(ARGS)

serve:
	go run ./cmd/agentmgrd

# Local container image (no push). Use podman or docker: make image ENGINE=docker
ENGINE ?= podman
image: build
	$(ENGINE) build -f Containerfile -t agent-manager:$(VERSION) .

# --- local helpers ---------------------------------------------------------
# The shell helpers in scripts/ (see scripts/README.md). cc-kill is a symlink
# to cc-grep: the script picks its mode from $$0. scripts/observer/ is for
# remote hosts and is deliberately not installed here.

PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

install:
	install -d $(BINDIR)
	rm -f $(BINDIR)/cc-grep  # an older install may be a symlink into the repo
	install -m 0755 scripts/cc-grep $(BINDIR)/cc-grep
	ln -sf cc-grep $(BINDIR)/cc-kill

uninstall:
	rm -f $(BINDIR)/cc-grep $(BINDIR)/cc-kill
