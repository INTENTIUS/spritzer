# Makefile mirrors the justfile. `just` is the primary task runner; this exists
# for contributors who prefer make. Keep the two in sync.

BINARY   := spritzer
PKG      := ./...
IMAGE    := ghcr.io/intentius/spritzer
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: build test race lint cover e2e-real e2e-docker docker run tidy fmt docs-build docs-serve

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

e2e-real:
	./scripts/e2e-k3d.sh

e2e-docker:
	./scripts/e2e-docker.sh

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
