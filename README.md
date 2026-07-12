# spritzer

A standalone, stateful local emulator of the Fly.io Sprites API. Like LocalStack, but for Fly Sprites.

spritzer is the local target for [chant](https://intentius.io/chant)'s Fly lexicon — its Sprite activities (create, exec, checkpoint, restore) run against spritzer for offline, accountless checkpoint-as-compensation. See the [chant docs](https://intentius.io/chant) and the [Fly deploy tutorial](https://intentius.io/chant/tutorials/fly-deploy-rollback/) it powers.

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
- `exec` is a control WebSocket at `GET /v1/sprites/{id}/exec` speaking the real
  Sprites SDK's framed protocol: each binary message is `[streamID][payload]`
  (StreamStdin=0, StreamStdout=1, StreamStderr=2, StreamExit=3, StreamStdinEOF=4).
  Behind the frames a small scripted interpreter (`echo > path`, `echo`, `cat`,
  `rm`, `true`/`false`, `./risky.sh`, and an echo-back default) writes,
  overwrites, or fails a filesystem key so the result is observable.
- Checkpoint / restore: create is `POST /v1/sprites/{id}/checkpoint` (singular),
  streaming NDJSON progress and assigning a server version id (`v1`, `v2`, …) with
  an optional caller comment; the list is a bare array with `create_time` and
  `is_auto`; restore takes a checkpoint id in the path, streams NDJSON, replaces
  the filesystem with that copy, and returns the sprite to `running`. This is the
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

# Checkpoint the current state. The server assigns the version id and streams
# NDJSON progress; the id is on the terminal complete event.
curl -s -X POST "$BASE/v1/sprites/demo/checkpoint" -d '{"comment":"pre-run"}'
# => {"type":"info","data":"Creating checkpoint..."}
#    {"type":"info","data":"  ID: v1"}
#    {"type":"complete","data":"Checkpoint v1 created successfully"}

# List the checkpoints (creation order) as a bare array.
curl -s "$BASE/v1/sprites/demo/checkpoints"
# => [{"id":"v1","comment":"pre-run","create_time":"2026-07-11T...Z","is_auto":false}]

# Restore rewinds the filesystem to the checkpoint, addressed by id in the path,
# streaming NDJSON progress.
curl -s -X POST "$BASE/v1/sprites/demo/checkpoints/v1/restore"
# => {"type":"info","data":"Restoring checkpoint v1..."}
#    {"type":"complete","data":"Checkpoint v1 restored successfully"}
```

`exec` is a control WebSocket at `ws://<host>/v1/sprites/{id}/exec`. Pass the
command as `cmd` query params (`?cmd=echo&cmd=hi`, or a single `?cmd=echo hi`).
Every message is a binary frame `[streamID][payload]`: the server writes stdout
as `[1]<bytes>`, stderr as `[2]<bytes>`, then `[3]<exitCodeByte>`. So
`echo hi` yields `[1]"hi\n"` then `[3]\x00` (exit 0), and `./risky.sh` yields
`[2]"risky.sh: failed\n"` then `[3]\x01` (exit 1).

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

Implemented: create, the exec control WebSocket, checkpoint (NDJSON), list
checkpoints (bare array), get one checkpoint, restore-by-id (NDJSON), destroy,
and an inspection `GET`, plus a `/_spritzer/health` report. The full table is in
the [API coverage docs](https://intentius.github.io/spritzer/api-coverage/).

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
