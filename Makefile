GO      ?= go
VERSION := $(shell git describe --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/abox

.PHONY: build test install link docker-build clean

# Static binary, no runtime dependencies.
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/abox

test:
	$(GO) vet ./...
	$(GO) test ./...

# Link into ~/.local/bin. Everything abox needs is embedded, so the binary can also be copied anywhere.
install: build link

link:
	@test -x $(BIN) || { echo "$(BIN) missing: run make build or make docker-build"; exit 1; }
	mkdir -p $(HOME)/.local/bin
	ln -sfn $(CURDIR)/$(BIN) $(HOME)/.local/bin/abox

# Build without a local Go toolchain.
docker-build:
	docker run --rm -u $$(id -u):$$(id -g) -e HOME=/tmp -e GOFLAGS=-buildvcs=false \
	  -v $(CURDIR):/src -w /src golang:1.24 \
	  sh -c 'CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/abox'

clean:
	rm -rf bin
