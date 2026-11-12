SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GO ?= go
BIN := bin
LDFLAGS := -s -w -X github.com/haasele/prox/internal/version.Version=$(VERSION) -X github.com/haasele/prox/internal/version.Commit=$(COMMIT)
export CGO_ENABLED=0

.PHONY: all build test e2e lint completions package package-tar package-deb clean authz-e2e doctor-e2e

all: build

build:
	mkdir -p $(BIN)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/prox ./cmd/prox
	ln -sf prox $(BIN)/gw
	ln -sf prox $(BIN)/proxd

test:
	$(GO) test ./...

e2e: build
	$(GO) test -tags=e2e -count=1 ./test/e2e

lint:
	@out="$$(gofmt -l cmd internal profiles web test)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...
	$(GO) vet -tags=e2e ./test/e2e

completions: build
	mkdir -p packaging/completions
	$(BIN)/prox completions bash > packaging/completions/prox.bash
	$(BIN)/prox completions zsh > packaging/completions/_prox
	$(BIN)/prox completions fish > packaging/completions/prox.fish

package-deb: build completions
	VERSION=$(VERSION) packaging/build-deb.sh

package-tar: build completions
	mkdir -p dist
	tar -C $(BIN) -czf dist/prox-$(VERSION)-linux-amd64.tar.gz prox gw proxd
	cp packaging/completions/* dist/

package: package-deb package-tar

authz-e2e: build
	sudo env PATH="$$PATH" bash test/e2e/authz-ci.sh

doctor-e2e: build
	sudo env PATH="$$PATH" bash test/e2e/doctor-ci.sh

clean:
	rm -rf $(BIN) dist build
