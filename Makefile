GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e vet fmt lint leakcheck clean install docs release

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

docs:
	$(GO) run ./tools/docsite -root . -out site -base / -version $(VERSION)

clean:
	rm -rf bin dist site

# Cut a release: make release TAG=v0.4.0. A release is a v* tag on main;
# pushing it runs .github/workflows/release.yml. Where tags cannot be
# pushed, dispatch that workflow on main with the version instead.
release:
	@case "$(TAG)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "usage: make release TAG=v0.4.0"; exit 2;; esac
	@test "$$(git rev-parse --abbrev-ref HEAD)" = main || { echo "release from main (you are on $$(git rev-parse --abbrev-ref HEAD))"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "commit or stash your changes first"; exit 1; }
	@git fetch -q origin main && test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/main)" || { echo "main is not at origin/main; pull first"; exit 1; }
	@! git rev-parse -q --verify "refs/tags/$(TAG)" >/dev/null || { echo "$(TAG) already exists"; exit 1; }
	$(GO) test -race ./...
	git tag -a $(TAG) -m $(TAG)
	git push origin refs/tags/$(TAG)
	@echo "pushed $(TAG); the release workflow builds and publishes it: https://github.com/yjmrobert/wormhole/actions/workflows/release.yml"
