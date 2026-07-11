# spritzer

A standalone, stateful local emulator of the Fly.io Sprites API. Like
LocalStack, but for Fly Sprites.

## What it is

spritzer runs a small HTTP server that speaks the Sprites REST surface over the
`/v1` path space and keeps sprites, their filesystems, and their checkpoints in
memory. Point a Sprites client at it by setting `SPRITES_BASE_URL` and your
client talks to spritzer instead of the real service.

## Why it exists

Testing a Sprites client means testing against state. A command run with `exec`
mutates a sprite's filesystem. A checkpoint captures that filesystem under a
label. A restore rewinds to it. This is the checkpoint-as-compensation pattern:
a workflow checkpoints before a risky step and, on failure, restores the label
instead of unwinding with an inverse action. A schema mock has no memory, so it
cannot model any of that. spritzer does.

spritzer is wire-compatible with the in-process Sprites fake in the `chant`
lexicon (`sprites-fake.ts`): the endpoint shapes and the exec interpreter match
it, so the same integration suite passes against the spritzer container image.

## What it models faithfully

- A sprite filesystem that `exec` writes, reads, and deletes.
- Checkpoint (deep copy of the filesystem) and restore (replace with the copy).
- The lifecycle: a sprite is created `running`; destroy makes it `destroyed`,
  after which any operation returns `404`.

See [Fidelity](fidelity.md) for the details and the boundaries of what spritzer
deliberately does not do.

## Next steps

- [Getting started](getting-started.md): run it and point a client at it.
- [API coverage](api-coverage.md): which endpoints are implemented.
- [Contributing](contributing.md): how to work on spritzer.
