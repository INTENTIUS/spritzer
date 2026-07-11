# Makefile mirrors the justfile. `just` is the primary task runner; this exists
# for contributors who prefer make. Keep the two in sync.

BINARY   := spritzer
PKG      := ./...
IMAGE    := ghcr.io/intentius/spritzer
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint cover docker run tidy fmt docs-build docs-serve release

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/spritzer

test:
	go test $(PKG)

race:
	go test -race $(PKG)

lint:
	golangci-lint run $(PKG)

cover:
	go test -race -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

docker:
	docker build -t $(IMAGE):$(VERSION) .

run: build
	./$(BINARY)

tidy:
	go mod tidy

fmt:
	gofmt -w .

docs-build:
	mkdocs build --strict

docs-serve:
	mkdocs serve

# Cut a release: preflight, then tag and push vV. Usage: make release V=0.2.0
# (`just release 0.2.0` is the primary path; see RELEASING.md.)
release:
	@test -n "$(V)" || { echo "usage: make release V=X.Y.Z"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree not clean"; exit 1; }
	@test "$$(git rev-parse --abbrev-ref HEAD)" = "main" || { echo "not on main"; exit 1; }
	git pull --ff-only origin main
	@grep -q "## \[$(V)\]" CHANGELOG.md || { echo "CHANGELOG.md has no '## [$(V)]' section"; exit 1; }
	go build ./... && go vet ./... && test -z "$$(gofmt -l .)" && go test ./...
	git tag -a "v$(V)" -m "spritzer v$(V)"
	git push origin "v$(V)"
