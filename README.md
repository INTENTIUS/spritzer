# spritzer

A standalone, stateful local emulator of the Fly.io Sprites API. Like LocalStack, but for Fly Sprites.

[![CI](https://github.com/intentius/spritzer/actions/workflows/ci.yml/badge.svg)](https://github.com/intentius/spritzer/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/intentius/spritzer.svg)](https://pkg.go.dev/github.com/intentius/spritzer)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)
[![Release](https://img.shields.io/github/v/release/intentius/spritzer)](https://github.com/intentius/spritzer/releases)
[![GHCR](https://img.shields.io/badge/ghcr.io-intentius%2Fspritzer-blue)](https://github.com/intentius/spritzer/pkgs/container/spritzer)

## Purpose

Testing a Sprites client, such as a workflow that checkpoints a sandbox before a
risky step and restores it on failure, requires testing against state: a sprite
whose filesystem a command mutates, a checkpoint that captures that filesystem,
and a restore that rewinds to it. A schema mock holds no state, so it cannot
model these behaviors. spritzer keeps sprites, their filesystems, and their
checkpoints in memory and runs a small exec interpreter, so a client's
checkpoint-as-compensation logic can be exercised end to end offline.

spritzer is wire-compatible with the in-process Sprites fake in the `chant`
lexicon (`sprites-fake.ts`): the endpoint shapes and the exec interpreter match
it, so the same integration suite passes against the spritzer container image.

## Features

- Stateful in-memory store of sprites keyed by name, each with a filesystem
  (path → contents) and an ordered list of checkpoints.
- `exec` runs a small scripted interpreter (`echo > path`, `echo`, `cat`, `rm`,
  `true`/`false`, `./risky.sh`, and an echo-back default) so a command can write,
  overwrite, or fail a filesystem key and the result is observable.
- Checkpoint / restore: a checkpoint deep-copies the filesystem under a
  server-assigned version id (`v1`, `v2`, …) with an optional caller comment; a
  restore takes a checkpoint id in the path, replaces the filesystem with that
  copy, and returns the sprite to `running`. This is the
  checkpoint-as-compensation primitive.
- A destroyed or missing sprite returns `404` on any subsequent operation.
- A `/_spritzer/health` endpoint reporting version and implemented paths.
- Single static binary and distroless container image; no runtime dependencies.

## Quick start

Run the container:

```sh
docker run --rm -p 4290:4290 ghcr.io/intentius/spritzer:latest
```

Or install with Go:

```sh
go install github.com/intentius/spritzer/cmd/spritzer@latest
spritzer            # listens on :4290 by default
```

Point a Sprites client at it with the same environment variable the real client
uses:

```sh
export SPRITES_BASE_URL=http://localhost:4290
```

The listen address is configurable with `-addr` or `SPRITZER_ADDR` (default
`:4290`).

## Usage example

```sh
BASE=http://localhost:4290

# Create a sprite (its name is its id).
curl -s -X POST "$BASE/v1/sprites" -d '{"name":"demo"}'
# => {"id":"demo","url":"http://localhost:4290/s/demo"}

# Seed state, then checkpoint it. The server assigns the version id.
curl -s -X POST "$BASE/v1/sprites/demo/exec" -d '{"cmd":"echo good > /state"}'
curl -s -X POST "$BASE/v1/sprites/demo/checkpoints" -d '{"comment":"pre-run"}'
# => {"id":"v1"}

# List the checkpoints (creation order).
curl -s "$BASE/v1/sprites/demo/checkpoints"
# => {"checkpoints":[{"id":"v1","comment":"pre-run"}]}

# Run a risky step that corrupts state and fails.
curl -s -X POST "$BASE/v1/sprites/demo/exec" -d '{"cmd":"./risky.sh"}'
# => {"stdout":"","stderr":"risky.sh: failed\n","exitCode":1}

# Restore rewinds the filesystem to the checkpoint, addressed by id in the path.
curl -s -X POST "$BASE/v1/sprites/demo/checkpoints/v1/restore"
curl -s "$BASE/v1/sprites/demo"
# => {"id":"demo","status":"running","url":"...","fs":{"/state":"good"},"checkpoints":[{"id":"v1","comment":"pre-run"}]}
```

## Comparison

| Capability | spritzer | Schema mock | Real Sprites |
| --- | --- | --- | --- |
| Sprite filesystem that exec mutates | Yes | No | Yes |
| Checkpoint / restore (rewind on failure) | Yes | No | Yes |
| Destroyed-sprite `404` semantics | Yes | No | Yes |
| Runs fully offline | Yes | Yes | No |
| Cost | Free | Free | Billed |
| Real sandboxes, images, code execution | No | No | Yes |

## API coverage

Implemented: create, exec, checkpoint, list checkpoints, restore-by-id, destroy,
and an inspection `GET`, plus a `/_spritzer/health` report. The full table is in the
[API coverage docs](https://intentius.github.io/spritzer/api-coverage/).

## Development

The primary task runner is [`just`](https://github.com/casey/just); a `Makefile`
mirrors the same targets.

```sh
just build      # compile
just test       # go test ./...
just race       # go test -race ./...
just lint       # golangci-lint
just cover      # coverage profile
just docs-serve # preview the doc site
```

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](./CONTRIBUTING.md) and the
[Code of Conduct](./CODE_OF_CONDUCT.md).

## License

Licensed under the [MIT License](./LICENSE). Copyright (c) 2026 Intentius.
