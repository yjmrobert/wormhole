GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e vet fmt lint leakcheck clean install

build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/wormhole ./cmd/wormhole

install:
	$(GO) install -trimpath -ldflags '$(LDFLAGS)' ./cmd/wormhole

test: vet
	$(GO) test -race ./...

e2e:
	$(GO) test -race -v ./test/e2e/

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:" && gofmt -l . && exit 1)

leakcheck:
	./scripts/leakcheck.sh

clean:
	rm -rf bin dist
