# justfile is the primary task runner for spritzer (the intentius org uses just).
# The Makefile mirrors these recipes; keep the two in sync.

binary  := "spritzer"
pkg     := "./..."
image   := "ghcr.io/intentius/spritzer"
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
ldflags := "-s -w -X main.version=" + version

# List available recipes.
default:
    @just --list

# Compile the binary.
build:
    go build -ldflags "{{ldflags}}" -o {{binary}} ./cmd/spritzer

# Run the test suite.
test:
    go test {{pkg}}

# Run the test suite with the race detector.
race:
    go test -race {{pkg}}

# Lint with golangci-lint (matches CI).
lint:
    golangci-lint run {{pkg}}

# Produce a coverage profile and print the total.
cover:
    go test -race -coverprofile=coverage.out {{pkg}}
    go tool cover -func=coverage.out | tail -1

# Tidy module dependencies.
tidy:
    go mod tidy

# Format all Go sources.
fmt:
    gofmt -w .

# Container exec mode acceptance on a throwaway k3d cluster (INTENTIUS/spritzer#22).
e2e-real:
    ./scripts/e2e-k3d.sh

# Container exec mode acceptance against the local Docker daemon.
e2e-docker:
    ./scripts/e2e-docker.sh

# Build the container image.
docker:
    docker build -t {{image}}:{{version}} .

# Build and run the server locally.
run: build
    ./{{binary}}

# Build the documentation site with strict checking (matches CI).
docs-build:
    mkdocs build --strict

# Serve the documentation site locally with live reload.
docs-serve:
    mkdocs serve

# Dispatch the Release workflow (see RELEASING.md). The label is `auto` or a
# bare version. Usage:
#   just release          # label auto
#   just release 0.6.5
release label="auto":
    gh workflow run publish.yml --repo INTENTIUS/spritzer --ref main -f label={{label}}
