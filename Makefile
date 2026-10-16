PREFIX     ?= $(HOME)/.local
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
GORELEASER ?= goreleaser

.PHONY: build install test snapshot release goreleaser-check

# Build ./bin/so for this machine.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/so ./cmd/so

# Build and install to $(PREFIX)/bin/so, for running a local checkout.
install:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(PREFIX)/bin/so ./cmd/so

test:
	go vet ./...
	go test -race ./...

# Build every release archive into dist/ without publishing.
snapshot: goreleaser-check
	$(GORELEASER) release --snapshot --clean

# Publish a GitHub release for the current tag. CI runs this on v* tags.
release: goreleaser-check
	$(GORELEASER) release --clean

goreleaser-check:
	@command -v $(GORELEASER) >/dev/null || { echo "goreleaser not found: brew install goreleaser (or set GORELEASER=/path/to/goreleaser)"; exit 1; }
