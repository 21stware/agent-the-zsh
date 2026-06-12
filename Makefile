# flow — build, install, and package targets.
#
#   make build     compile flowd + flow-agent into ./bin
#   make test      run the Go test suite
#   make install   build, install binaries to the prefix, wire up ~/.zshrc + autostart
#   make dist      build a self-contained flow-<os>-<arch>.tar.gz in ./dist
#   make uninstall remove installed binaries and the ~/.zshrc hook
#   make clean     remove ./bin and ./dist

SHELL := /bin/bash

# Install prefix (binaries go to $(PREFIX)/bin). Override: make install PREFIX=/usr/local
PREFIX ?= $(HOME)/.local
BINDIR := $(PREFIX)/bin

# Where the widget is installed and sourced from.
SHARE_DIR := $(PREFIX)/share/flow
WIDGET_SRC := shell/flow.zsh
DOCTOR_SRC := shell/flow-doctor

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

OS := $(shell go env GOOS)
ARCH := $(shell go env GOARCH)

.PHONY: build test install uninstall dist clean fmt vet

build:
	@mkdir -p bin
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/flowd ./cmd/flowd
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/flow-agent ./cmd/flow-agent
	@echo "built bin/flowd and bin/flow-agent ($(VERSION))"

test:
	go test ./...

fmt:
	gofmt -w internal/ cmd/

vet:
	go vet ./...

# Install binaries + widget locally, wire ~/.zshrc, and start the daemon.
install: build
	@mkdir -p "$(BINDIR)" "$(SHARE_DIR)"
	install -m 0755 bin/flowd "$(BINDIR)/flowd"
	install -m 0755 bin/flow-agent "$(BINDIR)/flow-agent"
	install -m 0644 $(WIDGET_SRC) "$(SHARE_DIR)/flow.zsh"
	install -m 0755 $(DOCTOR_SRC) "$(SHARE_DIR)/flow-doctor"
	@FLOW_BIN="$(BINDIR)" SHARE_DIR="$(SHARE_DIR)" ./scripts/wire-zshrc.sh
	@echo ""
	@echo "Installed. Open a new shell (or 'source ~/.zshrc') to start using flow."

uninstall:
	@rm -f "$(BINDIR)/flowd" "$(BINDIR)/flow-agent"
	@rm -rf "$(SHARE_DIR)"
	@./scripts/unwire-zshrc.sh
	@echo "Uninstalled flow binaries and removed the ~/.zshrc hook."
	@echo "(A running flowd, if any, will exit on next reboot or 'pkill flowd'.)"

# Build a self-contained tarball: binaries for the host OS/arch + widget +
# doctor + install.sh. Unpack on the target and run ./install.sh.
dist: build
	@mkdir -p dist/flow
	cp bin/flowd bin/flow-agent dist/flow/
	cp $(WIDGET_SRC) $(DOCTOR_SRC) dist/flow/
	cp scripts/install.sh dist/flow/install.sh
	chmod +x dist/flow/install.sh
	cp README.md dist/flow/README.md
	tar -C dist -czf dist/flow-$(OS)-$(ARCH).tar.gz flow
	@rm -rf dist/flow
	@echo "packaged dist/flow-$(OS)-$(ARCH).tar.gz"

clean:
	rm -rf bin dist
