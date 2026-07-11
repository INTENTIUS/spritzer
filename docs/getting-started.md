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

# Seed state, checkpoint it, then corrupt it and fail.
curl -s -X POST "$BASE/v1/sprites/demo/exec" -d '{"cmd":"echo good > /state"}'
curl -s -X POST "$BASE/v1/sprites/demo/checkpoints" -d '{"label":"pre"}'
curl -s -X POST "$BASE/v1/sprites/demo/exec" -d '{"cmd":"./risky.sh"}'

# Restore rewinds the filesystem to the checkpoint.
curl -s -X POST "$BASE/v1/sprites/demo/restore" -d '{"checkpoint":"pre"}'
curl -s "$BASE/v1/sprites/demo" | jq '{id, status, fs, checkpoints}'
```

## Check what is implemented

```sh
curl -s http://localhost:4290/_spritzer/health | jq
```

The response reports the running version and the implemented paths.
