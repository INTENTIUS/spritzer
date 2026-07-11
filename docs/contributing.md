# Contributing

The full contributing guide lives in
[CONTRIBUTING.md](https://github.com/intentius/spritzer/blob/main/CONTRIBUTING.md)
at the repository root. This page summarizes the essentials.

## Setup

You need Go 1.25 or newer and, ideally, [`just`](https://github.com/casey/just).

```sh
git clone https://github.com/intentius/spritzer
cd spritzer
just build
just test
```

## Before opening a pull request

Run the checks CI runs:

```sh
just fmt        # gofmt -w
just lint       # golangci-lint
just race       # go test -race ./...
just docs-build # mkdocs build --strict
```

## Principles

- spritzer is wire-compatible with chant's in-process Sprites fake
  (`sprites-fake.ts`); keep the endpoint shapes and the exec interpreter in step
  with it.
- Route time-dependent behavior through `internal/clock` so tests stay
  deterministic.
- New behavior needs a test.
- Keep the build self-contained; prefer the standard library.

## Conduct and security

Participation is governed by the
[Code of Conduct](https://github.com/intentius/spritzer/blob/main/CODE_OF_CONDUCT.md).
Report vulnerabilities privately as described in
[SECURITY.md](https://github.com/intentius/spritzer/blob/main/SECURITY.md).
