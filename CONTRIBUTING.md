# Contributing to spritzer

Thanks for your interest in improving spritzer. This document covers how to get
set up, the conventions the project follows, and how to submit changes.

## Getting started

You need Go 1.25 or newer. The project uses [`just`](https://github.com/casey/just)
as its task runner (a `Makefile` mirrors the same targets if you prefer `make`).

```sh
git clone https://github.com/intentius/spritzer
cd spritzer
just build
just test
```

## Before you open a pull request

Run the same checks CI runs:

```sh
just fmt     # gofmt -w
just lint    # golangci-lint run
just race    # go test -race ./...
```

CI runs `go build`, `go vet`, a `gofmt` check, `golangci-lint`, the race test
suite with coverage, and `mkdocs build --strict`. A green local run of the
commands above should keep CI green.

## Conventions

- The wire behavior is the contract. spritzer is wire-compatible with chant's
  in-process Sprites fake (`sprites-fake.ts`); the endpoint shapes and the exec
  interpreter must match it so chant's integration suite passes against the
  spritzer image unchanged. When in doubt, read the fake.
- Time-dependent behavior goes through the `internal/clock` abstraction so tests
  stay deterministic and fast. Do not call `time.Now` or `time.Sleep` directly
  in the store.
- New behavior needs a test. The sprite and server packages both have table- and
  harness-style tests to follow.
- Keep the build self-contained. spritzer deliberately avoids third-party
  dependencies; prefer the standard library.

## Documentation

Doc pages live in `docs/` and are built with mkdocs-material. Preview them with
`just docs-serve` and validate with `just docs-build` (which runs
`mkdocs build --strict`, the same check CI enforces).

## Reporting bugs and requesting features

Open an issue using the bug or feature template. For anything security-related,
see [SECURITY.md](./SECURITY.md) instead of filing a public issue.

## Releasing

Maintainers: see [RELEASING.md](./RELEASING.md). Releases are tag-driven — `just release X.Y.Z` from a clean `main`.
