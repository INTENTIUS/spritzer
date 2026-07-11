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

# Cut a release: preflight checks, then tag and push vVERSION (triggers the
# Release workflow, which builds binaries + the GHCR image). Usage:
#   just release 0.2.0
# The CHANGELOG must already have a `## [VERSION]` section (see RELEASING.md).
release version:
    #!/usr/bin/env bash
    set -euo pipefail
    [ -z "$(git status --porcelain)" ] || { echo "✗ working tree not clean"; exit 1; }
    [ "$(git rev-parse --abbrev-ref HEAD)" = "main" ] || { echo "✗ not on main"; exit 1; }
    git pull --ff-only origin main
    grep -q "## \[{{version}}\]" CHANGELOG.md || { echo "✗ CHANGELOG.md has no '## [{{version}}]' section"; exit 1; }
    echo "→ preflight: build / vet / gofmt / test"
    go build ./...
    go vet ./...
    [ -z "$(gofmt -l .)" ] || { echo "✗ gofmt reported files"; exit 1; }
    go test ./...
    echo "→ tagging v{{version}}"
    git tag -a "v{{version}}" -m "spritzer v{{version}}"
    git push origin "v{{version}}"
    echo "✓ pushed v{{version}} — the Release workflow builds binaries + the GHCR image."
    echo "  watch: gh run watch --repo INTENTIUS/spritzer"
