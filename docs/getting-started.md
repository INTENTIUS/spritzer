# Getting started

## Run with Docker

```sh
docker run --rm -p 4290:4290 ghcr.io/intentius/spritzer:latest
```

## Run with Go

```sh
go install github.com/intentius/spritzer/cmd/spritzer@latest
spritzer
```

By default spritzer listens on `:4290`. Change it with `-addr` or the
`SPRITZER_ADDR` environment variable:

```sh
spritzer -addr :8080
# or
SPRITZER_ADDR=:8080 spritzer
```

## Point a client at it

Sprites clients read a base-URL override from the environment. Set it to the
address spritzer listens on:

```sh
export SPRITES_BASE_URL=http://localhost:4290
```

All API paths are prefixed with `/v1`, exactly as against the real service.

## A quick tour with curl

```sh
BASE=http://localhost:4290

# Create a sprite (its name is its id).
curl -s -X POST "$BASE/v1/sprites" -d '{"name":"demo"}'

# Checkpoint the current state; the server assigns id v1 and streams NDJSON
# progress ending in {"type":"complete","data":"Checkpoint v1 created successfully"}.
curl -s -X POST "$BASE/v1/sprites/demo/checkpoint" -d '{"comment":"pre-run"}'

# List the checkpoints as a bare array.
curl -s "$BASE/v1/sprites/demo/checkpoints" | jq

# Restore rewinds the filesystem to the checkpoint, by id in the path (NDJSON).
curl -s -X POST "$BASE/v1/sprites/demo/checkpoints/v1/restore"
curl -s "$BASE/v1/sprites/demo" | jq '{id, status, fs, checkpoints}'
```

`exec` is a control WebSocket, so it is not a plain `curl` call. Connect
`ws://localhost:4290/v1/sprites/demo/exec?cmd=<command>` and read the binary
`[streamID][payload]` frames — the server writes stdout as `[1]<bytes>`, stderr
as `[2]<bytes>`, then a final `[3]<exitCodeByte>`. See the
[API coverage](api-coverage.md#the-exec-control-websocket) for the frame
protocol.

## Check what is implemented

```sh
curl -s http://localhost:4290/_spritzer/health | jq
```

The response reports the running version and the implemented paths.
